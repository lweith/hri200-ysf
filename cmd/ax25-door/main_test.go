package main

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPTYDoesNotEcho(t *testing.T) {
	m, slave, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	cmd := exec.Command("/bin/sh", "-c", `printf 'Instructions? '; read x; printf 'got [%s]\n' "$x"`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	m.Write(fromAir([]byte("n\r")))
	var got []byte
	buf := make([]byte, 256)
	m.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		n, err := m.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	cmd.Wait()
	if s := string(forAir(got)); s != "Instructions? got [n]\r" {
		t.Errorf("on the air: %q", s)
	}
}

func TestForAir(t *testing.T) {
	in := "\x1b[2Jhello\r\nworld\n\x1b=tab\there\x7f\x00"
	if got := string(forAir([]byte(in))); got != "hello\rworld\rtab\there" {
		t.Errorf("forAir = %q", got)
	}
	if got := string(fromAir([]byte("a\r\nb\rc\n"))); got != "a\nb\nc\n" {
		t.Errorf("fromAir = %q", got)
	}
	if strings.Contains(string(forAir([]byte("\x1b[1;31mred\x1b[0m"))), "\x1b") {
		t.Error("left an escape")
	}
}
