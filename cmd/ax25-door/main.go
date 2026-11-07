// ax25-door runs a text program, such as a game, for every station that
// connects to its callsign over AX.25, through direwolf's AGW port.
//
//	ax25-door -agw 127.0.0.1:8000 -call KN6FLN-11 /usr/games/wump
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

var (
	agwAddr = flag.String("agw", "127.0.0.1:8000", "direwolf's AGW port")
	call    = flag.String("call", "", "callsign-SSID to answer connections to")
	maxConn = flag.Int("max", 2, "most connections at once")
	idle    = flag.Duration("idle", 15*time.Minute, "disconnect a station silent this long")
	gather  = flag.Duration("gather", 600*time.Millisecond, "send output once the program has been quiet this long")
	paclen  = flag.Int("paclen", 200, "most bytes of data per frame")
	banner  = flag.String("banner", "", "sent before the program's own output")
)

type session struct {
	remote string
	pty    *os.File
	cmd    *exec.Cmd
	input  chan []byte
	last   time.Time
}

var (
	mu       sync.Mutex
	sessions = map[string]*session{}
	conn     *agw.Conn
)

func main() {
	flag.Parse()
	if *call == "" || flag.NArg() == 0 {
		log.Fatal("usage: ax25-door -call CALL-SSID [flags] PROGRAM [ARGS...]")
	}
	*call = strings.ToUpper(*call)
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
	if err := c.Register(*call); err != nil {
		return err
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
		switch f.Kind {
		case 'X':
			if len(f.Data) == 1 && f.Data[0] == 1 {
				log.Printf("answering %s", *call)
			} else {
				return fmt.Errorf("direwolf refused to register %s", *call)
			}
		case 'C':
			if strings.EqualFold(f.To, *call) {
				log.Printf("%s: %s", f.From, agw.Text(f))
				open(f.From)
			}
		case 'D':
			if strings.EqualFold(f.To, *call) {
				mu.Lock()
				s := sessions[f.From]
				mu.Unlock()
				if s != nil {
					s.input <- f.Data
				}
			}
		case 'd':
			log.Printf("%s: %s", f.From, agw.Text(f))
			mu.Lock()
			if s := sessions[f.From]; s != nil {
				s.cmd.Process.Kill()
			}
			mu.Unlock()
		}
	}
}

func send(remote string, data []byte) {
	mu.Lock()
	c := conn
	mu.Unlock()
	for len(data) > 0 {
		n := min(len(data), *paclen)
		if err := c.Data(*call, remote, data[:n]); err != nil {
			log.Printf("%s: %v", remote, err)
			return
		}
		data = data[n:]
	}
}

func open(remote string) {
	mu.Lock()
	defer mu.Unlock()
	if sessions[remote] != nil {
		return // direwolf reports a reconnection the same way
	}
	if len(sessions) >= *maxConn {
		go func() {
			send(remote, []byte("Busy, try later.\r"))
			conn.Disconnect(*call, remote)
		}()
		return
	}
	m, slave, err := openPTY()
	if err != nil {
		log.Printf("%s: pty: %v", remote, err)
		return
	}
	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
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
	s := &session{remote: remote, pty: m, cmd: cmd, input: make(chan []byte, 16), last: time.Now()}
	sessions[remote] = s
	log.Printf("%s: started %s (pid %d)", remote, flag.Arg(0), cmd.Process.Pid)
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
	if *banner != "" {
		pending = append(pending, *banner+"\r"...)
	}
	var quiet <-chan time.Time
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	flush := func() {
		if len(pending) > 0 {
			send(s.remote, pending)
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
	flush()
	s.cmd.Process.Kill()
	s.cmd.Wait()
	s.pty.Close()
	mu.Lock()
	delete(sessions, s.remote)
	c := conn
	mu.Unlock()
	log.Printf("%s: program ended, disconnecting", s.remote)
	c.Disconnect(*call, s.remote)
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
