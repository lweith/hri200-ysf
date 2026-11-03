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

// WIRES-X starts some transmissions with a D1F in the long D1G form (0x6C
// characters, with DT data) rather than the 0x52 header.
func TestLongNetworkHeader(t *testing.T) {
	var dt [20]byte
	copy(dt[:], "0123456789ABCDEFGHIJ")
	header := hri.DataPayload("21216000", block("*****ABCDE", "N0CALL"), block("N0GW", "N0UP"), block("12345", ""), dt)
	if len(header) != 0x6C {
		t.Fatalf("header payload is %#x characters", len(header))
	}

	var collected []Packet
	b := &Bridge{Out: func(p Packet) { collected = append(collected, p) }}
	now := time.Unix(0, 0)
	b.Handle(Network, message(t, 'F', header), now)
	var blocks [5][13]byte
	for i := range 8 {
		b.Handle(Network, message(t, 'E', hri.VoicePayload(uint16(5+5*i), blocks)), now.Add(time.Duration(i)*100*time.Millisecond))
	}

	if len(collected) != 9 {
		t.Fatalf("got %d packets, want header and 8 voice frames", len(collected))
	}
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
	// Frames 6 and 7 carry the DT data.
	for fn, want := range map[int]string{6: "0123456789", 7: "ABCDEFGHIJ"} {
		d, err := ysf.DecodeDCH(collected[1+fn].Frame)
		if err != nil {
			t.Fatalf("frame %d: %v", fn, err)
		}
		if string(d[:]) != want {
			t.Errorf("frame %d DCH %q, want %q", fn, d, want)
		}
	}
}
