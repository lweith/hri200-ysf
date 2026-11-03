package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lweith/hri200-ysf/internal/bridge"
)

func packet(src string, seq byte, end bool) bridge.Packet {
	p := bridge.Packet{Seq: seq, End: end, Frame: []byte{seq, 0xAB}}
	copy(p.Source[:], src+"          ")
	return p
}

func TestOverLog(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "2026-01-01")
	recent := filepath.Join(dir, "2026-01-05")
	for _, d := range []string{old, recent} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	o := &overLog{dir: dir, keep: 7, log: t.Logf}
	now := time.Date(2026, 1, 10, 12, 34, 56, 0, time.UTC)

	o.packet(packet("N0CALL", 0, false), now)
	o.packet(packet("N0CALL", 1, true), now.Add(time.Second))
	o.packet(packet("N0/UP", 0, false), now.Add(2*time.Second))
	o.packet(packet("N0/UP", 1, true), now.Add(3*time.Second))

	b, err := os.ReadFile(filepath.Join(dir, "2026-01-10", "123456-N0CALL.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "0 false 00ab\n1 true 01ab\n"; got != want {
		t.Errorf("first transmission %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-01-10", "123458-N0_UP.txt")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("%s not pruned: %v", old, err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("%s pruned: %v", recent, err)
	}
}

func TestOverLogUnwritable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	o := &overLog{dir: file, keep: 7, log: func(f string, a ...any) { logged = append(logged, f) }}
	now := time.Now()
	for i := range 3 {
		o.packet(packet("N0CALL", byte(i), i == 2), now)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "over log") {
		t.Errorf("logged %q, want one error per transmission", logged)
	}
}
