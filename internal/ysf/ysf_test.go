package ysf

import (
	"math/rand/v2"
	"testing"
)

func TestGolayKnownCodeword(t *testing.T) {
	// MMDVMHost's ENCODING_TABLE_24128[1].
	if got := golay24(1); got != 0x18EB {
		t.Fatalf("golay24(1) = %#x, want 0x18eb", got)
	}
}

func TestGolayCorrectsThree(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		d := uint32(r.IntN(4096))
		w := golay24(d)
		for range 3 {
			w ^= 1 << r.IntN(24)
		}
		got, ok := golayDecode(w)
		if !ok || got != d {
			t.Fatalf("golayDecode with errors: got %#x %v, want %#x", got, ok, d)
		}
	}
}

func TestViterbiRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, nBits := range []int{100, 180} {
		for range 200 {
			in := make([]byte, (nBits+7)/8)
			for i := range nBits - 4 { // leave the four tail bits zero
				setBit(in, i, r.IntN(2) == 1)
			}
			coded := convolve(in, nBits)
			// Two isolated errors well apart are always correctable at K=5.
			flip := func(i int) { setBit(coded, i, !getBit(coded, i)) }
			flip(r.IntN(nBits / 2))
			flip(nBits + r.IntN(nBits-10))
			out := viterbi(coded, nBits)
			for i := range nBits {
				if getBit(out, i) != getBit(in, i) {
					t.Fatalf("nBits %d: bit %d differs", nBits, i)
				}
			}
		}
	}
}

func TestFICHRoundTrip(t *testing.T) {
	want := FICH{FI: FICommunications, CS: 2, CM: 0, BN: 0, BT: 0, FN: 5, FT: 7, Dev: true, MR: 2, VoIP: true, DT: DTVDMode2, SQL: false, DGID: 42}
	var dch [10]byte
	var voice [5][VoiceBlockLength]byte
	frame := VDMode2Frame(want, dch, voice)
	setBit(frame, 5*8+17, !getBit(frame, 5*8+17)) // one bit error inside the FICH
	got, err := DecodeFICH(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("DecodeFICH = %+v, want %+v", got, want)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	var csd1, csd2 [20]byte
	copy(csd1[:], "*****ABCDEN0CALL    ")
	copy(csd2[:], "N0GW      N0UP      ")
	frame := HeaderFrame(FICH{FI: FIHeader, CS: 2, FT: 7, DT: DTVDMode2}, csd1, csd2)
	got1, got2, err1, err2 := DecodeHeader(frame)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if got1 != csd1 || got2 != csd2 {
		t.Fatalf("DecodeHeader = %q %q", got1, got2)
	}
}

func TestDCHAndVoiceRoundTrip(t *testing.T) {
	var dch [10]byte
	copy(dch[:], "N0CALL    ")
	var voice [5][VoiceBlockLength]byte
	for i := range voice {
		for j := range voice[i] {
			voice[i][j] = byte(i*16 + j)
		}
	}
	frame := VDMode2Frame(FICH{FI: FICommunications, CS: 2, FN: 1, FT: 7, DT: DTVDMode2}, dch, voice)
	got, err := DecodeDCH(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got != dch {
		t.Fatalf("DecodeDCH = %q, want %q", got, dch)
	}
	if VoiceBlocks(frame) != voice {
		t.Fatal("VoiceBlocks differ")
	}
}

func TestCRCDetectsCorruption(t *testing.T) {
	var dch [10]byte
	copy(dch[:], "N0CALL    ")
	var voice [5][VoiceBlockLength]byte
	frame := VDMode2Frame(FICH{FI: FICommunications, CS: 2, FT: 7, DT: DTVDMode2}, dch, voice)
	for i := 30; i < 35; i++ { // wreck the first DCH chunk
		frame[i] ^= 0xFF
	}
	if _, err := DecodeDCH(frame); err == nil {
		t.Fatal("corrupted DCH decoded without error")
	}
}
