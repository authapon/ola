//go:build linux

package main

import (
	"os"
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
