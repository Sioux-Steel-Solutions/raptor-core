# VFD Control Session Notes - 2025-01-22

## LATEST DISCOVERY (Read This First!)

**P0226 and P0227 are SOURCE SELECTION parameters, not direction values!**

| Parameter | Actual Purpose | What We Thought |
|-----------|----------------|-----------------|
| P0226 | Direction SOURCE (where direction command comes from) | Unknown |
| P0227 | Run/Stop SOURCE (where run/stop command comes from) | Direction value |

### The Real Solution

Configure child VFDs with **split source control**:
- **P0227 = 1** → Run/Stop from DIx (physical switches stay in control)
- **P0226 = 3 or 7** → Direction from Serial/Fieldbus (Modbus controls direction)

Then write the actual direction VALUE to a **fieldbus control word register** (not P0227).

### Why We Had Problems

When we wrote `P0227 = 1` thinking it meant "reverse", we may have been changing the run/stop SOURCE to DIx. When we wrote `P0227 = 0` for "forward", we may have changed run/stop source to something else entirely.

### Next Steps

1. ~~Read current P0226 values from all VFDs~~ DONE - all are 1
2. Set P0226 = 3 (Serial) or 7 (Fieldbus) for direction source
3. Keep P0227 = 1 for run/stop from DI
4. Find the correct fieldbus control word register for direction value
5. Test!

### THE FIX FOR DIRECTION CONTROL (DO THIS NEXT TIME)

**Root cause found:** We used wrong Modbus address!

```go
// WRONG - what we had:
const directionRegister = 227  // This hit P0228, not P0227!

// CORRECT - use this:
const directionRegister = 226  // P0227 in 0-based (227-1=226)
```

goburrow library uses **0-based** addressing. mbpoll uses **1-based**.

| Register | mbpoll (-r) | goburrow (address) |
|----------|-------------|-------------------|
| P0122 (speed) | 122 | 121 |
| P0227 (direction) | 227 | **226** |

**This is why direction caused auto-switch problems** - we were accidentally writing to P0228 (some control source param), not P0227 (direction).

