// ax25-term is a line-mode AX.25 connected-mode terminal on direwolf's AGW
// port: it connects to a station, sends each line typed as one frame (ending
// in CR), and prints what comes back. EOF (^D) disconnects.
//
//	ax25-term -agw 127.0.0.1:8000 -mycall KN6FLN KN6FLN-11
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/lweith/hri200-ysf/internal/agw"
)

var (
	agwAddr = flag.String("agw", "127.0.0.1:8000", "direwolf's AGW port")
	mycall  = flag.String("mycall", "", "this station's callsign-SSID")
)

func main() {
	flag.Parse()
	if *mycall == "" || flag.NArg() != 1 {
		log.Fatal("usage: ax25-term -mycall CALL[-SSID] [-agw HOST:PORT] REMOTE[-SSID]")
	}
	me, remote := strings.ToUpper(*mycall), strings.ToUpper(flag.Arg(0))
	c, err := agw.Dial(*agwAddr)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	// direwolf only connects from a callsign a client has registered.
	if err := c.Register(me); err != nil {
		log.Fatal(err)
	}
	if err := c.Connect(me, remote); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "*** connecting %s to %s...\n", me, remote)

	connected := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := c.Recv()
			if err != nil {
				fmt.Fprintf(os.Stderr, "*** %v\n", err)
				return
			}
			if !strings.EqualFold(f.From, remote) && f.Kind != 'X' {
				continue
			}
			switch f.Kind {
			case 'X':
				if len(f.Data) != 1 || f.Data[0] != 1 {
					fmt.Fprintf(os.Stderr, "*** direwolf refused to register %s\n", me)
					return
				}
			case 'C':
				fmt.Fprintf(os.Stderr, "%s\n", agw.Text(f))
				select {
				case connected <- true:
				default:
				}
			case 'D':
				os.Stdout.WriteString(strings.ReplaceAll(string(f.Data), "\r", "\n"))
			case 'd':
				fmt.Fprintf(os.Stderr, "%s\n", agw.Text(f))
				return
			}
		}
	}()

	select {
	case <-connected:
	case <-done:
		os.Exit(1)
	}
	lines := make(chan string)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			lines <- in.Text()
		}
		close(lines)
	}()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				fmt.Fprintf(os.Stderr, "*** disconnecting\n")
				c.Disconnect(me, remote)
				<-done
				return
			}
			if err := c.Data(me, remote, []byte(l+"\r")); err != nil {
				log.Fatal(err)
			}
		case <-done:
			return
		}
	}
}
