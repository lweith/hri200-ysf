package proxy

import (
	"encoding/hex"
	"strings"
)

// WIRES-X knows the HRI-200 by its answers to what it asks on startup: M10,
// R6423 (the HRI-200's serial number and the date it was made), D1V0000 (the
// radio and its firmware) and the echoes of its settings writes. The cache
// keeps the real radio's answers, so the face gives them with no HRI-200
// attached; a settings write it has no echo for is echoed as the radio would,
// over the settings the radio last echoed.

// Serial is the HRI-200's serial number and the date it was made, from its
// R6423 answer: "R0", then the hex of "00000,00000,SERIAL,YYYYMMDDhhmmss".
func Serial(r6423 string) (serial, made string, ok bool) {
	if !strings.HasPrefix(r6423, "R0") {
		return "", "", false
	}
	b, err := hex.DecodeString(r6423[2:])
	if err != nil {
		return "", "", false
	}
	f := strings.Split(string(b), ",")
	if len(f) != 4 || len(f[3]) != 14 {
		return "", "", false
	}
	d := f[3]
	return f[2], d[:4] + "-" + d[4:6] + "-" + d[6:8] + " " + d[8:10] + ":" + d[10:12] + ":" + d[12:], true
}

// Describe names the HRI-200 and radio the cache knows, for the log.
func (c *Cache) Describe() string {
	r, _ := c.Reply("R6423")
	serial, made, ok := Serial(r)
	if !ok {
		return "no HRI-200 known yet"
	}
	s := "HRI-200 serial " + serial + " (made " + made + ")"
	if v, _ := c.Reply("D1V0000"); len(v) > 7 {
		s += ", " + strings.Join(strings.Fields(v[7:min(len(v), 35)]), " ")
	}
	return s
}

// The settings echo, as the DR-1X makes it (every write/echo pair in
// tmra's cache, 2026-11-14): the settings it has, with every character of the
// write that isn't F put over them, the callsign (10 characters) unless it is
// blank; but at offset 70 the radio's own value, whatever was written.
const (
	settingsCallAt   = 76
	settingsCallLen  = 10
	settingsReadOnly = 70
)

// settingsEcho is the radio's echo of a settings write, with the settings cur.
func settingsEcho(cur, write string) string {
	if len(write) != len(cur) || write[:7] != cur[:7] {
		return cur
	}
	b := []byte(cur)
	call := write[settingsCallAt : settingsCallAt+settingsCallLen]
	for i := 7; i < len(b); i++ {
		switch {
		case i == settingsReadOnly, write[i] == 'F':
		case i >= settingsCallAt && i < settingsCallAt+settingsCallLen:
			if strings.TrimSpace(call) != "" {
				b[i] = write[i]
			}
		default:
			b[i] = write[i]
		}
	}
	return string(b)
}