See sources:
- [WEG CFW500 Programming Manual](https://static.weg.net/medias/downloadcenter/h69/h0f/WEG-CFW500-programming-manual-10002296099-en.pdf)
- [WEG CFW700 Programming Manual](https://static.weg.net/medias/downloadcenter/hc1/h74/WEG-CFW700-pogramming-manual-10001006882-en.pdf)

---

## Summary

This session we implemented wheel speed control and attempted direction control. Speed control works. Direction control causes issues with physical switch operation.

---

## What Works

### Speed Control (P0122)
- **Register**: P0122 (Modbus address 121, zero-based)
- **Range**: 100-1200
- **Ratio**: Inner wheel (.23) gets base speed, outer wheel (.24) gets speed × 0.9167
- **Status**: WORKING - does NOT trigger auto-switch to serial mode

Commands via MQTT:
```json
{"wheel_speed": 600}
```

Manual test via mbpoll:
```bash
# Set inner wheel (.23) to 600
mbpoll -a 1 -r 122 -t 4 10.0.106.23 -- 600

# Set outer wheel (.24) to 550
mbpoll -a 1 -r 122 -t 4 10.0.106.24 -- 550
```

### Run/Stop Control (Coils)
- **Wheels coil**: 50066 (1-based)
- **Chain coil**: 50067 (1-based)
- **Status**: WORKING via main VFD (.22) SoftPLC

---

## What Doesn't Work (Yet)

### Direction Control (P0227)
- **Register**: P0227 (direction: 0=forward, 1=reverse)
- **Problem**: Writing to P0227 triggers VFD auto-switch from "Run/Stop DI" to "Serial" mode
- **Result**: Physical switches stop working after direction change

#### What We Tried:
1. Write P0227 alone → triggers auto-switch
2. Write P0227 then P0221=2 (Terminal) → caused CONFIG mode crisis
3. Disabled direction control in raptor-core for safety

#### Why It Happens:
The CFW900 has an auto-switch feature. When it receives Modbus writes to control registers, it assumes you want full Modbus control and switches command source away from DI (physical switches).

P0122 (speed reference) doesn't trigger this because it's just a setpoint value.
P0227 (direction) is part of the command/control system, so it triggers auto-switch.

---

## The CONFIG Mode Crisis

### What Happened
We tried to write P0221=2 (Terminal command source) to force the VFD back to DI mode after direction changes. This created an invalid configuration state and both child VFDs got stuck in "CONFIG" mode.

### How We Fixed It
Compared parameters from working VFD (.22) to broken ones (.23/.24) and copied the values:

```bash
# Recovery commands (run from Pi)
mbpoll -a 1 -r 211 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 219 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 222 -t 4 10.0.106.23 -- 7
mbpoll -a 1 -r 223 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 225 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 228 -t 4 10.0.106.23 -- 4

# Repeat for .24
```

After recovery, manually set R2 profile → Run/Stop to "Run/Stop DI" on each child VFD panel.

See: `docs/VFD-CONFIG-MODE-RECOVERY.md` for full details.

---

## VFD Network Layout

| VFD | IP Address | Role | Notes |
|-----|------------|------|-------|
| Main | 10.0.106.22 | Chain motor + SoftPLC | Controls coils for wheels/chain |
| Child 1 | 10.0.106.23 | Inner wheel | Speed via P0122 |
| Child 2 | 10.0.106.24 | Outer wheel | Speed via P0122 × 0.9167 ratio |

---

## Key Parameters

### Safe to Write
| Register | Parameter | Description | Notes |
|----------|-----------|-------------|-------|
| P0122 | Speed reference | 100-1200 range | Modbus addr 121 (0-based) |

### DANGEROUS - Do Not Write
| Register | Parameter | Why Dangerous |
|----------|-----------|---------------|
| P0221 | Command source | Causes CONFIG mode if dependencies wrong |
| P0227 | Direction | Triggers auto-switch to serial mode |
| P0219-P0228 | Control sources | Can create invalid config states |

---

## Next Steps for Direction Control

### Option 1: Find Auto-Switch Disable Parameter
Search CFW900 manual for parameter that disables auto-switch behavior when receiving Modbus commands.

### Option 2: Configure Split Control Sources
Configure VFD so:
- Run/Stop → DI (physical switches)
- Direction → Fieldbus (Modbus)

Relevant parameters to investigate:
- P0220 - Local/Remote selection
- P0225 - Direction source selection
- P0226 - Run/Stop source selection

### Option 3: HMI DR Key Method
Earlier we found P0227 works with "HMI DR Key" mode. Need to understand how to configure this properly without triggering auto-switch.

### Option 4: Accept Limitation
Keep direction as manual-only (VFD panel), control only speed via Modbus.

---

## Code Changes Made

### raptor-core/main.go
1. Added wheel speed control (P0122)
2. Fresh Modbus connections for each write (avoids stale connection issues)
3. Proper 0-based addressing (register 122 → address 121)
4. Direction control disabled by default (ENABLE_DIRECTION_CONTROL=true to enable)
5. Speed included in MQTT state messages

### raptor-frontend
1. Added wheel speed slider (100-1200 range)
2. Shows inner/outer speeds with ratio preview
3. Publishes speed commands via MQTT

---

## Files Modified

- `/Users/kalebtringale/raptor-core/main.go` - Speed control, connection handling
- `/Users/kalebtringale/raptor-core/docs/VFD-CONFIG-MODE-RECOVERY.md` - Recovery procedures
- `/Users/kalebtringale/raptor-frontend/src/app/auger/[id]/auger-detail-client.tsx` - Speed slider UI

---

## Commands Reference

### Check VFD Parameters
```bash
# Read P0122 (speed) from both child VFDs
mbpoll -a 1 -r 122 -t 4 -c 1 -1 10.0.106.23
mbpoll -a 1 -r 122 -t 4 -c 1 -1 10.0.106.24

# Read config block P0200-P0230
mbpoll -a 1 -r 200 -t 4 -c 30 -1 10.0.106.23
```

### Test Speed Control
```bash
# Via MQTT
docker exec local-mqtt mosquitto_pub -h localhost -t 'raptor/shop/revpi-135593/cmd' -m '{"wheel_speed": 600}'

# Via mbpoll directly
mbpoll -a 1 -r 122 -t 4 10.0.106.23 -- 600
mbpoll -a 1 -r 122 -t 4 10.0.106.24 -- 550
```

### Check raptor-core logs
```bash
docker compose -f ~/raptor-core/docker-compose.yml logs --tail=20 raptor-core
```

---

## Lessons Learned

1. **Modbus addressing**: goburrow library uses 0-based addresses, mbpoll uses 1-based
2. **Connection management**: Create fresh connections for child VFD writes to avoid stale pipes
3. **VFD auto-switch**: Writing to control registers (not just setpoints) triggers command source switch
4. **CONFIG mode recovery**: Compare working VFD params to broken ones, copy differences
5. **Don't write P0221**: It has dependencies on other parameters that must be set correctly
