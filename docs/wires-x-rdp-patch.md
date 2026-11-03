# Patching WIRES-X so RDP doesn't end the room

When an RDP session connects to the machine running WIRES-X, or disconnects
from it, the active room ends about 5–6 seconds later. The room comes back only
when someone reconnects it. Two conditional jumps in `Wires-X.exe` cause this.
Patching them lets you connect and disconnect RDP as often as you like.

Applies to **WIRES-X Ver-2.010A** only:

| | SHA-256 |
|---|---|
| original | `8F05DC26794144438566B880CE775F5178D62EB3C4DEB8C053089216342BC671` |
| patched  | `7DD91B5B974427FA894D84F2C25F7DF2A756C60C1AA488901A4FE21E6F395467` |

## Applying it

1. Close WIRES-X in the desktop session it runs in (over RDP is fine). It runs
   in the user's session, so `taskkill` from an SSH shell (session 0) can't
   close it cleanly.
2. Copy `scripts/wires-x-rdp-patch.ps1` to the machine, then run it from an
   elevated shell:

   ```
   powershell -ExecutionPolicy Bypass -File wires-x-rdp-patch.ps1
   ```

   The script keeps the original as `Wires-X.orig.exe` next to the exe and
   checks the hash before and after. It refuses to run in these cases:
   - WIRES-X is running from that file.
   - The exe is some build other than 2.010A.
   - The bytes at the offsets aren't what they should be.

   Running it again on a patched exe does nothing.
3. Start WIRES-X again.

To undo, close WIRES-X and run the script with `-Restore`.

**Smart App Control.** The patch breaks the exe's Authenticode signature
(DigiCert, Yaesu). WIRES-X doesn't check its own signature, but Windows 11's
Smart App Control blocks unsigned executables, so it has to be off. WIRES-X
doesn't need any other signature-related change. SAC is in Windows Security >
App & browser control. On many Windows 11 builds, SAC can't be turned back on
without reinstalling Windows.

**Updates.** A WIRES-X update or reinstall puts back an unpatched exe, and the
drops return. The script refuses the new build. Find the two sites again, as
described under [Finding the sites again](#finding-the-sites-again).

## Why RDP ends the room

Attaching an RDP session to the Windows session, or detaching one, swaps that
session's audio endpoints. The HRI-200 is a USB sound card. Its DirectSound
capture stream keeps a handle that no longer delivers buffers. WIRES-X sees
this in two places, and either one ends the room.

### 1. Capture watchdog (the cause that matters)

A thread at `0x4D2110` waits on the capture notification events with
`WaitForMultipleObjects(2, …, FALSE, 1000)`:

```
4d2133:  push   0x3e8                 ; 1000 ms
4d213f:  call   [WaitForMultipleObjects]
4d2149:  cmp    eax, 0x102            ; WAIT_TIMEOUT
...
4d2158:  push   0x72bd44              ; "No input sound."  (logged on the first timeout)
4d2166:  inc    edi                   ; consecutive timeouts
4d216a:  cmp    edi, 5
4d216d:  jl     0x4d2277              ; fewer than 5: keep waiting
4d2173:  push   0x72bd2c              ; "Reset input device."
...                                   ; CreateEvent, then post WM 0x732 to the main window
```

The main window maps message `0x732` to `0x4D2A80`. That handler calls
`StopRecord` (`0x4D28A0`). The state machine then raises `Ev_OnAirLock` and
`Ev_ConfEnd`, and the room is gone. Five one-second timeouts plus the handling
add up to the 5–6 seconds observed.

**Patch:** at `0x4D216D` (file offset `0xD156D`), `0F 8C` → `90 E9`. The
`jl rel32` becomes `nop; jmp rel32`. Both instructions end at the same address,
so the displacement (`0x104`) is unchanged and the jump goes to the same target
every time. The thread carries on as before, logging "No input sound." once and
then waiting again, but it never resets the device.

### 2. WM_DEVICECHANGE audio check

The `WM_DEVICECHANGE` handler (`0x4A2B10`) responds to
`DBT_DEVNODES_CHANGED`, which RDP also generates, by asking the
`CAudioDevicesVista` object (global `0x2F1DD50`, vtable slot `+0xC`) whether the
configured audio device is still present. That object enumerates active
endpoints with `IMMDeviceEnumerator::EnumAudioEndpoints(eAll,
DEVICE_STATE_ACTIVE)` and compares friendly names. While the endpoints are
being swapped, the name is briefly missing, and the handler calls `0x41ABB0`.
That function posts the same `0x732`.

```
4a2c78:  call   [edx+0xc]             ; device still present?
4a2c7a:  test   eax, eax
4a2c7c:  jne    0x4a2c8d              ; yes: done
4a2c7e:  ...
4a2c88:  call   0x41abb0              ; no: reset, ends the room
4a2c8d:  ...
```

**Patch:** at `0x4A2C7C` (file offset `0xA207C`), `75` → `EB`, so `jne`
becomes `jmp` and the check is always skipped.

Patching this site alone was not enough: the watchdog still ended the room.
Both patches are needed.

## What still works

- **Removing the HRI-200.** `DBT_DEVICEREMOVECOMPLETE` for its COM port takes
  a separate branch to `0x41ABB0`, which is not patched. WIRES-X still notices
  when the HRI-200 is unplugged.
- **Other devices.** Plugging in unrelated devices is now ignored, which is the
  point of the patch.
- **Digital (C4FM) rooms.** The HRI-200 carries DN voice over its serial link,
  not as audio, so a dead capture stream doesn't affect them. Rooms stayed up
  across repeated RDP connects and disconnects.

## Known gap: analog audio

WIRES-X opens the HRI-200's capture device once, at startup. It reopens it
only through the reset this patch disables. After the first RDP session change,
capture probably stays dead until WIRES-X restarts. That would leave analog
FM from the radio into WIRES-X without audio. This has not been tested. If
analog rooms matter, test an FM transmission into the node after an RDP
connect.

## Finding the sites again

The addresses above are virtual addresses in 2.010A. `.text` starts at VA
`0x401000` and file offset `0x400`, so file offset = VA − `0x400C00`. In
another build:

- **Watchdog:** find the string `Reset input device.` in `.rdata`, then find the
  `push` of its address. The `cmp reg, 5` / `jl` just before it is the site.
- **Device change:** find the `WM_DEVICECHANGE` handler. It compares `wParam`
  with `7` (`DBT_DEVNODES_CHANGED`) and makes an indirect call through the
  audio-device object's vtable. The `jne` on the result skips the call to the
  reset function. That reset function is the same one the
  `DBT_DEVICEREMOVECOMPLETE` branch calls.

WIRES-X's own log, `____WIRESX_Vs.Log`, shows which
path fired. "No input sound." followed by "Reset input device." and then
`StopRecord` means the watchdog fired. A `StopRecord` right after a device
change, without those two lines, means the device-change path fired.
