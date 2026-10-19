//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procGetMode      = kernel32.NewProc("GetConsoleMode")
	procSetMode      = kernel32.NewProc("SetConsoleMode")
	procScreenBuffer = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

// termWidth is the console's visible width in columns, or 0 when unknown.
func termWidth(f *os.File) int {
	var info struct {
		size, cursor             struct{ x, y int16 }
		attr                     uint16
		left, top, right, bottom int16
		maxSize                  struct{ x, y int16 }
	}
	if r, _, _ := procScreenBuffer.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0
	}
	return int(info.right-info.left) + 1
}

// enableColor turns on ANSI escape processing (Windows 10 and later); an
// older console gets the banner without colour.
func enableColor(f *os.File) bool {
	const vtProcessing = 0x0004
	var mode uint32
	if r, _, _ := procGetMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	if mode&vtProcessing != 0 {
		return true
	}
	r, _, _ := procSetMode.Call(f.Fd(), uintptr(mode|vtProcessing))
	return r != 0
}

// isTTY reports whether f is a console.
func isTTY(f *os.File) bool {
	var mode uint32
	r, _, _ := procGetMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
