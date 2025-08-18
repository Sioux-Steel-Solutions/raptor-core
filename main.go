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
	Seq          uint64 `json:"seq"`
	TS           string `json:"ts"`
	Voltage      uint16 `json:"voltage"`       // 40005 (addr 4)
	TargetRPM    uint16 `json:"target_rpm"`    // 40002 (addr 1)
	ActualRPM    uint16 `json:"actual_rpm"`    // 40003 (addr 2)
	WheelsRunning bool  `json:"wheels_running"` // coil 16032 (zero-based)
	PaddleRunning bool  `json:"paddle_running"` // coil 64 (zero-based)
}

func env(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }

func main() {
	site   := env("RAPTOR_SITE", "shop")
	device := env("RAPTOR_DEVICE", "revpi-135593")
	mbAddr := env("MODBUS_ADDR", "10.0.106.100:502")
	mqttURL := env("MQTT_URL", "tcp://10.0.106.22:1883")
	mqUser  := os.Getenv("MQTT_USER")
	mqPass  := os.Getenv("MQTT_PASS")

	stateTopic := fmt.Sprintf("raptor/%s/%s/state", site, device)

	// MQTT
	opts := mqtt.NewClientOptions().
		AddBroker(mqttURL).
		SetClientID("raptor-core-"+device).
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

	pollInterval := 2 * time.Second
	var seq uint64

	for {
		// Holding regs block: 40002..40005 (addr 1, count 4)
		raw, err := client.ReadHoldingRegisters(1, 4)
		if err != nil || len(raw) < 8 {
			if err != nil { log.Printf("block read failed: %v", err) } else { log.Printf("unexpected block length: %d", len(raw)) }
			time.Sleep(pollInterval)
			continue
		}
		targetRPM := binary.BigEndian.Uint16(raw[0:2]) // 40002
		actualRPM := binary.BigEndian.Uint16(raw[2:4]) // 40003
		voltage   := binary.BigEndian.Uint16(raw[6:8]) // 40005

		// Coils: NOTE goburrow uses ZERO-BASED addresses (same as your `mbpoll -0` tests)
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

