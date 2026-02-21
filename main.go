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

// VFDTelemetry holds per-VFD metrics read from Modbus
// Register mapping (goburrow 0-based addressing):
//   P0002 (target RPM)  = goburrow addr 1
//   P0003 (actual RPM)  = goburrow addr 2
//   P0005 (voltage)     = goburrow addr 4
//   P0006 (amps x10)    = goburrow addr 5
//   P0007 (drive state) = goburrow addr 6
type VFDTelemetry struct {
	TargetRPM  uint16  `json:"target_rpm"`
	ActualRPM  uint16  `json:"actual_rpm"`
	Voltage    uint16  `json:"voltage"`
	Amps       float32 `json:"amps"`        // P0006 divided by 10
	DriveState uint16  `json:"drive_state"` // 0=stopped, 1=running
}

type Snapshot struct {
	Seq uint64 `json:"seq"`
	TS  string `json:"ts"`

	// Per-VFD telemetry (NEW)
	Chain      VFDTelemetry `json:"chain"`       // Main VFD (.22) - chain/paddle motor
	InnerWheel VFDTelemetry `json:"inner_wheel"` // Child1 (.23) - inner wheel motor
	OuterWheel VFDTelemetry `json:"outer_wheel"` // Child2 (.24) - outer wheel motor

	// Backwards compatibility - keep flat fields from main VFD
	Voltage   uint16 `json:"voltage"`    // 40005 (addr 4) - from main VFD
	TargetRPM uint16 `json:"target_rpm"` // 40002 (addr 1) - from main VFD
	ActualRPM uint16 `json:"actual_rpm"` // 40003 (addr 2) - from main VFD

	// Control state
	WheelsRunning    bool   `json:"wheels_running"`    // coil 16032 (zero-based)
	PaddleRunning    bool   `json:"paddle_running"`    // coil 64 (zero-based)
	WheelDirection   string `json:"wheel_direction"`   // "fwd" or "rev"
	WheelSpeed       uint16 `json:"wheel_speed"`       // P0122 speed reference (inner wheel)
	ChainSpeed       uint16 `json:"chain_speed"`       // P0122 speed reference (chain motor)
	DirectionWarning string `json:"direction_warning"` // empty if OK, warning message if mismatch possible
}

type CmdPayload struct {
	WheelsRunning  *bool   `json:"wheels_running,omitempty"`
	ChainRunning   *bool   `json:"chain_running,omitempty"`
	WheelDirection *string `json:"wheel_direction,omitempty"` // "fwd" or "rev"
	WheelSpeed     *uint16 `json:"wheel_speed,omitempty"`     // P0122 speed reference (100-1200)
	ChainSpeed     *uint16 `json:"chain_speed,omitempty"`     // P0122 speed reference for chain (100-600)
}

