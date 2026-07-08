//go:build !linux

package main

import "os"

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
