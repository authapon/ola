//go:build linux

// platform_linux.go - Linux-specific terminal and process-group helpers,
// merged from term_linux.go and proc_linux.go as part of a file-count
// cleanup; nothing about the logic changed, only its location. Kept
// separate from main.go (rather than folded in with a build tag inside
// that file) because a build-tag file must contain *only* the
// build-tagged code - see platform_other.go for the non-Linux counterpart
// that provides the same function signatures.

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// isRealTerminal reports whether f is attached to an actual interactive
// terminal (tty/pty). This is deliberately stricter than checking
// os.ModeCharDevice: /dev/null - a common redirect target in cron jobs and
// scripts ("ola ask ... < /dev/null") - is also a character device, so that
// check alone would wrongly treat a non-interactive run as interactive and
// let ask_user block forever waiting for input that will never arrive.
// ioctl(TCGETS) only succeeds on a real tty/pty.
func isRealTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

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
