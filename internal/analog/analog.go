// Package analog records the analog FM transmissions an HRI-200's radio
// receives. The radio streams its receive audio to the PC continuously (48 kHz,
// 16-bit mono); its serial states say when the squelch is open and whether the
// signal is analog or digital. Each analog transmission becomes an 8 kHz WAV
// file, dir/YYYY-MM-DD/HHMMSS.wav (UTC).
package analog

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

const (
	// Rate is the rate files are written at; InRate the radio's.
	Rate   = 8000
	InRate = 48000
	// preRoll and postRoll widen each recording past the squelch states,
	// which may lead or lag the audio.
	preRoll  = Rate * 3 / 10
	postRoll = Rate * 3 / 10
)

// mode is the last character of a receive state: '7' analog, '8' digital,
// anything else not known yet.
func mode(s hri.State) byte {
	if len(s) != 5 {
		return 0
	}
	return s[4]
}

type phase int

const (
	idle      phase = iota
	pending         // receiving, mode not known yet
	recording       // receiving analog
	skipping        // receiving digital
	tailing         // squelch closed, writing the post-roll
)

// Recorder turns audio and states into files. Call Audio and State from one
// goroutine.
type Recorder struct {
	Dir  string
	Keep int // days of recordings to keep
	Log  func(format string, args ...any)

	dec     Decimator
	history []int16 // the last preRoll samples, for the pre-roll
	phase   phase
	start   time.Time
	held    []int16 // audio while pending
	tail    int     // post-roll samples still to write
	w       *wavWriter
	day     string
}

func (r *Recorder) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// State notes a state the radio reported.
func (r *Recorder) State(s hri.State, at time.Time) {
	if s.Receiving() {
		switch r.phase {
		case idle, tailing:
			if r.phase == tailing {
				r.finish()
			}
			r.phase, r.start = pending, at
			r.held = append([]int16(nil), r.history...)
		}
		switch mode(s) {
		case '7':
			if r.phase == pending {
				r.open()
			}
		case '8':
			if r.phase == pending {
				r.phase, r.held = skipping, nil
			}
		}
		return
	}
	switch r.phase {
	case pending, skipping:
		r.phase, r.held = idle, nil
	case recording:
		r.phase, r.tail = tailing, postRoll
	}
}

// Audio takes the radio's receive audio, at InRate.
func (r *Recorder) Audio(in []int16) {
	out := r.dec.Write(in)
	r.history = append(r.history, out...)
	if n := len(r.history) - preRoll; n > 0 {
		r.history = append(r.history[:0], r.history[n:]...)
	}
	switch r.phase {
	case pending:
		r.held = append(r.held, out...)
	case recording:
		r.write(out)
	case tailing:
		n := min(len(out), r.tail)
		r.write(out[:n])
		r.tail -= n
		if r.tail == 0 {
			r.finish()
			r.phase = idle
		}
	}
}

// Close ends any recording in progress.
func (r *Recorder) Close() {
	if r.w != nil {
		r.finish()
	}
	r.phase = idle
}

func (r *Recorder) open() {
	r.phase = recording
	w, err := r.create(r.start.UTC())
	if err != nil {
		r.logf("analog: %v", err)
	}
	r.w = w
	r.write(r.held)
	r.held = nil
}

func (r *Recorder) write(s []int16) {
	if r.w == nil {
		return
	}
	if err := r.w.write(s); err != nil {
		r.logf("analog: %v", err)
		r.w.f.Close()
		r.w = nil
	}
}

func (r *Recorder) finish() {
	if r.w == nil {
		return
	}
	secs := float64(r.w.samples) / Rate
	if err := r.w.close(); err != nil {
		r.logf("analog: %v", err)
	} else {
		r.logf("analog: %.1f s, %s", secs, r.w.f.Name())
	}
	r.w = nil
}

func (r *Recorder) create(start time.Time) (*wavWriter, error) {
	day := start.Format(time.DateOnly)
	if day != r.day {
		r.prune(start)
		r.day = day
	}
	dir := filepath.Join(r.Dir, day)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	base := start.Format("150405")
	for i := 0; ; i++ {
		name := base + ".wav"
		if i > 0 {
			name = fmt.Sprintf("%s-%d.wav", base, i)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return newWAV(f)
	}
}

// prune removes the day directories older than Keep days.
func (r *Recorder) prune(now time.Time) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -r.Keep).Format(time.DateOnly)
	for _, e := range entries {
		if _, err := time.Parse(time.DateOnly, e.Name()); err != nil || !e.IsDir() {
			continue
		}
		if e.Name() < cutoff {
			if err := os.RemoveAll(filepath.Join(r.Dir, e.Name())); err != nil {
				r.logf("analog: %v", err)
			}
		}
	}
}

// wavWriter writes 16-bit mono PCM at Rate, fixing the header's sizes on close.
type wavWriter struct {
	f       *os.File
	samples int
}

func newWAV(f *os.File) (*wavWriter, error) {
	w := &wavWriter{f: f}
	if _, err := f.Write(wavHeader(0)); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

func wavHeader(samples int) []byte {
	data := uint32(2 * samples)
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+data)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], Rate)
	binary.LittleEndian.PutUint32(h[28:], 2*Rate)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], data)
	return h
}

func (w *wavWriter) write(s []int16) error {
	if len(s) == 0 {
		return nil
	}
	b := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	_, err := w.f.Write(b)
	w.samples += len(s)
	return err
}

func (w *wavWriter) close() error {
	if _, err := w.f.WriteAt(wavHeader(w.samples), 0); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}
