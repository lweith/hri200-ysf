# hri200-ysf

`hri200-ysf` turns the traffic between WIRES-X and a Yaesu HRI-200 into YSF.
It runs in one of two modes:

- **Sniffer** (default): reads the USB traffic with usbmon and never writes to
  the HRI-200. It serves what it hears as a small YSF reflector.
- **Proxy** (`-proxy`): sits between WIRES-X, which talks to a USB gadget
  standing in for the HRI-200, and the real HRI-200 when it is attached. It
  caches the HRI-200's replies, answers WIRES-X itself when the radio is absent,
  and can key the radio for packet and for YSF clients.

The rest of the repository supports that: `ax25-door` and `ax25-term` run
connected-mode AX.25 over direwolf's AGW port, `hri200-analog` records the
analog receive audio, and `hri200-fakecodec` presents the codec to the WIRES-X
guest.

## Build and test

Go 1.26 or later.

```
go build ./cmd/hri200-ysf
go test ./...
```

## Sniffer

Reads the HRI-200's usbmon bus. Needs root to open `/dev/usbmonN`.

```
hri200-ysf                              # live, bus found by USB ID
hri200-ysf -pcap capture.pcap           # replay a tcpdump capture in real time
hri200-ysf -pcap capture.pcap -fast     # replay as fast as possible
```

It serves YSF clients on UDP `:42000` (`-listen`). The reflector name, ID,
description and gateway callsign are set with `-name`, `-id`, `-description`
and `-gateway`. `-overs DIR` keeps each transmission's frames under `DIR`, one
subdirectory per day, for `-keep` days (default 7). `-cache FILE` also learns
the HRI-200's replies so the proxy can use them later.

## Proxy

```
hri200-ysf -proxy -cache /var/lib/hri200-ysf/cache.json -allow 10.99.0.0/24
```

- `-downstream` is the tty WIRES-X talks to (default `/dev/ttyGS0`, the USB
  gadget). `-upstream` is the HRI-200's tty, or `auto` to find it by USB ID.
- `-allow` lists the networks whose YSF audio is played to WIRES-X as received
  audio. It is played only while the repeater is idle.
- With `-transmit`, the `-allow`ed clients' overs are also transmitted on the
  HRI-200's radio, so the repeater hears them. Transmitting is on the air, so
  check that you're licensed and that the setup is what you intend before you
  turn it on.
- `-metrics ADDR` serves how the repeater is used as Prometheus metrics, at
  `/metrics`. Overs are timed from the HRI-200's states: received off the air
  (analog or digital) and transmitted for WIRES-X or for clients.

### Keying the radio for packet (`-ptt`)

`-ptt` presents a fake CM108 USB sound interface, through uhid, whose GPIO 3
keys the HRI-200's radio in Analog. direwolf uses it as `PTT CM108`. The proxy
switches the repeater to Analog for each transmission and back to Digital
afterwards.

- The proxy refuses PTT while the channel is busy: a reception, the repeater's
  hang time, its CW ID, or WIRES-X transmitting. Those refusals used to show up
  as lost frames, so the TX inhibit below was added.
- `-txinh PULL` drives a simulated GPIO line (set up by
  `scripts/hri200-txinh-setup`, read through `PULL`) that direwolf uses as
  `TXINH`. It is held up while the channel is busy, so direwolf holds its frames
  instead of keying into a refused PTT.
- `-hang-zero` sets the repeater's hang time (SQLT) to 0 for the length of a
  packet session in Analog, and writes the original value back when the radio
  returns to Digital. The original value is kept in the cache until the radio's
  echo confirms the restore, so a crash mid-session doesn't lose it.
- `-agw-monitor` is direwolf's AGW port (default `127.0.0.1:8000`). The proxy
  follows the AX.25 sessions there and keeps room traffic off the repeater while
  a session is open.

## Verbose and debugging

- `-v` logs every HRI-200 message.
- `-dump FILE` writes every frame as hex to a file.

## systemd

The units are in `systemd/`:

| unit | what it runs |
|---|---|
| `hri200-ysf.service` | sniffer, with the cache and overs under `/var/lib/hri200-ysf` |
| `hri200-ysf-proxy.service` | proxy with `-ptt`; conflicts with the sniffer unit, since both serve YSF on the same port |
| `hri200-txinh.service` | the simulated GPIO line for `TXINH` (oneshot) |
| `direwolf-hri200.service` | direwolf on the HRI-200's radio, with its config in `direwolf-hri200.conf` |
| `hri200-direwolf-feed.service` | a read-only usbmon reader that forwards receive audio to direwolf on UDP `127.0.0.1:7355` |
| `ax25-doors.service` | `ax25-door` serving the game doors over direwolf |

The `scripts/direwolf-poc-install.sh` script installs the direwolf pieces,
including the TXINH line; `direwolf-poc-remove.sh` reverts them. Both scripts
are for the HRI-200 host.

## Layout

```
cmd/hri200-ysf     this program
cmd/ax25-door      connected-mode AX.25 doors (multi-door, one AGW connection)
cmd/ax25-term      line-mode AX.25 terminal
cmd/hri200-analog  analog receive recorder, and a feed for direwolf
cmd/hri200-fakecodec  the codec the WIRES-X guest sees
internal/hri       the HRI-200's D1 message format
internal/proxy     caching proxy, PTT and mode state machine, TX inhibit
internal/usbmon    usbmon capture and device discovery
internal/cm108     the fake CM108 for direwolf's PTT
internal/agw       AGW client
internal/usage     usage metrics
docs/              notes on WIRES-X (see wires-x-rdp-patch.md)
```

## Status

Experimental. The packet path (`-ptt`, `-txinh`, `-hang-zero`, `ax25-door`) was
tested on the air against a DR-1X repeater in November 2026. The sniffer and
YSF proxy paths have not been re-checked against that setup.
