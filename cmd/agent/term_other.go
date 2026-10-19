//go:build !linux && !darwin && !windows

package main

import "os"

func termWidth(*os.File) int { return 0 }

func enableColor(*os.File) bool { return true }

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
