package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/goburrow/modbus"
)

type Snapshot struct {
	Seq            uint64 `json:"seq"`
	TS             string `json:"ts"`
	Voltage        uint16 `json:"voltage"`         // 40005 (addr 4)
	TargetRPM      uint16 `json:"target_rpm"`      // 40002 (addr 1)
	ActualRPM      uint16 `json:"actual_rpm"`      // 40003 (addr 2)
	WheelsRunning  bool   `json:"wheels_running"`  // coil 16032 (zero-based)
	PaddleRunning  bool   `json:"paddle_running"`  // coil 64 (zero-based)
	WheelDirection string `json:"wheel_direction"` // "fwd" or "rev" from child VFD P0681
}

type CmdPayload struct {
	WheelsRunning  *bool   `json:"wheels_running,omitempty"`
	ChainRunning   *bool   `json:"chain_running,omitempty"`
	WheelDirection *string `json:"wheel_direction,omitempty"` // "fwd" or "rev"
}

type Cmd struct {
	wheels    *bool
	chain     *bool
	direction *string // "fwd" or "rev"
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
	mbAddrChild1 := env("MODBUS_ADDR_CHILD1", "10.0.106.23:502")
	mbAddrChild2 := env("MODBUS_ADDR_CHILD2", "10.0.106.24:502")
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

	// Direction register (P0227) - 0=forward, 1=reverse
	const directionRegister = 227

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

	// Modbus - Main VFD (chain motor + SoftPLC)
	handler := modbus.NewTCPClientHandler(mbAddr)
	handler.Timeout = 5 * time.Second
	handler.SlaveId = 1
	if err := handler.Connect(); err != nil {
		log.Fatalf("Failed to connect to main VFD: %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	// Modbus - Child VFD 1 (wheel inner)
	handlerChild1 := modbus.NewTCPClientHandler(mbAddrChild1)
	handlerChild1.Timeout = 5 * time.Second
	handlerChild1.SlaveId = 1
	if err := handlerChild1.Connect(); err != nil {
		log.Printf("Warning: Failed to connect to child VFD 1 (%s): %v", mbAddrChild1, err)
	} else {
		defer handlerChild1.Close()
	}
	clientChild1 := modbus.NewClient(handlerChild1)

	// Modbus - Child VFD 2 (wheel outer)
	handlerChild2 := modbus.NewTCPClientHandler(mbAddrChild2)
	handlerChild2.Timeout = 5 * time.Second
	handlerChild2.SlaveId = 1
	if err := handlerChild2.Connect(); err != nil {
		log.Printf("Warning: Failed to connect to child VFD 2 (%s): %v", mbAddrChild2, err)
	} else {
		defer handlerChild2.Close()
	}
	clientChild2 := modbus.NewClient(handlerChild2)

	// Channel to serialize command writes (avoid concurrent Modbus I/O)
	cmdCh := make(chan Cmd, 16)

	// Subscribe to commands
	if tok := mc.Subscribe(cmdTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		var p CmdPayload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("cmd: bad json: %v (payload=%q)", err, string(msg.Payload()))
			return
		}
		cmdCh <- Cmd{wheels: p.WheelsRunning, chain: p.ChainRunning, direction: p.WheelDirection}
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

	// writeDirectionToBothWheels writes P0227 to both child VFDs in parallel
	// direction: "fwd" -> 0, "rev" -> 1
	writeDirectionToBothWheels := func(direction string) {
		var val uint16
		if direction == "rev" {
			val = 1
		} else {
			val = 0 // default to forward
		}

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			if _, err := clientChild1.WriteSingleRegister(directionRegister, val); err != nil {
				log.Printf("cmd: write direction to child1 failed: %v", err)
			} else {
				log.Printf("cmd: child1 direction -> %s (P0227=%d)", direction, val)
			}
		}()

		go func() {
			defer wg.Done()
			if _, err := clientChild2.WriteSingleRegister(directionRegister, val); err != nil {
				log.Printf("cmd: write direction to child2 failed: %v", err)
			} else {
				log.Printf("cmd: child2 direction -> %s (P0227=%d)", direction, val)
			}
		}()

		wg.Wait()
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
			if c.direction != nil {
				writeDirectionToBothWheels(*c.direction)
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

		// Read direction state from child VFD 1 (P0681)
		// P0681=4624 means forward, P0681=5648 means reverse (bit 10 set)
		wheelDir := "fwd"
		if raw681, err := clientChild1.ReadHoldingRegisters(681, 1); err == nil && len(raw681) >= 2 {
			p0681 := binary.BigEndian.Uint16(raw681[0:2])
			if (p0681 & 0x0400) != 0 { // bit 10 set = reverse
				wheelDir = "rev"
			}
		}

		snap := Snapshot{
			Seq:            atomic.AddUint64(&seq, 1),
			TS:             time.Now().UTC().Format(time.RFC3339Nano),
			Voltage:        voltage,
			TargetRPM:      targetRPM,
			ActualRPM:      actualRPM,
			WheelsRunning:  wheels,
			PaddleRunning:  paddle,
			WheelDirection: wheelDir,
		}
		b, _ := json.Marshal(snap)
		mc.Publish(stateTopic, 1, false, b) // QoS 1, not retained

		fmt.Printf("pub seq=%d tgt=%d rpm=%d volt=%d wheels=%t paddle=%t dir=%s\n",
			snap.Seq, targetRPM, actualRPM, voltage, wheels, paddle, wheelDir)

		time.Sleep(pollInterval)
	}
}
