package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lweith/hri200-ysf/internal/bridge"
	"github.com/lweith/hri200-ysf/internal/hri"
	"github.com/lweith/hri200-ysf/internal/serial"
)

// openPTY returns a pty's master and its slave's path.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no ptys: %v", err)
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	// Raw on the master side too, so our test bytes aren't echoed or translated.
	tio, _ := unix.IoctlGetTermios(fd, unix.TCGETS)
	tio.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	tio.Iflag &^= unix.ICRNL | unix.IXON
	tio.Oflag &^= unix.OPOST
	_ = unix.IoctlSetTermios(fd, unix.TCSETS, tio)
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

// expect reads messages from a pty master until one equals want.
func expect(t *testing.T, r *os.File, want string) {
	t.Helper()
	got := make(chan string, 16)
	go func() {
		var s hri.Splitter
		buf := make([]byte, 512)
		for {
			n, err := r.Read(buf)
			for _, m := range s.Write(buf[:n]) {
				got <- m
				if m == want {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-got:
			if m == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestProxySwapsHRI200(t *testing.T) {
	wiresx, downPath := openPTY(t)
	defer wiresx.Close()
	hri200, upPath := openPTY(t)

	var attached atomic.Bool
	attached.Store(true)
	cache, _ := LoadCache(filepath.Join(t.TempDir(), "cache.json"))
	p := &Proxy{
		Downstream: downPath,
		Upstream: func() (string, error) {
			if !attached.Load() {
				return "", errors.New("unplugged")
			}
			return upPath, nil
		},
		Open:   serial.Open,
		Cache:  cache,
		Bridge: &bridge.Bridge{},
		Log:    t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, make(chan YSF))
	time.Sleep(300 * time.Millisecond) // let it open both ttys

	// Attached: the poll goes through, the HRI-200 answers, the answer comes back.
	wiresx.Write(hri.Wrap("P010000"))
	expect(t, hri200, "P010000")
	hri200.Write(hri.Wrap("B0 0    0000000"))
	expect(t, wiresx, "B0 0    0000000")
	wiresx.Write(hri.Wrap("R6423"))
	expect(t, hri200, "R6423")
	hri200.Write(hri.Wrap("R0303030"))
	expect(t, wiresx, "R0303030")

	// Unplug: the proxy answers WIRES-X from what it learned.
	attached.Store(false)
	hri200.Close()
	time.Sleep(300 * time.Millisecond)
	wiresx.Write(hri.Wrap("R6423"))
	expect(t, wiresx, "R0303030")
	wiresx.Write(hri.Wrap("P010000"))
	expect(t, wiresx, "B0 0    0000000")
	wiresx.Write(hri.Wrap("D1C0000"))
	expect(t, wiresx, "D1C000B04000R0OR40")
}
