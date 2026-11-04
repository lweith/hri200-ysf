package analog

import "math"

const (
	factor = InRate / Rate
	taps   = 121
	cutoff = 3400.0 // Hz: FM voice's upper edge, under Rate/2 with room to roll off
)

// lowpass is a Blackman-windowed sinc at cutoff, for InRate, with unity gain.
var lowpass = func() [taps]float64 {
	var h [taps]float64
	fc := cutoff / InRate
	sum := 0.0
	for i := range h {
		n := float64(i - (taps-1)/2)
		v := 2 * fc
		if n != 0 {
			v = math.Sin(2*math.Pi*fc*n) / (math.Pi * n)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/(taps-1)) + 0.08*math.Cos(4*math.Pi*float64(i)/(taps-1))
		h[i] = v * w
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}()

// Decimator low-pass filters audio at InRate and keeps every factor-th
// sample, across calls.
type Decimator struct {
	buf   []float64 // the last taps-1 input samples, then new ones
	phase int       // input samples to skip before the next output
}

// Write filters in and returns the samples at Rate.
func (d *Decimator) Write(in []int16) []int16 {
	if d.buf == nil {
		d.buf = make([]float64, taps-1)
	}
	for _, s := range in {
		d.buf = append(d.buf, float64(s))
	}
	var out []int16
	i := d.phase
	for ; i+taps <= len(d.buf); i += factor {
		acc := 0.0
		for k, h := range lowpass {
			acc += h * d.buf[i+k]
		}
		out = append(out, int16(max(-32768, min(32767, math.Round(acc)))))
	}
	// Keep the samples the next output still needs.
	keep := len(d.buf) - (taps - 1)
	if i < keep {
		keep = i
	}
	d.phase = i - keep
	d.buf = append(d.buf[:0], d.buf[keep:]...)
	return out
}
