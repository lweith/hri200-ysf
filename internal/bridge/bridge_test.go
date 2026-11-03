package bridge

import (
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

func block(a, b string) [20]byte {
	var c [20]byte
	copy(c[:], "                    ")
	copy(c[:10], a)
	copy(c[10:], b)
	return c
}

func message(t *testing.T, cmd byte, payload string) hri.Message {
	t.Helper()
	m, ok := hri.Parse(hri.D1(cmd, payload))
	if !ok {
		t.Fatalf("D1%c does not parse", cmd)
	}
	return m
}

// record builds a DT record of n bytes: length, type 22 62, 5F, filler, the
// 03 end marker and the 8-bit sum.
func record(n int) [20]byte {
	var r [20]byte
	r[0], r[1], r[2], r[3] = byte(n), 0x22, 0x62, 0x5F
	for i := 4; i < n-2; i++ {
		r[i] = 0x30 + byte(i)
	}
	r[n-2] = 0x03
	for _, b := range r[:n-1] {
		r[n-1] += b
	}
	return r
}

// transmit sends a network header (D1F payload) and voice frames through a
// bridge and returns its packets.
func transmit(t *testing.T, header string, frames int) []Packet {
	t.Helper()
	var collected []Packet
	b := &Bridge{Out: func(p Packet) { collected = append(collected, p) }}
	now := time.Unix(0, 0)
	b.Handle(Network, message(t, 'F', header), now)
	var blocks [5][13]byte
	for i := range frames {
		b.Handle(Network, message(t, 'E', hri.VoicePayload(uint16(5+5*i), blocks)), now.Add(time.Duration(i)*100*time.Millisecond))
	}
	if len(collected) != frames+1 {
		t.Fatalf("got %d packets, want header and %d voice frames", len(collected), frames)
	}
	return collected
}

// checkFrame decodes a voice frame and checks its FICH frame number and total
// and its DCH.
func checkFrame(t *testing.T, p Packet, fn, ft byte, dch []byte) {
	t.Helper()
	f, err := ysf.DecodeFICH(p.Frame)
	if err != nil {
		t.Fatal(err)
	}
	if f.FN != fn || f.FT != ft {
		t.Errorf("FN %d FT %d, want %d %d", f.FN, f.FT, fn, ft)
	}
	d, err := ysf.DecodeDCH(p.Frame)
	if err != nil {
		t.Fatalf("fn %d: %v", fn, err)
	}
	if dch != nil && string(d[:]) != string(dch) {
		t.Errorf("fn %d DCH %x, want %x", fn, d, dch)
	}
}

// WIRES-X starts some transmissions with a D1F in the long D1G form (0x6C
// characters, with DT data) rather than the 0x52 header. A record longer than
// one frame (GPS) goes out in frames 6 and 7, with a frame total of 7.
func TestLongNetworkHeader(t *testing.T) {
	dt := record(15)
	header := hri.DataPayload("21216000", block("*****ABCDE", "N0CALL"), block("N0GW", "N0UP"), block("12345", ""), dt)
	if len(header) != 0x6C {
		t.Fatalf("header payload is %#x characters", len(header))
	}
	collected := transmit(t, header, 9)

	if got := string(collected[0].Source[:]); got != "N0CALL    " {
		t.Errorf("source %q, want N0CALL", got)
	}
	csd1, csd2, err1, err2 := ysf.DecodeHeader(collected[0].Frame)
	if err1 != nil || err2 != nil {
		t.Fatalf("header: %v %v", err1, err2)
	}
	if csd1 != block("*****ABCDE", "N0CALL") || csd2 != block("N0GW", "N0UP") {
		t.Errorf("header CSD %q %q", csd1, csd2)
	}
	checkFrame(t, collected[2], 1, 7, []byte("N0CALL    "))
	checkFrame(t, collected[7], 6, 7, dt[:10])
	checkFrame(t, collected[8], 7, 7, dt[10:])
	checkFrame(t, collected[9], 0, 7, nil)
}

// Without a valid record from WIRES-X, the bridge sends a radio's no-GPS
// record in frame 6 with a frame total of 6; Yaesu radios show no callsigns
// otherwise.
func TestNoRecord(t *testing.T) {
	for name, dt := range map[string][20]byte{
		"blank":     {},
		"bad sum":   func() [20]byte { r := record(7); r[6]++; return r }(),
		"too long":  {0x30, 0x22, 0x62, 0x5F},
		"no marker": func() [20]byte { r := record(7); r[5] = 0; return r }(),
	} {
		t.Run(name, func(t *testing.T) {
			header := hri.DataPayload("21216000", block("**********", "N0CALL"), block("N0GW", "N0UP"), block("", ""), dt)
			collected := transmit(t, header, 8)
			checkFrame(t, collected[7], 6, 6, noGPS[:10])
			checkFrame(t, collected[8], 0, 6, []byte("**********"))
		})
	}
}

// WIRES-X sometimes puts the room and an ID in the destination; it goes out
// addressed to everyone, since radios show no callsigns otherwise. A radio
// ID destination is kept.
func TestRoomDestination(t *testing.T) {
	for dest, want := range map[string]string{
		"12345ABCDE": "**********",
		"*****ABCDE": "*****ABCDE",
		"**********": "**********",
	} {
		header := hri.DataPayload("21216000", block(dest, "N0CALL"), block("N0GW", "N0UP"), block("", ""), [20]byte{})
		collected := transmit(t, header, 1)
		csd1, _, err1, _ := ysf.DecodeHeader(collected[0].Frame)
		if err1 != nil {
			t.Fatal(err1)
		}
		if csd1 != block(want, "N0CALL") {
			t.Errorf("%s: header CSD1 %q, want %q", dest, csd1, block(want, "N0CALL"))
		}
		checkFrame(t, collected[1], 0, 6, []byte(want))
	}
}

// A valid short record from WIRES-X is passed on in frame 6.
func TestShortRecord(t *testing.T) {
	dt := record(9)
	header := hri.DataPayload("21216000", block("**********", "N0CALL"), block("N0GW", "N0UP"), block("", ""), dt)
	collected := transmit(t, header, 7)
	checkFrame(t, collected[7], 6, 6, dt[:10])
}

func TestNoGPSIsValid(t *testing.T) {
	if n, ok := dtRecord(noGPS); !ok || n != 7 {
		t.Fatalf("noGPS: %d %t", n, ok)
	}
}
