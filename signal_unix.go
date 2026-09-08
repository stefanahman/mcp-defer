//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// The backend gets its own process group, so that stopping it also
// stops whatever a wrapper script is waiting on: a credential prompt,
// a sleep, the real server it has not exec'ed yet. A shell defers a
// signal until its foreground child returns; the group does not.

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// shutdownSignals delivers the signals that end the proxy, so it can
// end the backend first, and keeps a write to a gone client from
// ending the proxy through SIGPIPE: the write fails, stdin ends next.
func shutdownSignals() <-chan os.Signal {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return c
}

func interrupt(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGINT) }

func kill(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGKILL) }

// signalGroup signals the process first through os.Process, which
// refuses once the process has been waited for, and only then its
// group: a reaped pid may already belong to someone else.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if err := cmd.Process.Signal(sig); err != nil {
		return err
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
