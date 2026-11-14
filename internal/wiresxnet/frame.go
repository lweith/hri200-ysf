// Package wiresxnet speaks the WIRES-X node-to-Yaesu network protocol: the
// plaintext TCP that the Windows WIRES-X.exe uses to register a node, pull the
// global node/room directory, and connect to and from rooms. It exists so a
// headless daemon can run the W8HHF node in place of that GUI, cache the
// directory, export stats, and bridge to YSF.
//
// The protocol was reverse-engineered from captures of the real node (node
// 30401, callsign W8HHF-RPT); see captures/2026-11-13/wx-login.pcapng. There is
// no TLS and no encryption. Everything is 7-bit ASCII with a handful of control
// bytes.
//
// # Framing
//
// Every message on every channel is one frame:
//
//	STX(0x02)  len(4 hex, uppercase)  body  sum(2 hex)  ETX(0x03)
//
// len is the body length in bytes. sum is the low 8 bits of the byte-wise sum
// of the body, as two hex digits. Because the length is carried up front, STX
// and ETX bytes that appear inside a binary voice body never confuse framing:
// a reader takes exactly len body bytes and does not scan for ETX. (A separate
// DLE(0x10) escape appears inside binary voice payloads; it is a property of
// that payload's content, not of the frame, and is handled where voice records
// are decoded, not here.)
//
// # Bodies
//
// A body is a type letter, a 4-hex inner length, a 2-character verb, and then
// fields. See message.go for the body grammar and the known verbs.
package wiresxnet

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Frame control bytes.
const (
	stx = 0x02
	etx = 0x03
	dle = 0x10 // escape inside binary voice payloads; not used by the frame layer
)

// ErrBadFrame reports a frame whose envelope or checksum did not hold. It is
// returned with context (what failed) so a caller can log the offending bytes.
var ErrBadFrame = errors.New("wiresxnet: bad frame")

// Frame is one message's body: everything between the length and the checksum.
// Encode and the reader handle the envelope, so callers work in bodies.
type Frame struct {
	Body []byte
}

// bodySum is the low 8 bits of the byte-wise sum of b, the checksum the wire
// carries after the body.
func bodySum(b []byte) byte {
	var s byte
	for _, v := range b {
		s += v
	}
	return s
}

// Encode renders f as wire bytes: STX, 4 hex length, body, 2 hex sum, ETX.
// Lengths are uppercase hex, as the node and server both send.
func (f Frame) Encode() []byte {
	n := len(f.Body)
	out := make([]byte, 0, n+7)
	out = append(out, stx)
	out = append(out, fmt.Sprintf("%04X", n)...)
	out = append(out, f.Body...)
	out = append(out, fmt.Sprintf("%02X", bodySum(f.Body))...)
	out = append(out, etx)
	return out
}

// Reader reads frames from a stream, reassembling across TCP segment
// boundaries. It is not safe for concurrent use.
type Reader struct {
	r *bufio.Reader
}

// NewReader wraps r to read frames.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReader(r)}
}

// ReadFrame returns the next frame. It reads to the leading STX (discarding any
// inter-frame bytes, which captures do not show but which keep a desynced
// stream recoverable), then the fixed length and sum envelope. A checksum
// mismatch is returned as ErrBadFrame with the computed and carried sums; the
// body is still returned so callers can inspect it.
func (rd *Reader) ReadFrame() (Frame, error) {
	// Seek STX.
	for {
		b, err := rd.r.ReadByte()
		if err != nil {
			return Frame{}, err
		}
		if b == stx {
			break
		}
	}
	// Length: four hex digits.
	var lenHex [4]byte
	if _, err := io.ReadFull(rd.r, lenHex[:]); err != nil {
		return Frame{}, err
	}
	n, err := hexU16(lenHex[:])
	if err != nil {
		return Frame{}, fmt.Errorf("%w: length %q: %v", ErrBadFrame, lenHex, err)
	}
	// Body, then two hex sum digits and ETX.
	body := make([]byte, n)
	if _, err := io.ReadFull(rd.r, body); err != nil {
		return Frame{}, err
	}
	var tail [3]byte // sum(2) + ETX(1)
	if _, err := io.ReadFull(rd.r, tail[:]); err != nil {
		return Frame{}, err
	}
	if tail[2] != etx {
		return Frame{}, fmt.Errorf("%w: no ETX after body, got 0x%02x", ErrBadFrame, tail[2])
	}
	want, err := hexU8(tail[:2])
	if err != nil {
		return Frame{Body: body}, fmt.Errorf("%w: sum %q: %v", ErrBadFrame, tail[:2], err)
	}
	if got := bodySum(body); got != want {
		return Frame{Body: body}, fmt.Errorf("%w: sum 0x%02x, want 0x%02x", ErrBadFrame, got, want)
	}
	return Frame{Body: body}, nil
}

// hexU16 parses four hex digits.
func hexU16(b []byte) (int, error) {
	var buf [2]byte
	if _, err := hex.Decode(buf[:], b); err != nil {
		return 0, err
	}
	return int(buf[0])<<8 | int(buf[1]), nil
}

// hexU8 parses two hex digits.
func hexU8(b []byte) (byte, error) {
	var buf [1]byte
	if _, err := hex.Decode(buf[:], b); err != nil {
		return 0, err
	}
	return buf[0], nil
}
