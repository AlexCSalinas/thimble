// Package rawterm puts the controlling terminal into raw mode so keystrokes
// reach the guest console byte-for-byte. It is the ~40 lines of golang.org/x/term
// we actually need, written against syscall so the module stays on the
// standard library.
package rawterm

import (
	"os"
	"syscall"
	"unsafe"
)

type State struct {
	fd      int
	termios syscall.Termios
}

func ioctl(fd int, req uint64, t *syscall.Termios) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(t)))
	if e != 0 {
		return e
	}
	return nil
}

// IsTerminal reports whether f is a tty.
func IsTerminal(f *os.File) bool {
	var t syscall.Termios
	return ioctl(int(f.Fd()), syscall.TIOCGETA, &t) == nil
}

// MakeRaw mirrors cfmakeraw(3). Restore with State.Restore.
func MakeRaw(f *os.File) (*State, error) {
	fd := int(f.Fd())
	var old syscall.Termios
	if err := ioctl(fd, syscall.TIOCGETA, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	return &State{fd: fd, termios: old}, nil
}

func (s *State) Restore() error { return ioctl(s.fd, syscall.TIOCSETA, &s.termios) }
