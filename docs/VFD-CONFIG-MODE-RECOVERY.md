# WEG CFW900 VFD CONFIG Mode Recovery

## Problem Description

The child VFDs (.23 and .24) became stuck in "CONFIG" mode after attempting to write P0221 (command source) via Modbus. The VFDs displayed "CONFIG" instead of "READY" and would not respond to run commands. Power cycling did not fix the issue.

## Root Cause

Writing certain parameters via Modbus can create invalid configuration states if the dependent parameters are not set correctly. In this case, writing P0221=2 (Terminal command source) without the proper supporting parameters caused the VFDs to enter CONFIG mode.

## Symptoms

- VFD display shows "CONFIG" instead of "READY"
- VFD will not enable output or respond to run commands
- Power cycling does not resolve the issue
- ESC button may be unresponsive
- One VFD mysteriously changed to Spanish language

## Solution

Compare parameters from a **working VFD** (in our case, the main VFD at .22) and copy the differing values to the broken VFDs.

### Key Parameters That Fixed the Issue

| Register | Parameter | Broken Value | Working Value | Description |
|----------|-----------|--------------|---------------|-------------|
| P0211    | ?         | 18           | 0             | Unknown     |
| P0219    | ?         | 1            | 0             | Local/Remote selection |
| P0222    | ?         | 2            | 7             | Speed ref source |
| P0223    | ?         | 1            | 0             | Speed ref source 2 |
| P0225    | ?         | 7            | 0             | Direction source |
| P0228    | ?         | 7            | 4             | Run/Stop source 2 (affects R2 profile!) |

### Recovery Commands

Run these from the Pi via SSH:

```bash
# For VFD at 10.0.106.23
mbpoll -a 1 -r 211 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 219 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 222 -t 4 10.0.106.23 -- 7
mbpoll -a 1 -r 223 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 225 -t 4 10.0.106.23 -- 0
mbpoll -a 1 -r 228 -t 4 10.0.106.23 -- 4

# For VFD at 10.0.106.24
mbpoll -a 1 -r 211 -t 4 10.0.106.24 -- 0
mbpoll -a 1 -r 219 -t 4 10.0.106.24 -- 0
mbpoll -a 1 -r 222 -t 4 10.0.106.24 -- 7
mbpoll -a 1 -r 223 -t 4 10.0.106.24 -- 0
mbpoll -a 1 -r 225 -t 4 10.0.106.24 -- 0
mbpoll -a 1 -r 228 -t 4 10.0.106.24 -- 4
```

### How to Compare VFDs

If you need to compare a working VFD with a broken one:

```bash
# Read config block from working VFD (.22)
mbpoll -a 1 -r 200 -t 4 -c 30 -1 10.0.106.22

# Read config block from broken VFD
mbpoll -a 1 -r 200 -t 4 -c 30 -1 10.0.106.23

# Compare the outputs and copy differing values
```

## Post-Recovery Steps

After recovering from CONFIG mode, you MUST manually verify/fix the R2 profile on each child VFD:

1. On each child VFD panel, navigate to **R2 profile settings**
2. Find the **Run/Stop** setting
3. Set it to **"Run/Stop Di"** (not "Serial" or other options)
4. Repeat for both child VFDs (.23 and .24)

This ensures the physical switches control the motors correctly.

## Prevention

**DO NOT write these parameters to child VFDs via Modbus:**
- P0221 (Command Source) - causes auto-switch issues and CONFIG mode
- Any parameter in the 200-230 range without understanding dependencies

**Safe operations:**
- P0227 (Direction) - can be written, but may trigger auto-switch to serial mode
- Reading any parameter is safe

## Related Issues

1. **Auto-switch to Serial mode**: Any Modbus write to child VFDs can trigger the VFD to switch from "Run/Stop DI" to "Serial" command mode, breaking physical switch control. This is a VFD firmware behavior, not a bug in our code.

2. **Direction control limitation**: Due to the auto-switch issue, we cannot reliably control wheel direction via Modbus while maintaining physical switch control. Direction must be set manually on the VFD panel or we need a different approach.

## Date

2025-01-22

## References

- WEG CFW900 Programming Manual
- WEG CFW900 Modbus TCP User's Guide
