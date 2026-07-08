//go:build !linux

package main

import "os/exec"

// setupProcessGroup/killProcessGroup best-effort fallback for non-Linux
// builds: same rationale as term_other.go - ola's target environment is
// Linux, this only matters if ola is ever built for another OS, and on
// those it can only kill the direct child, not a full process group.
func setupProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
