package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/goburrow/modbus"
)

type Snapshot struct {
	Seq           uint64 `json:"seq"`
	TS            string `json:"ts"`
	Voltage       uint16 `json:"voltage"`        // 40005 (addr 4)
	TargetRPM     uint16 `json:"target_rpm"`     // 40002 (addr 1)
	ActualRPM     uint16 `json:"actual_rpm"`     // 40003 (addr 2)
	WheelsRunning bool   `json:"wheels_running"` // coil 16032 (zero-based)
	PaddleRunning bool   `json:"paddle_running"` // coil 64 (zero-based)
}

type CmdPayload struct {
	WheelsRunning *bool `json:"wheels_running,omitempty"`
	ChainRunning  *bool `json:"chain_running,omitempty"`
}

type Cmd struct {
	wheels *bool
	chain  *bool
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	site := env("RAPTOR_SITE", "shop")
	device := env("RAPTOR_DEVICE", "revpi-135593")
	mbAddr := env("MODBUS_ADDR", "10.0.106.22:502")
	mqttURL := env("MQTT_URL", "tcp://10.0.106.26:1883")
	mqUser := os.Getenv("MQTT_USER")
	mqPass := os.Getenv("MQTT_PASS")

	stateTopic := fmt.Sprintf("raptor/%s/%s/state", site, device)
	cmdTopic := fmt.Sprintf("raptor/%s/%s/cmd", site, device)

	// Coils to write for commands (as used in your mbpoll examples, 1-based)
	// Wheels: mbpoll ... -r 50066
	// Chain : mbpoll ... -r 50067
	const wheelsCoilOneBased = 50066
	const chainCoilOneBased = 50067

	// MQTT
	opts := mqtt.NewClientOptions().
		AddBroker(mqttURL).
		SetClientID("raptor-core-" + device).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetOrderMatters(false)
	if mqUser != "" {
		opts.SetUsername(mqUser)
		opts.SetPassword(mqPass)
	}
	mc := mqtt.NewClient(opts)
	if tok := mc.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		log.Fatalf("mqtt connect: %v", tok.Error())
	}
	defer mc.Disconnect(250)

	// Modbus
	handler := modbus.NewTCPClientHandler(mbAddr)
	handler.Timeout = 5 * time.Second
	handler.SlaveId = 1
	if err := handler.Connect(); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	// Channel to serialize command writes (avoid concurrent Modbus I/O)
	cmdCh := make(chan Cmd, 16)

	// Subscribe to commands
	if tok := mc.Subscribe(cmdTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		var p CmdPayload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("cmd: bad json: %v (payload=%q)", err, string(msg.Payload()))
			return
		}
		cmdCh <- Cmd{wheels: p.WheelsRunning, chain: p.ChainRunning}
	}); !tok.WaitTimeout(5 * time.Second) || tok.Error() != nil {
		log.Fatalf("mqtt subscribe %s: %v", cmdTopic, tok.Error())
	}
	log.Printf("Subscribed to %s", cmdTopic)

	// Helpers
	writeCoil := func(addrOneBased int, on bool) error {
		// goburrow expects zero-based coil address
		addrZero := uint16(addrOneBased - 1)
		var val uint16
		if on {
			val = 0xFF00
		} else {
			val = 0x0000
		}
		_, err := client.WriteSingleCoil(addrZero, val)
		return err
	}

	// Background goroutine: process command writes as they come in
	go func() {
		for c := range cmdCh {
			if c.wheels != nil {
				if err := writeCoil(wheelsCoilOneBased, *c.wheels); err != nil {
					log.Printf("cmd: write wheels coil failed: %v", err)
				} else {
					log.Printf("cmd: wheels_running -> %v", *c.wheels)
				}
			}
			if c.chain != nil {
				if err := writeCoil(chainCoilOneBased, *c.chain); err != nil {
					log.Printf("cmd: write chain coil failed: %v", err)
				} else {
					log.Printf("cmd: chain_running -> %v", *c.chain)
				}
			}
		}
	}()

	// State publisher loop (unchanged)
	pollInterval := 2 * time.Second
	var seq uint64

	for {
		// Holding regs block: 40002..40005 (addr 1, count 4)
		raw, err := client.ReadHoldingRegisters(1, 4)
		if err != nil || len(raw) < 8 {
			if err != nil {
				log.Printf("block read failed: %v", err)
			} else {
				log.Printf("unexpected block length: %d", len(raw))
			}
			time.Sleep(pollInterval)
			continue
		}
		targetRPM := binary.BigEndian.Uint16(raw[0:2]) // 40002
		actualRPM := binary.BigEndian.Uint16(raw[2:4]) // 40003
		voltage := binary.BigEndian.Uint16(raw[6:8])   // 40005

		// Coils (zero-based addressing with goburrow)
		var wheels, paddle bool

		if buf, err := client.ReadCoils(16032, 1); err == nil && len(buf) >= 1 {
			wheels = (buf[0] & 0x01) != 0
		} else if err != nil {
			log.Printf("read wheels coil failed: %v", err)
		}

		if buf, err := client.ReadCoils(64, 1); err == nil && len(buf) >= 1 {
			paddle = (buf[0] & 0x01) != 0
		} else if err != nil {
			log.Printf("read paddle coil failed: %v", err)
		}

		snap := Snapshot{
			Seq:           atomic.AddUint64(&seq, 1),
			TS:            time.Now().UTC().Format(time.RFC3339Nano),
			Voltage:       voltage,
			TargetRPM:     targetRPM,
			ActualRPM:     actualRPM,
			WheelsRunning: wheels,
			PaddleRunning: paddle,
		}
		b, _ := json.Marshal(snap)
		mc.Publish(stateTopic, 1, false, b) // QoS 1, not retained

		fmt.Printf("pub seq=%d tgt=%d rpm=%d volt=%d wheels=%t paddle=%t\n",
			snap.Seq, targetRPM, actualRPM, voltage, wheels, paddle)

		time.Sleep(pollInterval)
	}
}
