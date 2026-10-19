//go:build linux || darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// termWidth is the terminal's width in columns, or 0 when unknown.
func termWidth(f *os.File) int {
	var ws struct{ rows, cols, x, y uint16 }
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return 0
	}
	return int(ws.cols)
}

// enableColor reports whether the terminal takes ANSI colours: every Unix
// terminal does.
func enableColor(*os.File) bool { return true }

// isTTY reports whether f is a terminal.
func isTTY(f *os.File) bool {
	var ws struct{ rows, cols, x, y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	return errno == 0
}
