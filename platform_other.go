//go:build !linux

// platform_other.go - non-Linux terminal and process-group helpers, merged
// from term_other.go and proc_other.go as part of a file-count cleanup;
// nothing about the logic changed, only its location. See
// platform_linux.go for the Linux counterpart these fall back for; ola's
// target environment is Linux, so this file only matters if ola is ever
// built for another OS.

package main

import (
	"os"
	"os/exec"
)

// isRealTerminal is a best-effort fallback for non-Linux builds. It cannot
// distinguish /dev/null from a real terminal the way the Linux
// ioctl(TCGETS)-based check can, but ola's target environment is Linux
// (see the fish-shell/Ollama/local-LLM workflow this tool is built for);
// this fallback only matters if ola is ever built for another OS.
func isRealTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// setupProcessGroup/killProcessGroup best-effort fallback for non-Linux
// builds: same rationale as isRealTerminal above - ola's target
// environment is Linux, this only matters if ola is ever built for
// another OS, and on those it can only kill the direct child, not a full
// process group.
func setupProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
