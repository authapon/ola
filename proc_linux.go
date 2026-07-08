//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup puts cmd in its own process group before it starts, so
// killProcessGroup can later take down not just the "sh -c ..." wrapper but
// anything it spawned (e.g. a test runner forking worker processes). A
// plain cmd.Process.Kill() only signals the direct child; a build/test
// command that hangs in a grandchild would survive that and defeat
// run_command's --cmd-timeout entirely.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the whole process group started by
// setupProcessGroup (negative PID = process group in the kill(2) syscall).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
