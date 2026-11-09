//go:build darwin || linux

package tui

import (
	"syscall"
	"unsafe"
)

// rawState is a terminal's saved settings.
type rawState struct {
	fd  int
	old syscall.Termios
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)) == nil
}

// makeRaw puts fd in raw input mode (no echo, no line buffering, no signals from keys) and keeps
// output processing so "\n" still works; restore undoes it.
func makeRaw(fd int) (*rawState, error) {
	var t syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	st := &rawState{fd: fd, old: t}
	t.Iflag &^= syscall.IXON | syscall.ICRNL | syscall.BRKINT | syscall.INPCK | syscall.ISTRIP
	t.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, ioctlSetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *rawState) restore() {
	if s != nil {
		_ = ioctl(s.fd, ioctlSetTermios, unsafe.Pointer(&s.old))
	}
}

type winsize struct{ Row, Col, X, Y uint16 }

// Size returns the terminal's columns and rows.
func Size(fd int) (cols, rows int, ok bool) {
	var ws winsize
	if ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil || ws.Col == 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}
