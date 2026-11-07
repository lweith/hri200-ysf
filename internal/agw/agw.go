// Package agw speaks the AGWPE TCP protocol to direwolf, enough for AX.25
// connected mode: register a callsign, connect, send and receive connected
// data, disconnect.
//
// A frame is a 36-byte header (port, kind, PID, from and to callsigns of up to
// 10 bytes, data length, all little-endian) and the data. direwolf (1.7,
// server.c) reports a connection with kind 'C' ("*** CONNECTED To Station X"
// for one that X made to a registered callsign, "... With Station X" for one
// this client made), connected data with 'D' and a disconnection with 'd'.
// From and To are as the frame travels: for what direwolf reports, From is
// the remote station.
package agw

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

const headerSize = 36

// Frame is one AGW frame.
type Frame struct {
	Port     byte
	Kind     byte
	PID      byte
	From, To string
	Data     []byte
}

// Conn is a connection to direwolf's AGW port.
type Conn struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex // writes
}

// Dial connects to an AGW server such as "127.0.0.1:8000".
func Dial(addr string) (*Conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c, r: bufio.NewReader(c)}, nil
}

func (c *Conn) Close() error { return c.c.Close() }

func putCall(b []byte, call string) {
	copy(b[:10], strings.ToUpper(call))
}

func getCall(b []byte) string {
	s := string(b[:10])
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Send writes a frame.
func (c *Conn) Send(f Frame) error {
	b := make([]byte, headerSize+len(f.Data))
	b[0], b[4], b[6] = f.Port, f.Kind, f.PID
	putCall(b[8:], f.From)
	putCall(b[18:], f.To)
	binary.LittleEndian.PutUint32(b[28:], uint32(len(f.Data)))
	copy(b[headerSize:], f.Data)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.c.Write(b)
	return err
}

// Recv reads a frame.
func (c *Conn) Recv() (Frame, error) {
	h := make([]byte, headerSize)
	if _, err := io.ReadFull(c.r, h); err != nil {
		return Frame{}, err
	}
	n := binary.LittleEndian.Uint32(h[28:])
	if n > 1<<16 {
		return Frame{}, fmt.Errorf("agw: %d-byte frame", n)
	}
	f := Frame{Port: h[0], Kind: h[4], PID: h[6], From: getCall(h[8:]), To: getCall(h[18:]),
		Data: make([]byte, n)}
	if _, err := io.ReadFull(c.r, f.Data); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// Register asks direwolf to accept connections to call. Its answer comes back
// as an 'X' frame whose one data byte is 1 for success.
func (c *Conn) Register(call string) error {
	return c.Send(Frame{Kind: 'X', From: call})
}

// Connect asks direwolf to connect from one of its callsigns to a station.
func (c *Conn) Connect(from, to string) error {
	return c.Send(Frame{Kind: 'C', PID: 0xF0, From: from, To: to})
}

// Data sends connected data.
func (c *Conn) Data(from, to string, data []byte) error {
	return c.Send(Frame{Kind: 'D', PID: 0xF0, From: from, To: to, Data: data})
}

// Disconnect ends a connection.
func (c *Conn) Disconnect(from, to string) error {
	return c.Send(Frame{Kind: 'd', From: from, To: to})
}

// Text is a status frame's text without its line ending and padding.
func Text(f Frame) string {
	return strings.TrimRight(string(f.Data), "\r\n\x00")
}
