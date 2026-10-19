//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// detach puts the background agent in its own session, so it outlives the
// terminal that started it and a Ctrl-C there does not reach it.
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func startDetached(c *exec.Cmd) (*exec.Cmd, error) { return c, c.Start() }

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
