//go:build !unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
)

// Without process groups only the wrapper itself can be stopped.

func setProcessGroup(*exec.Cmd) {}

func shutdownSignals() <-chan os.Signal {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	return c
}

func interrupt(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func kill(cmd *exec.Cmd) error { return cmd.Process.Kill() }
