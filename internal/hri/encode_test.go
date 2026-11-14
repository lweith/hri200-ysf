package hri

import (
	"encoding/hex"
	"testing"
)

func block20(s string) (b [20]byte) {
	copy(b[:], s)
	for i := len(s); i < 20; i++ {
		b[i] = ' '
	}
	return
}

func dt20(t *testing.T, hexed string, pad byte) (b [20]byte) {
	raw, err := hex.DecodeString(hexed)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = pad
	}
	copy(b[:], raw)
	return
}

// The HRI-200's D1G for an FT-5D without a position, captured 2026-11-14
// 05:07 on its serial port: 0x52 characters, DT 7 bytes.
const capturedShort = "2100200021636EBKrQKN6FLN/H                                     EBKrQ2D22615F330345"

func TestRadioDataPayloadShortDT(t *testing.T) {
	csd1, csd2, csd3 := block20("21636EBKrQKN6FLN/H"), block20(""), block20("               EBKrQ")
	// As the proxy has it from a hotspot's frames 6 and 7: the DT, zeros to
	// the end of frame 6's half, frame 7's half blank.
	dt := dt20(t, "2D22615F330345000000", ' ')
	got := RadioDataPayload(RadioPrefix, csd1, csd2, csd3, dt)
	if got != capturedShort {
		t.Errorf("payload\n got %q\nwant %q", got, capturedShort)
	}
	if len(got) != 0x52 {
		t.Errorf("length %#x, want 0x52", len(got))
	}
	d, ok := ParseData(Message{Cmd: 'G', Payload: capturedShort})
	if !ok {
		t.Fatal("ParseData rejected the captured short D1G")
	}
	if d.CSD1 != csd1 || d.CSD3 != csd3 || DTLen(d.DT) != 7 || hex.EncodeToString(d.DT[:7]) != "2d22615f330345" {
		t.Errorf("parsed %+v", d)
	}
}

func TestDTLen(t *testing.T) {
	for _, c := range []struct {
		name, hexed string
		pad         byte
		want        int
	}{
		// FT-5D with a position (captured 2026-11-02): ends in its last byte.
		{"long", "2822625F335431535336526F43426C212C2003C1", 0, 20},
		// The hotspot's, tonight: counter, 22 61 5F 33, end, sum, then padding.
		{"short padded", "1722615F33032F000000", ' ', 7},
		{"no end", "", ' ', 20},
		{"03 with a wrong sum", "1722615F330300", 0, 20},
	} {
		if got := DTLen(dt20(t, c.hexed, c.pad)); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// A long DT keeps the 0x6C form, unchanged from DataPayload.
func TestRadioDataPayloadLongDT(t *testing.T) {
	csd := block20("*****EBKrQKN6FLN")
	dt := dt20(t, "2822625F335431535336526F43426C212C2003C1", 0)
	got := RadioDataPayload(RadioPrefix, csd, block20(""), block20(""), dt)
	if want := DataPayload(RadioPrefix, csd, block20(""), block20(""), dt); got != want || len(got) != 0x6C {
		t.Errorf("long DT: %q (%#x)", got, len(got))
	}
}
