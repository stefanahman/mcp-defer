//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the child in a session of its own, outside the process
// group mcp-defer signals.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
