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

// TestStreamResponseReportsPromptEvalDuration confirms prompt_eval_duration
// from the final NDJSON chunk - Ollama's time spent ingesting the prompt
// before it could start generating - is captured separately from
// load_duration (getting the model into memory) and eval_duration
// (generating the reply), and surfaced in both the terminal output and the
// log file. This is what lets a session that's slow to *start* answering
// (e.g. a huge auto-injected directory tree or attached file) be told apart
// from one that's just generating a long reply.
func TestStreamResponseReportsPromptEvalDuration(t *testing.T) {
	// prompt_eval_duration=450ms, well under fmtLoadDur's 1s cutoff, so it
	// should be reported with millisecond precision rather than rounded to
	// "0.5s" by the coarser fmtDur used for preload/round.
	chunk := `{"message":{"role":"assistant","content":"hi"},"done":true,"prompt_eval_count":500,"eval_count":5,"eval_duration":500000000,"load_duration":0,"prompt_eval_duration":450000000}` + "\n"

	outFile, err := os.CreateTemp(t.TempDir(), "stream-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamResponse(strings.NewReader(chunk), outFile, "", "", "", "")

	if out.PromptEvalDurationNS != 450000000 {
		t.Fatalf("expected PromptEvalDurationNS to be captured as 450ms (450000000ns), got %d", out.PromptEvalDurationNS)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "prompt eval") {
		t.Fatalf("expected the log to mention prompt eval time, got:\n%s", logged)
	}
	if !strings.Contains(string(logged), "450ms") {
		t.Fatalf("expected the log to report the prompt eval time with ms precision (450ms), got:\n%s", logged)
	}
}

// TestStreamResponseNoPromptEvalLineWhenZero mirrors
// TestStreamResponseNoPreloadLineWhenZero: a chunk that doesn't report
// prompt_eval_duration at all (some model/proxy setups omit it) must not
// produce a spurious "prompt eval: 0ms" line on every single round.
func TestStreamResponseNoPromptEvalLineWhenZero(t *testing.T) {
	chunk := `{"message":{"role":"assistant","content":"hi"},"done":true,"eval_duration":100000000,"load_duration":0,"prompt_eval_duration":0}` + "\n"

	outFile, err := os.CreateTemp(t.TempDir(), "stream-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamResponse(strings.NewReader(chunk), outFile, "", "", "", "")
	if out.PromptEvalDurationNS != 0 {
		t.Fatalf("expected PromptEvalDurationNS 0, got %d", out.PromptEvalDurationNS)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "prompt eval") {
		t.Fatalf("expected no prompt eval line when prompt_eval_duration is 0, got:\n%s", logged)
	}
}
