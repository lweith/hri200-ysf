package analog

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

func tone(hz float64, n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/InRate))
	}
	return s
}

func rms(s []int16) float64 {
	sum := 0.0
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestDecimatorPassesVoiceBlocksAliases(t *testing.T) {
	var d Decimator
	voice := d.Write(tone(1000, InRate))
	if len(voice) != Rate {
		t.Fatalf("%d samples out of 1 s, want %d", len(voice), Rate)
	}
	if got := rms(voice[taps:]) / (10000 / math.Sqrt2); got < 0.97 || got > 1.03 {
		t.Errorf("1 kHz gain %.3f", got)
	}
	var d2 Decimator
	if got := rms(d2.Write(tone(6000, InRate))[taps:]) / (10000 / math.Sqrt2); got > 0.001 {
		t.Errorf("6 kHz gain %.4f: it would alias", got)
	}
}

func TestDecimatorChunkingDoesNotMatter(t *testing.T) {
	in := tone(1234, 9000)
	var whole, parts Decimator
	want := whole.Write(in)
	var got []int16
	for i := 0; i < len(in); {
		n := min(1+i%97, len(in)-i)
		got = append(got, parts.Write(in[i:i+n])...)
		i += n
	}
	if !slices.Equal(got, want) {
		t.Errorf("chunked: %d samples, whole: %d; differ", len(got), len(want))
	}
}

var t0 = time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)

// run feeds states and 10 ms blocks of audio, a state before each block.
func run(r *Recorder, states []hri.State) {
	for i, s := range states {
		if s != "" {
			r.State(s, t0.Add(time.Duration(i)*10*time.Millisecond))
		}
		r.Audio(tone(1000, InRate/100))
	}
}

func repeat(s hri.State, n int) []hri.State {
	out := make([]hri.State, n)
	out[0] = s
	return out
}

func wavSamples(t *testing.T, path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "RIFF" || int(binary.LittleEndian.Uint32(b[40:])) != len(b)-44 {
		t.Fatalf("bad header: %q", b[:44])
	}
	return (len(b) - 44) / 2
}

func TestRecordsAnalog(t *testing.T) {
	r := &Recorder{Dir: t.TempDir(), Keep: 7}
	var states []hri.State
	states = append(states, repeat("04000", 50)...) // idle: fills the pre-roll
	states = append(states, repeat("0A010", 25)...) // mode not known yet
	states = append(states, repeat("0A017", 100)...)
	states = append(states, repeat("04007", 50)...) // squelch closed
	run(r, states)
	// Named for when reception started, 0.5 s in: 01:02:03.5.
	got := wavSamples(t, filepath.Join(r.Dir, "2026-11-04", "010203.wav"))
	want := preRoll + (25+100)*Rate/100 + postRoll
	if got < want-taps || got > want+taps {
		t.Errorf("%d samples, want about %d", got, want)
	}
}

func TestSkipsDigital(t *testing.T) {
	r := &Recorder{Dir: t.TempDir(), Keep: 7}
	var states []hri.State
	states = append(states, repeat("0A010", 10)...)
	states = append(states, repeat("0A038", 100)...)
	states = append(states, repeat("06008", 10)...)
	states = append(states, repeat("04000", 10)...)
	run(r, states)
	if entries, _ := os.ReadDir(r.Dir); len(entries) != 0 {
		t.Errorf("recorded a digital transmission: %v", entries)
	}
}

func TestBackToBackOvers(t *testing.T) {
	r := &Recorder{Dir: t.TempDir(), Keep: 7}
	var states []hri.State
	states = append(states, repeat("0A017", 100)...)
	states = append(states, repeat("04007", 10)...) // reopens inside the post-roll
	states = append(states, repeat("0A017", 200)...)
	states = append(states, repeat("04007", 50)...)
	run(r, states)
	r.Close()
	entries, _ := os.ReadDir(filepath.Join(r.Dir, "2026-11-04"))
	if len(entries) != 2 {
		t.Fatalf("%d files, want 2", len(entries))
	}
}

func TestPrunesOldDays(t *testing.T) {
	r := &Recorder{Dir: t.TempDir(), Keep: 2}
	for _, d := range []string{"2026-10-31", "2026-11-02", "notes"} {
		os.MkdirAll(filepath.Join(r.Dir, d), 0o700)
	}
	run(r, append(repeat("0A017", 50), repeat("04007", 50)...))
	for d, want := range map[string]bool{"2026-10-31": false, "2026-11-02": true, "notes": true, "2026-11-04": true} {
		_, err := os.Stat(filepath.Join(r.Dir, d))
		if (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", d, err == nil, want)
		}
	}
}
