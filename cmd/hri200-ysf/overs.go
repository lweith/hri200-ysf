package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
)

// overLog keeps every transmission as a file of its YSF frames, in the -dump
// format ("seq end hex" per line), as dir/YYYY-MM-DD/HHMMSS-SOURCE.txt (UTC).
// When a new day starts, day directories older than keep days are removed.
type overLog struct {
	dir  string
	keep int
	log  func(format string, args ...any)

	f      *os.File
	failed bool // the current transmission couldn't be opened: skip it
	day    string
}

func (o *overLog) packet(p bridge.Packet, now time.Time) {
	if o.f == nil && !o.failed {
		if err := o.open(p, now); err != nil {
			o.log("over log: %v", err)
			o.failed = true
		}
	}
	if o.f != nil {
		fmt.Fprintf(o.f, "%d %t %s\n", p.Seq, p.End, hex.EncodeToString(p.Frame))
	}
	if p.End {
		if o.f != nil {
			if err := o.f.Close(); err != nil {
				o.log("over log: %v", err)
			}
		}
		o.f, o.failed = nil, false
	}
}

func (o *overLog) open(p bridge.Packet, now time.Time) error {
	now = now.UTC()
	day := now.Format(time.DateOnly)
	if day != o.day {
		o.prune(now)
		o.day = day
	}
	dir := filepath.Join(o.dir, day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := now.Format("150405") + "-" + fileName(strings.TrimRight(string(p.Source[:]), " ")) + ".txt"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	o.f = f
	return nil
}

// prune removes the day directories older than keep days.
func (o *overLog) prune(now time.Time) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return // not created yet
	}
	cutoff := now.AddDate(0, 0, -o.keep).Format(time.DateOnly)
	for _, e := range entries {
		if _, err := time.Parse(time.DateOnly, e.Name()); err != nil || !e.IsDir() {
			continue
		}
		if e.Name() < cutoff {
			if err := os.RemoveAll(filepath.Join(o.dir, e.Name())); err != nil {
				o.log("over log: %v", err)
			}
		}
	}
}

// fileName keeps letters, digits and '-' of a callsign, replacing the rest.
func fileName(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '_'
	}, s)
}
