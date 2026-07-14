package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// toolCreateFolder
// ─────────────────────────────────────────────────────────────────

func TestToolCreateFolderCreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	result, err := toolCreateFolder(map[string]interface{}{"path": "a/b/c", "reason": "test"})
	if err != nil {
		t.Fatalf("expected create_folder to succeed, got: %v", err)
	}
	if !strings.Contains(result, "a/b/c") {
		t.Fatalf("expected the result to mention the path, got: %s", result)
	}
	info, statErr := os.Stat(filepath.Join(dir, "a", "b", "c"))
	if statErr != nil {
		t.Fatalf("expected nested directory to exist: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatal("expected the created path to be a directory")
	}
}

// TestToolCreateFolderNoOpWhenAlreadyExists confirms calling create_folder
// twice on the same path is a success both times - a model retrying a plan
// (or re-running a step it already completed) shouldn't be penalized for a
// directory that's already there.
func TestToolCreateFolderNoOpWhenAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if _, err := toolCreateFolder(map[string]interface{}{"path": "already-there"}); err != nil {
		t.Fatalf("expected first call to succeed: %v", err)
	}
	result, err := toolCreateFolder(map[string]interface{}{"path": "already-there"})
	if err != nil {
		t.Fatalf("expected second call on an existing directory to also succeed, got: %v", err)
	}
	if !strings.Contains(result, "มีอยู่แล้ว") {
		t.Fatalf("expected the result to note the directory already existed, got: %s", result)
	}
}

