package main

import (
	"os"
	"strings"
	"testing"
)

// TestStreamResponseReportsPreloadDuration confirms load_duration from the
// final NDJSON chunk is captured separately from thinking/eval time and
// surfaced in both the terminal output and the log file, so a slow first
// round (model still loading into VRAM/RAM) isn't misread as slow thinking.
func TestStreamResponseReportsPreloadDuration(t *testing.T) {
	// load_duration=2.5s, eval_duration=0.5s, no thinking content.
	chunk := `{"message":{"role":"assistant","content":"hi"},"done":true,"prompt_eval_count":10,"eval_count":5,"eval_duration":500000000,"load_duration":2500000000}` + "\n"

	outFile, err := os.CreateTemp(t.TempDir(), "stream-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamResponse(strings.NewReader(chunk), outFile, "", "", "", "")

	if out.LoadDurationNS != 2500000000 {
		t.Fatalf("expected LoadDurationNS to be captured as 2.5s (2500000000ns), got %d", out.LoadDurationNS)
	}
	if out.Content != "hi" {
		t.Fatalf("expected content to still be parsed normally, got %q", out.Content)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "preload") {
		t.Fatalf("expected the log to mention preload time, got:\n%s", logged)
	}
	if !strings.Contains(string(logged), "2.5s") {
		t.Fatalf("expected the log to report ~2.5s preload time, got:\n%s", logged)
	}
}

// TestStreamResponseNoPreloadLineWhenZero confirms a warm model (the common
// case after the first round) doesn't print a spurious "preload: 0.0s" line
// that would just be noise on every subsequent round.
func TestStreamResponseNoPreloadLineWhenZero(t *testing.T) {
	chunk := `{"message":{"role":"assistant","content":"hi"},"done":true,"eval_duration":100000000,"load_duration":0}` + "\n"

	outFile, err := os.CreateTemp(t.TempDir(), "stream-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamResponse(strings.NewReader(chunk), outFile, "", "", "", "")
	if out.LoadDurationNS != 0 {
		t.Fatalf("expected LoadDurationNS 0 for a warm model, got %d", out.LoadDurationNS)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "preload") {
		t.Fatalf("expected no preload line when load_duration is 0, got:\n%s", logged)
	}
}
