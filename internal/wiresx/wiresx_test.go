package wiresx

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/ysf"
)

func csd(s string) (b [20]byte) {
	b = blank
	copy(b[:], s)
	return b
}

// command builds a command's bytes: sequence, code, arguments, end marker, sum.
func command(seq byte, code [3]byte, args string) []byte {
	b := append([]byte{seq}, code[:]...)
	b = append(b, args...)
	b = append(b, 0x03)
	return append(b, sum(b))
}

func TestSumMatchesCapture(t *testing.T) {
	// A node info request as an FT-5D sent it ("33": the model), sum 22.
	c := command(0x7F, DXRequest, "3"+"          ")
	if got := hex.EncodeToString(c); got != "7f5d715f332020202020202020202003"+"22" {
		t.Errorf("command = %s", got)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	for _, args := range []string{"3          ", "301" + string(bytes.Repeat([]byte(" "), 19)), string(bytes.Repeat([]byte("x"), 150))} {
		want := Command{
			CSD1: csd("*****12345N0CALL"), CSD2: blank, CSD3: csd("               12345"),
			Data: command(0x00, AllRequest, args),
		}
		var a Assembler
		var got Command
		var done int
		for _, f := range Frames(want) {
			if c, ok := a.Frame(f); ok {
				got = c
				done++
			}
		}
		if done != 1 {
			t.Fatalf("%d bytes: completed %d times", len(want.Data), done)
		}
		if !bytes.Equal(got.Data, want.Data) || got.CSD1 != want.CSD1 || got.CSD3 != want.CSD3 {
			t.Errorf("%d bytes: got %q, want %q", len(want.Data), got, want)
		}
	}
}

func TestFramesLayout(t *testing.T) {
	// A node info reply is 129 bytes: padded to 140, frames 0 to 4.
	reply := Command{CSD1: csd("**********N0GW"), CSD2: csd("N0GW"), CSD3: csd("12345          12345"),
		Data: command(0x87, [3]byte{0x5D, 0x51, 0x5F}, string(append([]byte{0x26}, bytes.Repeat([]byte(" "), 122)...)))}
	if len(reply.Data) != 129 || !IsReply(reply.Data) {
		t.Fatalf("test reply: %d bytes", len(reply.Data))
	}
	frames := Frames(reply)
	if len(frames) != 7 {
		t.Fatalf("%d frames, want 7 (header, 0-4, terminator)", len(frames))
	}
	for i, f := range frames {
		fich, err := ysf.DecodeFICH(f)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if fich.DT != ysf.DTDataFRMode || fich.FT != 4 {
			t.Errorf("frame %d: DT %d FT %d", i, fich.DT, fich.FT)
		}
	}
}

func TestLongReplyBlocks(t *testing.T) {
	// A room list is 1031 bytes, from WIRES-X and YSFGateway alike: over 260
	// goes in blocks. As in YSFGateway, padding to whole frames runs into a
	// fifth block, whose BN (two bits) wraps to 0 under a BT of 3.
	data := command(0x88, [3]byte{0x5D, 0x46, 0x5F}, string(bytes.Repeat([]byte("r"), 1025)))
	if len(data) != 1031 {
		t.Fatalf("test reply: %d bytes", len(data))
	}
	frames := Frames(Command{CSD1: blank, CSD2: blank, CSD3: blank, Data: data})
	last, _ := ysf.DecodeFICH(frames[len(frames)-2])
	if last.BT != 3 || last.BN != 0 || len(frames) != 2+4*8+3 {
		t.Errorf("last frame BT %d BN %d, %d frames; want 3, 0 (block 4), 37", last.BT, last.BN, len(frames))
	}
	// Reassemble as a radio would: block 0 carries 20 + 6*40 bytes, later
	// blocks a 00 and 19 bytes in frame 1, then 6*40.
	var got []byte
	for _, f := range frames[1 : len(frames)-1] {
		fich, _ := ysf.DecodeFICH(f)
		d1, d2, _, _ := ysf.DecodeHeader(f)
		switch {
		case fich.FN == 0:
		case fich.FN == 1 && fich.BN == 0:
			got = append(got, d2[:]...)
		case fich.FN == 1:
			if d2[0] != 0 {
				t.Errorf("block %d starts with %02x", fich.BN, d2[0])
			}
			got = append(got, d2[1:]...)
		default:
			got = append(got, d1[:]...)
			got = append(got, d2[:]...)
		}
	}
	if !bytes.HasPrefix(got, data) {
		t.Errorf("reassembled reply differs")
	}
}

func TestHRICommandMatchesCapture(t *testing.T) {
	data := command(0x7F, DXRequest, "3          ")
	p := hri.CommandPayload(csd("*****12345N0CALL"), blank, csd("               12345"), data)
	if p[:8] != "2100107F" || len(p) != 0x66 {
		t.Errorf("payload %q, length %#x; want prefix 2100107F, length 0x66", p, len(p))
	}
	m, _ := hri.Parse(hri.D1('F', "20211000"+string(bytes.Repeat([]byte(" "), 60))+hex.EncodeToString(data)))
	c, ok := hri.ParseCommand(m)
	if !ok || !bytes.Equal(c.Data, data) || c.Prefix != "20211000" {
		t.Errorf("ParseCommand = %v, %v", c, ok)
	}
}
