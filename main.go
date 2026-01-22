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
	WheelSpeed     uint16 `json:"wheel_speed"`     // P0122 speed reference (inner wheel)
}

type CmdPayload struct {
	WheelsRunning  *bool   `json:"wheels_running,omitempty"`
	ChainRunning   *bool   `json:"chain_running,omitempty"`
	WheelDirection *string `json:"wheel_direction,omitempty"` // "fwd" or "rev"
	WheelSpeed     *uint16 `json:"wheel_speed,omitempty"`     // P0122 speed reference (100-1200)
}

type Cmd struct {
	wheels    *bool
	chain     *bool
	direction *string // "fwd" or "rev"
	speed     *uint16 // P0122 speed reference
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
		cmdCh <- Cmd{wheels: p.WheelsRunning, chain: p.ChainRunning, direction: p.WheelDirection, speed: p.WheelSpeed}
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

	// Track direction locally (don't poll child VFDs - it causes auto-switch to serial mode)
	var currentDirection atomic.Value
	currentDirection.Store("fwd") // default to forward

	// Track wheel speed locally
	// Speed is written to P0122 on both child VFDs
	// Inner wheel (.23) gets the base speed, outer wheel (.24) gets speed * ratio
	// Ratio: 550/600 = 0.9167 (outer wheel runs slightly slower)
	var currentSpeed atomic.Uint64
	currentSpeed.Store(600) // default speed

	const speedRegister = 121             // P0122 - speed reference (0-based: 122-1=121)
	const outerWheelRatio = 0.9167        // .24 runs at 91.67% of .23's speed
	const minSpeed uint16 = 100           // minimum allowed speed
	const maxSpeed uint16 = 1200          // maximum allowed speed

	// DANGER ZONE - Direction control via Modbus
	// WARNING: Writing to child VFDs can cause CONFIG mode or auto-switch issues!
	// See docs/VFD-CONFIG-MODE-RECOVERY.md for recovery procedures.
	//
	// DO NOT write P0221 (command source) - it will put VFDs in CONFIG mode!
	// Writing P0227 (direction) alone may still cause auto-switch to serial mode,
	// which breaks physical switch control. Use with caution.
	//
	// Direction control is DISABLED by default. Set ENABLE_DIRECTION_CONTROL=true to enable.
	directionControlEnabled := os.Getenv("ENABLE_DIRECTION_CONTROL") == "true"

	writeDirectionToBothWheels := func(direction string) {
		if !directionControlEnabled {
			log.Printf("cmd: direction control DISABLED (set ENABLE_DIRECTION_CONTROL=true to enable)")
			log.Printf("cmd: direction change requested: %s (ignored)", direction)
			// Still update local tracking so UI stays in sync
			currentDirection.Store(direction)
			return
		}

		var val uint16
		if direction == "rev" {
			val = 1
		} else {
			val = 0 // default to forward
		}

		log.Printf("WARNING: Writing direction to child VFDs - this may cause auto-switch issues!")

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			if _, err := clientChild1.WriteSingleRegister(directionRegister, val); err != nil {
				log.Printf("cmd: write direction to child1 failed: %v", err)
				return
			}
			log.Printf("cmd: child1 direction -> %s (P0227=%d)", direction, val)
		}()

		go func() {
			defer wg.Done()
			if _, err := clientChild2.WriteSingleRegister(directionRegister, val); err != nil {
				log.Printf("cmd: write direction to child2 failed: %v", err)
				return
			}
			log.Printf("cmd: child2 direction -> %s (P0227=%d)", direction, val)
		}()

		wg.Wait()
		currentDirection.Store(direction)
	}

	// Speed control - writes P0122 to both child VFDs with proper ratio
	// This is SAFE - P0122 is a proven writable parameter that doesn't cause CONFIG mode
	// Note: We create fresh connections for each write to avoid stale connection issues
	writeSpeedToBothWheels := func(speed uint16) {
		// Clamp speed to valid range
		if speed < minSpeed {
			speed = minSpeed
			log.Printf("cmd: speed clamped to minimum %d", minSpeed)
		}
		if speed > maxSpeed {
			speed = maxSpeed
			log.Printf("cmd: speed clamped to maximum %d", maxSpeed)
		}

		// Calculate outer wheel speed with ratio
		outerSpeed := uint16(float64(speed) * outerWheelRatio)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			// Create fresh connection for child1
			h := modbus.NewTCPClientHandler(mbAddrChild1)
			h.Timeout = 5 * time.Second
			h.SlaveId = 1
			if err := h.Connect(); err != nil {
				log.Printf("cmd: connect to child1 (.23) failed: %v", err)
				return
			}
			defer h.Close()
			c := modbus.NewClient(h)
			if _, err := c.WriteSingleRegister(speedRegister, speed); err != nil {
				log.Printf("cmd: write speed to child1 (.23) failed: %v", err)
				return
			}
			log.Printf("cmd: child1 (.23 inner) speed -> %d (P0122)", speed)
		}()

		go func() {
			defer wg.Done()
			// Create fresh connection for child2
			h := modbus.NewTCPClientHandler(mbAddrChild2)
			h.Timeout = 5 * time.Second
			h.SlaveId = 1
			if err := h.Connect(); err != nil {
				log.Printf("cmd: connect to child2 (.24) failed: %v", err)
				return
			}
			defer h.Close()
			c := modbus.NewClient(h)
			if _, err := c.WriteSingleRegister(speedRegister, outerSpeed); err != nil {
				log.Printf("cmd: write speed to child2 (.24) failed: %v", err)
				return
			}
			log.Printf("cmd: child2 (.24 outer) speed -> %d (P0122, ratio=%.4f)", outerSpeed, outerWheelRatio)
		}()

		wg.Wait()
		currentSpeed.Store(uint64(speed))
		log.Printf("cmd: wheel speed set to %d (inner) / %d (outer)", speed, outerSpeed)
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
			if c.speed != nil {
				writeSpeedToBothWheels(*c.speed)
			}
		}
	}()

	// State publisher loop
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

		// Get direction and speed from local tracking (don't poll child VFDs - causes auto-switch issue)
		wheelDir := currentDirection.Load().(string)
		wheelSpd := uint16(currentSpeed.Load())

		snap := Snapshot{
			Seq:            atomic.AddUint64(&seq, 1),
			TS:             time.Now().UTC().Format(time.RFC3339Nano),
			Voltage:        voltage,
			TargetRPM:      targetRPM,
			ActualRPM:      actualRPM,
			WheelsRunning:  wheels,
			PaddleRunning:  paddle,
			WheelDirection: wheelDir,
			WheelSpeed:     wheelSpd,
		}
		b, _ := json.Marshal(snap)
		mc.Publish(stateTopic, 1, false, b) // QoS 1, not retained

		fmt.Printf("pub seq=%d tgt=%d rpm=%d volt=%d wheels=%t paddle=%t dir=%s spd=%d\n",
			snap.Seq, targetRPM, actualRPM, voltage, wheels, paddle, wheelDir, wheelSpd)

		time.Sleep(pollInterval)
	}
}
