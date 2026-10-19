//go:build !unix && !windows

package main

import (
	"errors"
	"os/exec"
)

func detach(*exec.Cmd) {}

func startDetached(c *exec.Cmd) (*exec.Cmd, error) { return c, c.Start() }

func processAlive(int) bool { return false }

func killProcess(int) error { return errors.New("not supported on this platform") }
