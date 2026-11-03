// Package hri parses the serial protocol between WIRES-X and a Yaesu HRI-200,
// as observed on its CDC ACM port (38400 8N1).
//
// Every message is ASCII framed by SOH (0x01) and EOT (0x04). Messages to and
// from the radio side start "D1", a command letter and four hex digits of
// payload length. Binary data inside them is hex text.
package hri

import (
	"encoding/hex"
	"fmt"
	"strconv"
)

const (
	soh = 0x01
	eot = 0x04
)

// Splitter turns a byte stream into messages. Use one per direction.
type Splitter struct {
	buf []byte
	in  bool
}

// Write adds bytes and returns the complete messages, without SOH and EOT.
func (s *Splitter) Write(p []byte) []string {
	var out []string
	for _, b := range p {
		switch {
		case b == soh:
			s.buf = s.buf[:0]
			s.in = true
		case b == eot && s.in:
			out = append(out, string(s.buf))
			s.buf = s.buf[:0]
			s.in = false
		case s.in:
			if len(s.buf) < 8192 {
				s.buf = append(s.buf, b)
			}
		}
	}
	return out
}

// Message is a parsed "D1" message.
type Message struct {
	Cmd     byte   // the letter after "D1"
	Payload string // the payload text, its length checked
}

// Parse parses a "D1" message. Other messages (polls and their replies) return ok false.
func Parse(text string) (Message, bool) {
	if len(text) < 7 || text[0] != 'D' || text[1] != '1' {
		return Message{}, false
	}
	n, err := strconv.ParseUint(text[3:7], 16, 16)
	if err != nil || len(text) < 7+int(n) {
		return Message{}, false
	}
	return Message{Cmd: text[2], Payload: text[7 : 7+int(n)]}, true
}

// Voice is a D1R (received by the radio) or D1E (sent to the radio) frame: one
// YSF V/D Mode 2 frame's five voice blocks, as they are on air.
type Voice struct {
	Counter uint16 // counts 20 ms vocoder frames, steps by 5
	Blocks  [5][13]byte
}

// ParseVoice parses a D1R or D1E payload: "2", four hex digits of counter, "00",
// and 130 hex digits of voice.
func ParseVoice(m Message) (Voice, error) {
	var v Voice
	if len(m.Payload) != 137 {
		return v, fmt.Errorf("voice payload is %d characters, want 137", len(m.Payload))
	}
	c, err := strconv.ParseUint(m.Payload[1:5], 16, 16)
	if err != nil {
		return v, fmt.Errorf("voice counter: %w", err)
	}
	v.Counter = uint16(c)
	raw, err := hex.DecodeString(m.Payload[7:137])
	if err != nil {
		return v, fmt.Errorf("voice data: %w", err)
	}
	for i := range 5 {
		copy(v.Blocks[i][:], raw[13*i:13*i+13])
	}
	return v, nil
}

// CallsignBlock is a D1H message from the radio: one of the three 20-character
// callsign data blocks (CSD1 destination and source, CSD2 downlink and uplink,
// CSD3 remarks), sent more than once.
type CallsignBlock struct {
	Block  int // 1, 2 or 3
	Repeat int
	Text   [20]byte
}

// ParseCallsignBlock parses a D1H payload: eight characters, block, repeat, 20 characters.
func ParseCallsignBlock(m Message) (CallsignBlock, error) {
	var c CallsignBlock
	if len(m.Payload) != 30 {
		return c, fmt.Errorf("callsign payload is %d characters, want 30", len(m.Payload))
	}
	c.Block = int(m.Payload[8] - '0')
	c.Repeat = int(m.Payload[9] - '0')
	if c.Block < 1 || c.Block > 3 {
		return c, fmt.Errorf("callsign block %q", m.Payload[8])
	}
	copy(c.Text[:], m.Payload[10:30])
	return c, nil
}

// TxHeader is a D1F message of 0x52 characters from WIRES-X starting a
// transmission to the radio: all three callsign data blocks.
type TxHeader struct {
	CSD1, CSD2, CSD3 [20]byte
}

// ParseTxHeader parses a transmit header, or returns ok false for other D1F messages.
func ParseTxHeader(m Message) (TxHeader, bool) {
	var h TxHeader
	if m.Cmd != 'F' || len(m.Payload) != 0x52 {
		return h, false
	}
	copy(h.CSD1[:], m.Payload[8:28])
	copy(h.CSD2[:], m.Payload[28:48])
	copy(h.CSD3[:], m.Payload[48:68])
	return h, true
}
