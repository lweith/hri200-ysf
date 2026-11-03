// Package serial opens tty devices raw and finds the tty of a USB device.
package serial

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Open opens a tty raw at 38400 8N1, the HRI-200's settings. The descriptor is
// non-blocking so that Go's poller can interrupt reads when it is closed.
func Open(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%s: get termios: %w", path, err)
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | unix.B38400
	t.Ispeed, t.Ospeed = unix.B38400, unix.B38400
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%s: set termios: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// FindTTY returns the tty of the USB device with this vendor and product ID
// (lowercase hex, as in sysfs), such as /dev/ttyACM0. Devices on a dummy_hcd
// bus are skipped: the proxy's own gadget carries the same IDs.
func FindTTY(vendor, product string) (string, error) {
	devs, _ := filepath.Glob("/sys/bus/usb/devices/*")
	for _, d := range devs {
		if read(d, "idVendor") != vendor || read(d, "idProduct") != product {
			continue
		}
		if real, err := filepath.EvalSymlinks(d); err != nil || strings.Contains(real, "dummy_hcd") {
			continue
		}
		ttys, _ := filepath.Glob(filepath.Join(d, "*:*", "tty", "*"))
		if len(ttys) > 0 {
			return "/dev/" + filepath.Base(ttys[0]), nil
		}
	}
	return "", fmt.Errorf("no tty for USB device %s:%s", vendor, product)
}

func read(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(b))
}
