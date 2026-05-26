//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

func postStart(_ *exec.Cmd) (func(), error) {
	return func() {}, nil
}
