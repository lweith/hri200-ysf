// ax25-door runs a text program, such as a game, for every station that
// connects to its callsign over AX.25, through direwolf's AGW port.
//
//	ax25-door -agw 127.0.0.1:8000 -call KN6FLN-11 /usr/games/wump
//
// or several doors on one AGW connection (direwolf 1.7 takes only three AGW
// clients), each its own callsign:
//
//	ax25-door -door "KN6FLN-11 /usr/games/wump" -door "KN6FLN-12 /usr/games/trek" \
//		-banner "KN6FLN-12 Welcome aboard."
//
// Each connection gets its own copy of the program on a pty with echo off: on
// the air, echoing what the station sent would only send it back. The
// program's output is gathered until it pauses and sent as few frames as
// possible, since every transmission costs a key-up (on the HRI-200's radio,
// a switch to Analog and back). Line endings become CR, as AX.25 terminals
// expect, and terminal escape sequences are dropped. A station's CR is the
// program's newline.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lweith/hri200-ysf/internal/agw"
)

// multi is a repeatable flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ", ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

var (
	agwAddr = flag.String("agw", "127.0.0.1:8000", "direwolf's AGW port")
	call    = flag.String("call", "", "callsign-SSID to answer connections to, running PROGRAM")
	maxConn = flag.Int("max", 2, "most connections at once, over all doors")
	idle    = flag.Duration("idle", 15*time.Minute, "disconnect a station silent this long")
	gather  = flag.Duration("gather", 600*time.Millisecond, "send output once the program has been quiet this long")
	paclen  = flag.Int("paclen", 200, "most bytes of data per frame")
	doorFl  multi
	banners multi
)

func init() {
	flag.Var(&doorFl, "door", `"CALL-SSID PROGRAM [ARGS...]": another door (repeatable)`)
	flag.Var(&banners, "banner", `sent before the program's own output; "CALL-SSID TEXT" for one door's (repeatable)`)
}

// door is one callsign and the program it runs.
type door struct {
	call   string
	argv   []string
	banner string
}

type session struct {
	door   *door
	remote string
	pty    *os.File
	cmd    *exec.Cmd
	input  chan []byte
	last   time.Time
	// replaced: a reconnection took over (under mu); this session ends
	// without touching the new one's link.
	replaced bool
}

var (
	mu       sync.Mutex
	doors    = map[string]*door{}
	sessions = map[string]*session{} // door call + "<" + remote
	conn     *agw.Conn
)

// parseDoors builds the doors from -call and PROGRAM, and -door. A banner
// whose first word is a door's callsign is that door's; any other is the
// -call door's (or, with only one door, that one's).
func parseDoors(single string, args []string, extra, banners []string) (map[string]*door, error) {
	ds := map[string]*door{}
	var first *door
	add := func(c string, argv []string) error {
		c = strings.ToUpper(c)
		if c == "" || len(argv) == 0 {
			return fmt.Errorf("a door needs a callsign and a program")
		}
		if ds[c] != nil {
			return fmt.Errorf("%s: two doors", c)
		}
		ds[c] = &door{call: c, argv: argv}
		if first == nil {
			first = ds[c]
		}
		return nil
	}
	if single != "" || len(args) > 0 {
		if err := add(single, args); err != nil {
			return nil, err
		}
	}
	for _, d := range extra {
		f := strings.Fields(d)
		if len(f) < 2 {
			return nil, fmt.Errorf("-door %q: want CALL-SSID PROGRAM [ARGS...]", d)
		}
		if err := add(f[0], f[1:]); err != nil {
			return nil, err
		}
	}
	if len(ds) == 0 {
		return nil, fmt.Errorf("no doors")
	}
	for _, b := range banners {
		w, rest, _ := strings.Cut(b, " ")
		if d := ds[strings.ToUpper(w)]; d != nil {
			d.banner = rest
		} else if single != "" || len(ds) == 1 {
			first.banner = b
		} else {
			return nil, fmt.Errorf("-banner %q: names no door", b)
		}
	}
	return ds, nil
}