// TestToolCreateFolderRejectsWhenPathIsFile confirms a genuine conflict (the
// path exists but is a regular file, not a directory) is a real error, not
// silently treated the same as the already-a-directory no-op case above.
func TestToolCreateFolderRejectsWhenPathIsFile(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile(filepath.Join(dir, "im-a-file"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := toolCreateFolder(map[string]interface{}{"path": "im-a-file"}); err == nil {
		t.Fatal("expected create_folder to reject a path that already exists as a file")
	}
}

func TestToolCreateFolderRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if _, err := toolCreateFolder(map[string]interface{}{"path": "../escaped"}); err == nil {
		t.Fatal("expected a path escaping the sandbox to be rejected")
	}
}

func TestToolCreateFolderRequiresPath(t *testing.T) {
	if _, err := toolCreateFolder(map[string]interface{}{}); err == nil {
		t.Fatal("expected an empty path to be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────
// parseDelayDuration / formatDelayDuration
// ─────────────────────────────────────────────────────────────────

func TestParseDelayDurationValid(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"45s", 45 * time.Second},
		{"2h", 2 * time.Hour},
		{"30m", 30 * time.Minute},
		{"1d", 24 * time.Hour},
		{"1d2h30m", 24*time.Hour + 2*time.Hour + 30*time.Minute},
		{"1d2h30m10s", 24*time.Hour + 2*time.Hour + 30*time.Minute + 10*time.Second},
		{"0s", 0},
		{"  2h  ", 2 * time.Hour}, // surrounding whitespace is trimmed
	}
	for _, c := range cases {
		got, err := parseDelayDuration(c.in)
		if err != nil {
			t.Fatalf("parseDelayDuration(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("parseDelayDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseDelayDurationInvalid confirms malformed input - including unit
// letters out of order or repeated, which the anchored regex simply fails
// to match rather than reinterpreting - is rejected.
func TestParseDelayDurationInvalid(t *testing.T) {
	bad := []string{
		"",
		"garbage",
		"5",    // no unit letter
		"d5",   // digit after the letter instead of before
		"1h1d", // wrong order (d must come before h)
		"1d1d", // repeated unit
		"-5s",  // no negative numbers
		"5.5s", // no fractional units
		"5 s",  // no internal space
	}
	for _, entry := range bad {
		if _, err := parseDelayDuration(entry); err == nil {
			t.Errorf("parseDelayDuration(%q) expected an error, got none", entry)
		}
	}
}

func TestFormatDelayDurationRoundTrips(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{2 * time.Hour, "2h0m0s"},
		{24*time.Hour + 2*time.Hour + 30*time.Minute + 10*time.Second, "1d2h30m10s"},
	}
	for _, c := range cases {
		if got := formatDelayDuration(c.d); got != c.want {
			t.Fatalf("formatDelayDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestFormatDelayDurationIsParseDelayDurationInverse confirms every valid
// input from TestParseDelayDurationValid, once parsed, formats back into
// something parseDelayDuration itself accepts and resolves to the same
// duration - i.e. the two functions genuinely agree on the same unit
// family instead of just happening to pass separate example-based checks.
func TestFormatDelayDurationIsParseDelayDurationInverse(t *testing.T) {
	for _, in := range []string{"45s", "2h", "30m", "1d", "1d2h30m10s"} {
		d, err := parseDelayDuration(in)
		if err != nil {
			t.Fatalf("parseDelayDuration(%q) unexpected error: %v", in, err)
		}
		formatted := formatDelayDuration(d)
		reparsed, err := parseDelayDuration(formatted)
		if err != nil {
			t.Fatalf("parseDelayDuration(formatDelayDuration(%q)=%q) unexpected error: %v", in, formatted, err)
		}
		if reparsed != d {
			t.Fatalf("round-trip mismatch for %q: parsed=%v formatted=%q reparsed=%v", in, d, formatted, reparsed)
		}
	}
}

// ─────────────────────────────────────────────────────────────────
// toolDelay
// ─────────────────────────────────────────────────────────────────

// TestToolDelayActuallyWaits confirms the tool really blocks for
// (approximately) the requested duration rather than just validating and
// returning immediately - kept to 1s (the smallest unit the format
// supports) to keep the test fast while still being a genuine check.
func TestToolDelayActuallyWaits(t *testing.T) {
	start := time.Now()
	result, err := toolDelay(map[string]interface{}{"duration": "1s"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected delay to succeed, got: %v", err)
	}
	if elapsed < 1*time.Second {
		t.Fatalf("expected toolDelay to actually block for at least 1s, only took %s", elapsed)
	}
	if !strings.Contains(result, "1s") {
		t.Fatalf("expected the result to report the duration waited, got: %s", result)
	}
}

func TestToolDelayRejectsInvalidDuration(t *testing.T) {
	if _, err := toolDelay(map[string]interface{}{"duration": "not-a-duration"}); err == nil {
		t.Fatal("expected an invalid duration to be rejected")
	}
}

// TestToolDelayRejectsExceedingMaxDuration confirms a request over
// maxDelayDuration is rejected up front - critically, without ever calling
// time.Sleep - so this test itself stays fast instead of actually hanging
// for the (rejected) requested duration.
func TestToolDelayRejectsExceedingMaxDuration(t *testing.T) {
	start := time.Now()
	_, err := toolDelay(map[string]interface{}{"duration": "2d"}) // 48h > maxDelayDuration (24h)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a duration exceeding maxDelayDuration to be rejected")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("expected the cap to be enforced before ever sleeping, took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "เกินขีดจำกัด") {
		t.Fatalf("expected the error to explain the cap, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────
// dispatchToolCall wiring
// ─────────────────────────────────────────────────────────────────

func TestDispatchToolCallCreateFolder(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"path": "newdir", "reason": "เก็บ output"})
	tc := toolCall{Function: toolCallFunction{Name: "create_folder", Arguments: argsJSON}}

	var changes []string
	result := dispatchToolCall(tc, "", "", "", outFile, nil, &changes)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected create_folder to be recognized and succeed, got: %s", result)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "newdir")); statErr != nil {
		t.Fatalf("expected the directory to actually be created: %v", statErr)
	}
	if len(changes) != 1 || !strings.Contains(changes[0], "newdir") {
		t.Fatalf("expected the directory creation to be recorded as a session change, got: %v", changes)
	}
	if !strings.Contains(changes[0], "MKDIR") {
		t.Fatalf("expected the recorded change to be tagged MKDIR, got: %v", changes)
	}
}

func TestDispatchToolCallDelay(t *testing.T) {
	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"duration": "0s"})
	tc := toolCall{Function: toolCallFunction{Name: "delay", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, nil)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected delay to be recognized by dispatchToolCall, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "[tool_load_time] delay") {
		t.Fatalf("expected a [tool_load_time] entry for delay, got:\n%s", logged)
	}
}

// ─────────────────────────────────────────────────────────────────
// End-to-end: create_folder and delay reached through a real cmdAsk run
// ─────────────────────────────────────────────────────────────────

// TestCmdAskCreateFolderAndDelayEndToEnd drives cmdAsk (the same real entry
// point exercised in ask_integration_test.go/scp_integration_test.go)
// through a scripted mock model that calls create_folder, then delay, then
// gives a plain final answer - confirming both are offered to the model
// with no configuration needed (unlike scp_copy/web_search, they're part of
// builtinTools) and that they actually run end-to-end through
// dispatchToolCall.
func TestCmdAskCreateFolderAndDelayEndToEnd(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var round int32
	var firstBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			b, _ := io.ReadAll(r.Body)
			firstBody = string(b)
			fmt.Fprint(w, streamLine("", "create_folder", `{"path":"reports","reason":"เก็บรายงาน"}`, true))
		case 2:
			fmt.Fprint(w, streamLine("", "delay", `{"duration":"1s"}`, true))
		default:
			fmt.Fprint(w, streamLine("เสร็จเรียบร้อยครับ", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-folder-delay.log", "สร้างโฟลเดอร์ reports แล้วรอสักครู่"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 3 {
		t.Fatalf("expected exactly 3 rounds (create_folder, delay, final answer), got %d", got)
	}
	if !strings.Contains(firstBody, `"create_folder"`) {
		t.Fatalf("expected create_folder to always be offered (no config needed), got request body: %s", firstBody)
	}
	if !strings.Contains(firstBody, `"delay"`) {
		t.Fatalf("expected delay to always be offered (no config needed), got request body: %s", firstBody)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "reports")); statErr != nil {
		t.Fatalf("expected the reports/ directory to actually be created: %v", statErr)
	}

	log, err := os.ReadFile("ask-folder-delay.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "[tool_call] create_folder") {
		t.Fatalf("expected a create_folder tool_call entry in the log, got:\n%s", log)
	}
	if !strings.Contains(string(log), "[tool_call] delay") {
		t.Fatalf("expected a delay tool_call entry in the log, got:\n%s", log)
	}
}
