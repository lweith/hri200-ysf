package proxy

import (
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// The modem is inhibited exactly where its PTT would be refused: through a
// reception, the repeater's hang and its CW ID (0400B, every IDINT, 7-8 s,
// serial trace 2026-11-07), and clear once the repeater is idle, in Digital or
// left in Analog for the session.
func TestTXInhibitFollowsTheChannel(t *testing.T) {
	s, sent := analogSharer()
	check := func(at int, want bool, what string) {
		t.Helper()
		if got := s.TXInhibited(ms(at)); got != want {
			t.Errorf("%s: inhibited %v, want %v", what, got, want)
		}
	}
	check(0, false, "idle in Digital")
	s.FromRadio(stateP("0A017"), ms(100))
	check(100, true, "receiving")
	s.FromRadio(stateP("03007"), ms(500))
	check(500, true, "the hang")
	s.FromRadio(stateP("04000"), ms(2500))
	s.FromRadio(stateP("0400B"), ms(2505))
	check(2505, true, "the CW ID")
	s.FromRadio(stateP("04000"), ms(10000))
	check(10000, false, "idle after the ID")

	// Keyed for the modem, and back to back after it.
	s.PTT(true, ms(10100))
	check(10100, false, "switching for the modem")
	s.FromRadio(echoAnalog, ms(10176))
	s.FromRadio(stateP("00000"), ms(10216))
	s.FromRadio(stateP("00002"), ms(10240))
	check(10300, false, "keyed for the modem")
	s.PTT(false, ms(11000))
	check(11100, false, "back to back")

	// Left in Analog for the session: the radio's own states again.
	s.Sessions = func(now time.Time) int { return 1 }
	s.Tick(ms(11400))
	if s.analog.phase != aStay {
		t.Fatalf("phase %d, want left in Analog", s.analog.phase)
	}
	s.FromRadio(hri.D1('C', hri.StatePayload("00000")), ms(11500))
	check(11500, true, "just unkeyed, not yet idle")
	s.FromRadio(hri.D1('C', hri.StatePayload("04000")), ms(12000))
	check(12000, false, "idle in Analog")
	s.FromRadio(stateP("0A017"), ms(13000))
	s.FromRadio(stateP("04007"), ms(13400))
	check(13400, true, "the hang in Analog")
	s.FromRadio(stateP("04000"), ms(15400))
	s.FromRadio(stateP("0400B"), ms(15405))
	check(15405, true, "the CW ID in Analog")

	// Wherever it is clear, PTT is taken.
	s.FromRadio(stateP("04000"), ms(23000))
	check(23000, false, "idle again")
	n := len(*sent)
	s.PTT(true, ms(23100))
	if len(*sent) == n {
		t.Error("clear, but PTT was refused")
	}
}

func TestTXInhibitWithoutARadio(t *testing.T) {
	var s Sharer
	if !s.TXInhibited(ms(0)) {
		t.Error("no radio to transmit on, but not inhibited")
	}
}
