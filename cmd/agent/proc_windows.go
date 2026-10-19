//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	stillActive            = 259
	queryLimitedInfo       = 0x1000
)

// detach starts the background agent with no console and in its own process
// group, so closing the terminal or a Ctrl-C there does not reach it.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob}
}

// startDetached starts c, also out of the job object that npm's launcher
// (libuv) puts its children in, which would end the agent when the launcher
// exits; where the job forbids that, it starts a copy inside it.
func startDetached(c *exec.Cmd) (*exec.Cmd, error) {
	if err := c.Start(); err == nil || c.Process != nil {
		return c, err
	}
	retry := exec.Command(c.Path)
	retry.Args, retry.Env, retry.Dir = c.Args, c.Env, c.Dir
	retry.Stdin, retry.Stdout, retry.Stderr = c.Stdin, c.Stdout, c.Stderr
	retry.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess | createNewProcessGroup}
	return retry, retry.Start()
}

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(queryLimitedInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
