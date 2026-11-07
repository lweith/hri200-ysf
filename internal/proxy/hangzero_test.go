package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// What the DR-1X took and echoed (2026-11-07 02:54:35, SQLT 0; 02:55:18,
// back to Digital), with the mode given as it must be.
const (
	echoAnalogNoHang  = "D1M006BE000146.23500+000.000000210302310020146.83500+000.0000002103023100200W8HHF/R   0180000000012010000105020500"
	echoDigitalNoHang = "D1M006BE000146.23500+000.000000210302310020146.83500+000.0000002103023100210W8HHF/R   0180000000012010000105020500"
)

func TestModeWrite(t *testing.T) {
	// The hang-time field is where the radio's echo has it.
	w := ModeWrite(analogMode, "0000")
	if got := w[7+sqltAt : 7+sqltAt+4]; got != "0000" {
		t.Errorf("SQLT field %q", got)
	}
	if echoAnalog[7+sqltAt:7+sqltAt+4] != "2000" || echoAnalogNoHang[7+sqltAt:7+sqltAt+4] != "0000" {
		t.Error("sqltAt doesn't match the radio's echoes")
	}
	if w[7+67] != analogMode || len(w) != len(wxSetAnalog) {
		t.Errorf("mode %q, length %d", w[7+67], len(w))
	}
	if strings.Replace(w, "0000", "FFFF", 1) != wxSetAnalog {
		t.Errorf("differs from WIRES-X's mode write beyond SQLT: %q", w)
	}
	if ModeWrite(digitalMode, "") != wxSetDigital {
		t.Error("without a hang time it isn't WIRES-X's mode write")
	}
}

// quiet is when the repeater goes back to Digital after a PTT at 0.
var quiet = int(packetQuiet / time.Millisecond)

func hangSharer(sqlt string) (*Sharer, *[]string, *Cache) {
	s, sent := analogSharer()
	c := &Cache{Replies: map[string]string{}}
	s.emu.Cache = c
	s.HangZero = true
	if sqlt != "" {
		c.NoteMode(strings.Replace(echoDigital, "2000", sqlt, 1))
	}
	return s, sent, c
}

// A packet: no hang while in Analog, the repeater's own back after, and
// nothing owed once its echo shows it.
func TestHangZeroCycle(t *testing.T) {
	s, sent, c := hangSharer("2000")
	s.PTT(true, ms(0))
	if len(*sent) != 1 || (*sent)[0] != ModeWrite(analogMode, "0000") {
		t.Fatalf("switch to Analog: %q", *sent)
	}
	if c.HangOwed() != "2000" {
		t.Fatalf("owed %q, want 2000 (saved before the write)", c.HangOwed())
	}
	c.NoteMode(echoAnalogNoHang)
	s.FromRadio(echoAnalogNoHang, ms(76))
	s.FromRadio(stateP("00000"), ms(116))
	s.PTT(false, ms(1000))
	s.Tick(ms(1400))
	s.FromRadio(hri.D1('C', hri.StatePayload("04000")), ms(2000))
	s.Tick(ms(quiet + 100)) // packet quiet: back to Digital
	if last := (*sent)[len(*sent)-1]; last != ModeWrite(digitalMode, "2000") {
		t.Fatalf("switch back: %q, want Digital with the hang time", last)
	}
	if c.HangOwed() != "2000" {
		t.Fatal("stopped owing it before the radio confirmed")
	}
	s.FromRadio(echoDigital, ms(quiet+180))
	if c.HangOwed() != "" {
		t.Errorf("still owes %q after the echo showed it", c.HangOwed())
	}
}

func TestHangZeroOwedUntilEchoed(t *testing.T) {
	s, sent, c := hangSharer("2000")
	s.PTT(true, ms(0))
	s.FromRadio(echoAnalogNoHang, ms(76))
	s.FromRadio(stateP("00000"), ms(116))
	s.PTT(false, ms(1000))
	s.Tick(ms(1400))
	s.FromRadio(hri.D1('C', hri.StatePayload("04000")), ms(2000))
	s.Tick(ms(quiet + 100))
	s.FromRadio(echoDigitalNoHang, ms(quiet+180)) // took the mode, not the hang
	if c.HangOwed() != "2000" {
		t.Errorf("owed %q after an echo without it", c.HangOwed())
	}
	// The next session's restore writes it again; its own switch doesn't
	// overwrite what is owed with the 0 it set.
	c.NoteMode(echoDigitalNoHang)
	s.Tick(ms(quiet + 500)) // settled
	s.FromRadio(idleP, ms(quiet+600))
	n := len(*sent)
	s.PTT(true, ms(quiet+1000))
	if len(*sent) <= n || (*sent)[n] != ModeWrite(analogMode, "0000") || c.HangOwed() != "2000" {
		t.Errorf("next switch: %q, owed %q", (*sent)[n:], c.HangOwed())
	}
}

// No hang time known, or none set: plain mode writes, nothing owed.
func TestHangZeroNothingToRestore(t *testing.T) {
	for _, sqlt := range []string{"", "0000"} {
		s, sent, c := hangSharer(sqlt)
		s.PTT(true, ms(0))
		if (*sent)[0] != wxSetAnalog || c.HangOwed() != "" {
			t.Errorf("SQLT %q: sent %q, owed %q", sqlt, (*sent)[0], c.HangOwed())
		}
	}
}

// Off, it leaves the hang time alone, but still pays back what is owed (a
// previous run with it on).
func TestHangZeroOff(t *testing.T) {
	s, sent, c := hangSharer("2000")
	s.HangZero = false
	s.PTT(true, ms(0))
	if (*sent)[0] != wxSetAnalog {
		t.Errorf("off: %q", (*sent)[0])
	}
	c.OweHang("1500")
	s.FromRadio(echoAnalog, ms(76))
	s.FromRadio(stateP("00000"), ms(116))
	s.PTT(false, ms(1000))
	s.Tick(ms(1400))
	s.FromRadio(hri.D1('C', hri.StatePayload("04000")), ms(2000))
	s.Tick(ms(quiet + 100))
	if last := (*sent)[len(*sent)-1]; last != ModeWrite(digitalMode, "1500") {
		t.Errorf("owed 1500, switched back with %q", last)
	}
}