type Cmd struct {
	wheels     *bool
	chain      *bool
	direction  *string // "fwd" or "rev"
	speed      *uint16 // P0122 wheel speed reference
	chainSpeed *uint16 // P0122 chain speed reference
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
	mbAddr := env("MODBUS_ADDR", "192.168.1.152:502")        // Chain drive - 1200 RPM
	mbAddrChild1 := env("MODBUS_ADDR_CHILD1", "192.168.1.153:502") // Inner wheel - 1500 RPM
	mbAddrChild2 := env("MODBUS_ADDR_CHILD2", "192.168.1.151:502") // Outer wheel - 1800 RPM
	mqttURL := env("MQTT_URL", "tcp://192.168.1.154:1883")
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
	// CRITICAL: goburrow uses 0-based addressing, so P0227 = address 226
	const directionRegister = 226

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

	// Note: Child VFD connections (.23 and .24) are created fresh for each write
	// to avoid stale connection issues. See writeDirectionToBothWheels and writeSpeedToBothWheels.

	// Channel to serialize command writes (avoid concurrent Modbus I/O)
	cmdCh := make(chan Cmd, 16)

	// Subscribe to commands
	if tok := mc.Subscribe(cmdTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		var p CmdPayload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("cmd: bad json: %v (payload=%q)", err, string(msg.Payload()))
			return
		}
		cmdCh <- Cmd{wheels: p.WheelsRunning, chain: p.ChainRunning, direction: p.WheelDirection, speed: p.WheelSpeed, chainSpeed: p.ChainSpeed}
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

	// Track direction warning (set if writes to both VFDs didn't succeed)
	var directionWarning atomic.Value
	directionWarning.Store("") // empty = no warning

	// Track wheel speed locally
	// Speed is written to P0122 on both child VFDs
	// Inner wheel (.23) gets the base speed, outer wheel (.24) gets speed * ratio
	// Ratio: 550/600 = 0.9167 (outer wheel runs slightly slower)
	var currentSpeed atomic.Uint64
	currentSpeed.Store(600) // default speed

	const speedRegister = 121             // P0122 - speed reference (0-based: 122-1=121)
	const outerWheelRatio = 0.9167        // .24 runs at 91.67% of .23's speed
	const minSpeed uint16 = 100           // minimum allowed speed
	const maxSpeed uint16 = 1500          // maximum allowed speed

	// Track chain speed locally
	// Speed is written to P0122 on main VFD (.22)
	var currentChainSpeed atomic.Uint64
	currentChainSpeed.Store(420) // default chain speed (matches current setting)

	const minChainSpeed uint16 = 100  // minimum chain speed
	const maxChainSpeed uint16 = 1200 // maximum chain speed

	// Direction control via Modbus - WORKING as of 2025-01-23
	// Requires VFD panel setting: R2 -> Direction -> "HMI DR Key"
	// See docs/VFD-CONTROL-SESSION-NOTES.md for full documentation
	//
	// IMPORTANT: Both wheels must change direction at the same time.
	// We use fresh connections and track success of both writes.
	// If one fails, we set a warning so the UI can alert the user.

	writeDirectionToBothWheels := func(direction string) {
		var val uint16
		if direction == "rev" {
			val = 1
		} else {
			val = 0 // default to forward
			direction = "fwd" // normalize
		}

		log.Printf("cmd: setting wheel direction to %s (P0227=%d)", direction, val)

		var wg sync.WaitGroup
		var err1, err2 error
		wg.Add(2)

		// Write to both VFDs in parallel using fresh connections
		go func() {
			defer wg.Done()
			h := modbus.NewTCPClientHandler(mbAddrChild1)
			h.Timeout = 5 * time.Second
			h.SlaveId = 1
			if err := h.Connect(); err != nil {
				err1 = fmt.Errorf("connect child1: %v", err)
				return
			}
			defer h.Close()
			c := modbus.NewClient(h)
			if _, err := c.WriteSingleRegister(directionRegister, val); err != nil {
				err1 = fmt.Errorf("write child1: %v", err)
				return
			}
			log.Printf("cmd: child1 (.23) direction -> %s", direction)
		}()

		go func() {
			defer wg.Done()
			h := modbus.NewTCPClientHandler(mbAddrChild2)
			h.Timeout = 5 * time.Second
			h.SlaveId = 1
			if err := h.Connect(); err != nil {
				err2 = fmt.Errorf("connect child2: %v", err)
				return
			}
			defer h.Close()
			c := modbus.NewClient(h)
			if _, err := c.WriteSingleRegister(directionRegister, val); err != nil {
				err2 = fmt.Errorf("write child2: %v", err)
				return
			}
			log.Printf("cmd: child2 (.24) direction -> %s", direction)
		}()

		wg.Wait()

		// Check results - both must succeed for safe operation
		if err1 != nil && err2 != nil {
			// Both failed - don't update state
			log.Printf("cmd: DIRECTION FAILED - both VFDs failed: %v, %v", err1, err2)
			directionWarning.Store("Direction change failed on both wheels")
			return
		} else if err1 != nil {
			// Only child1 failed - DANGEROUS, wheels may be going different directions!
			log.Printf("cmd: WARNING - child1 failed but child2 succeeded: %v", err1)
			directionWarning.Store("WARNING: Inner wheel (.23) direction change failed - wheels may be mismatched!")
			// Still update state to what child2 is doing
			currentDirection.Store(direction)
			return
		} else if err2 != nil {
			// Only child2 failed - DANGEROUS, wheels may be going different directions!
			log.Printf("cmd: WARNING - child2 failed but child1 succeeded: %v", err2)
			directionWarning.Store("WARNING: Outer wheel (.24) direction change failed - wheels may be mismatched!")
			// Still update state to what child1 is doing
			currentDirection.Store(direction)
			return
		}

		// Both succeeded!
		log.Printf("cmd: direction set to %s on both wheels", direction)
		currentDirection.Store(direction)
		directionWarning.Store("") // clear any previous warning
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

	// Chain speed control - writes P0122 to main VFD (.22)
	// Same register as wheel speed, but on a different VFD
	writeChainSpeed := func(speed uint16) {
		// Clamp speed to valid range
		if speed < minChainSpeed {
			speed = minChainSpeed
			log.Printf("cmd: chain speed clamped to minimum %d", minChainSpeed)
		}
		if speed > maxChainSpeed {
			speed = maxChainSpeed
			log.Printf("cmd: chain speed clamped to maximum %d", maxChainSpeed)
		}

		log.Printf("cmd: setting chain speed to %d (P0122)", speed)

		// Write to main VFD using the persistent connection
		// P0122 = goburrow address 121 (122-1)
		if _, err := client.WriteSingleRegister(speedRegister, speed); err != nil {
			log.Printf("cmd: write chain speed failed: %v", err)
			return
		}

		currentChainSpeed.Store(uint64(speed))
		log.Printf("cmd: chain speed set to %d", speed)
	}

	// readVFDTelemetry reads telemetry registers from a single VFD using a fresh connection
	// Returns VFDTelemetry struct with amps already scaled (divided by 10)
	// Register mapping (goburrow 0-based):
	//   addr 1 = P0002 = Target RPM
	//   addr 2 = P0003 = Actual RPM
	//   addr 3 = P0004 = (skipped)
	//   addr 4 = P0005 = Voltage
	//   addr 5 = P0006 = Amps x10
	//   addr 6 = P0007 = Drive State
	readVFDTelemetry := func(addr string) (VFDTelemetry, error) {
		h := modbus.NewTCPClientHandler(addr)
		h.Timeout = 3 * time.Second
		h.SlaveId = 1
		if err := h.Connect(); err != nil {
			return VFDTelemetry{}, fmt.Errorf("connect %s: %v", addr, err)
		}
		defer h.Close()

		c := modbus.NewClient(h)

		// Read P0002-P0007 (goburrow addresses 1-6, count=6)
		raw, err := c.ReadHoldingRegisters(1, 6)
		if err != nil {
			return VFDTelemetry{}, fmt.Errorf("read %s: %v", addr, err)
		}

		if len(raw) < 12 {
			return VFDTelemetry{}, fmt.Errorf("short read from %s: got %d bytes, need 12", addr, len(raw))
		}

		// Parse registers (big-endian uint16)
		// raw[0:2]  = P0002 (goburrow addr 1) = Target RPM
		// raw[2:4]  = P0003 (goburrow addr 2) = Actual RPM
		// raw[4:6]  = P0004 (goburrow addr 3) = skipped
		// raw[6:8]  = P0005 (goburrow addr 4) = Voltage
		// raw[8:10] = P0006 (goburrow addr 5) = Amps x10
		// raw[10:12]= P0007 (goburrow addr 6) = Drive State
		targetRPM := binary.BigEndian.Uint16(raw[0:2])
		actualRPM := binary.BigEndian.Uint16(raw[2:4])
		voltage := binary.BigEndian.Uint16(raw[6:8])
		ampsRaw := binary.BigEndian.Uint16(raw[8:10])
		driveState := binary.BigEndian.Uint16(raw[10:12])

		return VFDTelemetry{
			TargetRPM:  targetRPM,
			ActualRPM:  actualRPM,
			Voltage:    voltage,
			Amps:       float32(ampsRaw) / 200.0, // Scale: divide by 200 (P0006 is % of rated current)
			DriveState: driveState,
		}, nil
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
			if c.chainSpeed != nil {
				writeChainSpeed(*c.chainSpeed)
			}
		}
	}()

	// State publisher loop
	pollInterval := 2 * time.Second
	var seq uint64

	for {
		// Read telemetry from all 3 VFDs in parallel
		var chainTelem, innerTelem, outerTelem VFDTelemetry
		var errChain, errInner, errOuter error
		var wgRead sync.WaitGroup
		wgRead.Add(3)

		go func() {
			defer wgRead.Done()
			chainTelem, errChain = readVFDTelemetry(mbAddr)
		}()
		go func() {
			defer wgRead.Done()
			innerTelem, errInner = readVFDTelemetry(mbAddrChild1)
		}()
		go func() {
			defer wgRead.Done()
			outerTelem, errOuter = readVFDTelemetry(mbAddrChild2)
		}()
		wgRead.Wait()

		// Log any read errors (but continue with zero values)
		if errChain != nil {
			log.Printf("read chain VFD failed: %v", errChain)
		}
		if errInner != nil {
			log.Printf("read inner wheel VFD failed: %v", errInner)
		}
		if errOuter != nil {
			log.Printf("read outer wheel VFD failed: %v", errOuter)
		}

		// Coils (zero-based addressing with goburrow) - read from main VFD
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

		// Get direction, speed, and warning from local tracking (don't poll child VFDs - causes auto-switch issue)
		wheelDir := currentDirection.Load().(string)
		wheelSpd := uint16(currentSpeed.Load())
		chainSpd := uint16(currentChainSpeed.Load())
		dirWarn := directionWarning.Load().(string)

		snap := Snapshot{
			Seq: atomic.AddUint64(&seq, 1),
			TS:  time.Now().UTC().Format(time.RFC3339Nano),

			// Per-VFD telemetry
			Chain:      chainTelem,
			InnerWheel: innerTelem,
			OuterWheel: outerTelem,

			// Backwards compat - flat fields from main VFD (chain)
			Voltage:   chainTelem.Voltage,
			TargetRPM: chainTelem.TargetRPM,
			ActualRPM: chainTelem.ActualRPM,

			// Control state
			WheelsRunning:    wheels,
			PaddleRunning:    paddle,
			WheelDirection:   wheelDir,
			WheelSpeed:       wheelSpd,
			ChainSpeed:       chainSpd,
			DirectionWarning: dirWarn,
		}
		b, _ := json.Marshal(snap)
		mc.Publish(stateTopic, 1, false, b) // QoS 1, not retained

		fmt.Printf("pub seq=%d chain[rpm=%d,A=%.1f] inner[rpm=%d,A=%.1f] outer[rpm=%d,A=%.1f] volt=%d wheels=%t paddle=%t\n",
			snap.Seq,
			chainTelem.ActualRPM, chainTelem.Amps,
			innerTelem.ActualRPM, innerTelem.Amps,
			outerTelem.ActualRPM, outerTelem.Amps,
			chainTelem.Voltage, wheels, paddle)

		time.Sleep(pollInterval)
	}
}