func main() {
	flag.Parse()
	ds, err := parseDoors(*call, flag.Args(), doorFl, banners)
	if err != nil {
		log.Fatalf("%v\nusage: ax25-door -call CALL-SSID [flags] PROGRAM [ARGS...], or -door \"CALL-SSID PROGRAM [ARGS...]\"...", err)
	}
	doors = ds
	for {
		if err := serve(); err != nil {
			log.Printf("agw: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func serve() error {
	c, err := agw.Dial(*agwAddr)
	if err != nil {
		return err
	}
	defer c.Close()
	mu.Lock()
	conn = c
	mu.Unlock()
	for _, d := range doors {
		if err := c.Register(d.call); err != nil {
			return err
		}
	}
	for {
		f, err := c.Recv()
		if err != nil {
			mu.Lock()
			for _, s := range sessions {
				s.cmd.Process.Kill()
			}
			mu.Unlock()
			return err
		}
		d := doors[strings.ToUpper(f.To)]
		switch f.Kind {
		case 'X':
			if len(f.Data) == 1 && f.Data[0] == 1 {
				log.Printf("answering %s", f.From)
			} else {
				return fmt.Errorf("direwolf refused to register %s", f.From)
			}
		case 'C':
			if d != nil {
				log.Printf("%s: %s", f.From, agw.Text(f))
				open(d, f.From)
			}
		case 'D':
			if d != nil {
				mu.Lock()
				s := sessions[d.call+"<"+f.From]
				mu.Unlock()
				if s != nil {
					s.input <- f.Data
				}
			}
		case 'd':
			// direwolf's disconnect names the station and our callsign, in
			// either order.
			log.Printf("%s: %s", f.From, agw.Text(f))
			mu.Lock()
			for _, k := range []string{strings.ToUpper(f.To) + "<" + f.From, strings.ToUpper(f.From) + "<" + f.To} {
				if s := sessions[k]; s != nil {
					s.cmd.Process.Kill()
				}
			}
			mu.Unlock()
		}
	}
}

func send(d *door, remote string, data []byte) {
	mu.Lock()
	c := conn
	mu.Unlock()
	for len(data) > 0 {
		n := min(len(data), *paclen)
		if err := c.Data(d.call, remote, data[:n]); err != nil {
			log.Printf("%s: %v", remote, err)
			return
		}
		data = data[n:]
	}
}

func open(d *door, remote string) {
	mu.Lock()
	defer mu.Unlock()
	key := d.call + "<" + remote
	if old := sessions[key]; old != nil {
		// The station connected again (direwolf reports a SABME on an open
		// link the same way): it lost track, typically of our replies, and
		// starts over. So does the program, so it gets the banner again.
		log.Printf("%s: %s reconnected, restarting %s", remote, d.call, d.argv[0])
		old.replaced = true
		old.cmd.Process.Kill()
		delete(sessions, key)
	}
	if len(sessions) >= *maxConn {
		c := conn
		go func() {
			send(d, remote, []byte("Busy, try later.\r"))
			c.Disconnect(d.call, remote)
		}()
		return
	}
	m, slave, err := openPTY()
	if err != nil {
		log.Printf("%s: pty: %v", remote, err)
		return
	}
	cmd := exec.Command(d.argv[0], d.argv[1:]...)
	cmd.Env = []string{"PATH=/usr/games:/usr/bin:/bin", "TERM=dumb", "PAGER=cat", "HOME=/tmp",
		"COLUMNS=80", "LINES=24", "AX25_CALLSIGN=" + remote}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		log.Printf("%s: %v", remote, err)
		m.Close()
		slave.Close()
		return
	}
	slave.Close()
	s := &session{door: d, remote: remote, pty: m, cmd: cmd, input: make(chan []byte, 16), last: time.Now()}
	sessions[key] = s
	log.Printf("%s: %s started %s (pid %d)", remote, d.call, d.argv[0], cmd.Process.Pid)
	go s.run()
}

func (s *session) run() {
	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		buf := make([]byte, 4096)
		for {
			n, err := s.pty.Read(buf)
			if n > 0 {
				out <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	var pending []byte
	if s.door.banner != "" {
		pending = append(pending, s.door.banner+"\r"...)
	}
	var quiet <-chan time.Time
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	flush := func() {
		if len(pending) > 0 {
			send(s.door, s.remote, pending)
			pending = nil
		}
	}
loop:
	for {
		select {
		case b, ok := <-out:
			if !ok {
				break loop
			}
			pending = append(pending, forAir(b)...)
			if len(pending) >= 4*(*paclen) {
				flush()
			}
			quiet = time.After(*gather)
		case <-quiet:
			flush()
		case b := <-s.input:
			s.last = time.Now()
			if _, err := s.pty.Write(fromAir(b)); err != nil {
				break loop
			}
		case now := <-tick.C:
			if now.Sub(s.last) > *idle {
				pending = append(pending, "\rIdle too long, goodbye.\r"...)
				break loop
			}
		}
	}
	mu.Lock()
	replaced := s.replaced
	mu.Unlock()
	if !replaced {
		flush()
	}
	s.cmd.Process.Kill()
	s.cmd.Wait()
	s.pty.Close()
	if replaced {
		return
	}
	mu.Lock()
	delete(sessions, s.door.call+"<"+s.remote)
	c := conn
	mu.Unlock()
	log.Printf("%s: %s program ended, disconnecting", s.remote, s.door.call)
	c.Disconnect(s.door.call, s.remote)
}

// forAir makes program output fit for an AX.25 terminal: CR line endings, no
// escape sequences or other control characters but tab and bell.
func forAir(b []byte) []byte {
	var o bytes.Buffer
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b: // ESC [ ... final byte, or ESC and one more
			if i+1 < len(b) && b[i+1] == '[' {
				i += 2
				for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
					i++
				}
			} else {
				i++
			}
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			o.WriteByte('\r')
		case c == '\n':
			o.WriteByte('\r')
		case c == '\t' || c == 7 || c >= 0x20 && c != 0x7f:
			o.WriteByte(c)
		}
	}
	return o.Bytes()
}

// fromAir makes a station's lines the program's: CR (or CR LF, or LF) ends a
// line.
func fromAir(b []byte) []byte {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return []byte(strings.ReplaceAll(s, "\r", "\n"))
}

// openPTY opens a pty pair with echo off and no output translation.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	t, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err == nil {
		t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ECHOCTL
		t.Oflag &^= unix.ONLCR
		t.Iflag &^= unix.ICRNL
		err = unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, t)
	}
	if err != nil {
		master.Close()
		slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
