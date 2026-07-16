// main_test.go - ola's whole test suite in one file: focused, no-network
// unit tests (originally unit_test.go, itself already a consolidation of
// coding_test.go, folder_delay_test.go, stream_test.go, notify_test.go,
// time_test.go, scp_test.go, search_test.go, and skills_test.go),
// end-to-end httptest-driven tests that drive the real cmdAsk/cmdCoding
// entry points against a mocked Ollama /api/chat endpoint (originally
// integration_test.go, itself a consolidation of ask_integration_test.go,
// coding_integration_test.go, scp_integration_test.go, and
// freshness_test.go), api_request-specific tests (originally
// api_request_test.go), and the quiet-mode tests for -q/--quiet/$OLA_QUIET
// (new). Merged into one file as part of the same file-count cleanup this
// package's non-test .go files went through - see main.go's own package
// doc comment - nothing about any individual test changed, only its
// location. Look for the "======= Section:" banners below to find where
// each former file's content begins.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ======================================================================
// Section: unit_test.go
// ======================================================================

// ======================================================================
// coding_test.go
// ======================================================================

func TestValidateCommandAllowsKnownToolchainBinaries(t *testing.T) {
	allowed := map[string]bool{"go": true}
	if err := validateCommand("go build ./...", allowed); err != nil {
		t.Fatalf("expected allowed command to pass, got error: %v", err)
	}
	if err := validateCommand("go build ./... && go test ./...", allowed); err != nil {
		t.Fatalf("expected chained allowed commands to pass, got error: %v", err)
	}
}

func TestValidateCommandRejectsDisallowedBinary(t *testing.T) {
	allowed := map[string]bool{"go": true}
	if err := validateCommand("rm -rf /", allowed); err == nil {
		t.Fatal("expected dangerous command to be rejected")
	}
	if err := validateCommand("curl http://example.com", allowed); err == nil {
		t.Fatal("expected disallowed binary to be rejected")
	}
	if err := validateCommand("go build ./... && curl http://evil", allowed); err == nil {
		t.Fatal("expected chained command with a disallowed segment to be rejected")
	}
}

func TestValidateCommandRejectsEmpty(t *testing.T) {
	if err := validateCommand("   ", map[string]bool{"go": true}); err == nil {
		t.Fatal("expected empty command to be rejected")
	}
}

func TestSplitCommandSegments(t *testing.T) {
	got := splitCommandSegments("go build ./... && go test ./... ; echo done | cat")
	want := []string{"go build ./...", "go test ./...", "echo done", "cat"}
	if len(got) != len(want) {
		t.Fatalf("got %d segments, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestIsVerifiableEditGatesByToolchainExtension(t *testing.T) {
	cases := []struct {
		path, label string
		forceAny    bool
		want        bool
	}{
		{"main.go", "go", false, true},
		{"notes.txt", "go", false, false},
		{"README.md", "go", false, false},
		{"package.json", "node", false, true},
		{"index.js", "node", false, true},
		{"design.txt", "node", false, false},
		{"lib.rs", "rust", false, true},
		{"app.py", "python", false, true},
		{"anything.txt", "generic", false, false},
		// An explicit --build-cmd/--test-cmd override means the user opted
		// in deliberately - any file counts, regardless of extension.
		{"notes.txt", "go", true, true},
		{"", "go", false, false},
	}
	for _, c := range cases {
		got := isVerifiableEdit(c.path, c.label, c.forceAny)
		if got != c.want {
			t.Errorf("isVerifiableEdit(%q, %q, %v) = %v, want %v", c.path, c.label, c.forceAny, got, c.want)
		}
	}
}

func TestRunBuildOnlyPassesAndFails(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module buildonlytest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)

	passed, report := runBuildOnly(cmds, 5*time.Second)
	if !passed {
		t.Fatalf("expected build-only check to pass on valid code, got: %s", report)
	}

	// Now break the build and confirm the light check catches it.
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {\n"), 0644); err != nil {
		t.Fatal(err)
	}
	passed, report = runBuildOnly(cmds, 5*time.Second)
	if passed {
		t.Fatal("expected build-only check to fail on broken code")
	}
	if !strings.Contains(report, "exit_code") {
		t.Fatalf("expected failure report to include exit_code, got: %s", report)
	}
}

// TestMarkTaskDoneRejectedWhenBuildBroken exercises dispatchCodingToolCall
// directly (no HTTP mock needed) to confirm mark_task_done's build-only
// light gate rejects the call - and does NOT mark the task done - when the
// project doesn't currently build, then succeeds once it's fixed.
func TestMarkTaskDoneRejectedWhenBuildBroken(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module marktest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Broken from the start.
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)

	state := newCodingState()
	state.addTasks([]string{"do the thing"})

	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	rc := &codingRunContext{
		outFile: outFile, state: state, allowed: cmds.AllowBins,
		cmdTO: 5 * time.Second, cmds: cmds,
	}

	markArgs, _ := json.Marshal(map[string]interface{}{"task_id": "T0", "note": "done"})
	tc := toolCall{Function: toolCallFunction{Name: "mark_task_done", Arguments: markArgs}}

	result, isReport := dispatchCodingToolCall(tc, rc)
	if isReport {
		t.Fatal("mark_task_done must never report as report_complete")
	}
	if !strings.Contains(result, "ถูกปฏิเสธ") {
		t.Fatalf("expected mark_task_done to be rejected while build is broken, got: %s", result)
	}
	if state.Tasks[0].Done {
		t.Fatal("task must not be marked done when the build-only gate rejects it")
	}

	// Fix the build and retry - should now succeed and actually mark done.
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	result, _ = dispatchCodingToolCall(tc, rc)
	if strings.Contains(result, "ถูกปฏิเสธ") {
		t.Fatalf("expected mark_task_done to succeed once build is fixed, got: %s", result)
	}
	if !state.Tasks[0].Done {
		t.Fatal("expected task to be marked done after the build-only gate passes")
	}
}

func TestCodingStateAddAndMarkDone(t *testing.T) {
	s := newCodingState()
	added := s.addTasks([]string{"Set up scaffolding", "Implement feature X"})
	if len(added) != 2 || added[0].ID != "T0" || added[1].ID != "T1" {
		t.Fatalf("unexpected task IDs: %+v", added)
	}
	if _, err := s.markDone("T0", "done"); err != nil {
		t.Fatalf("expected markDone to succeed: %v", err)
	}
	done, total := s.progress()
	if done != 1 || total != 2 {
		t.Fatalf("expected progress 1/2, got %d/%d", done, total)
	}
	if _, err := s.markDone("T99", ""); err == nil {
		t.Fatal("expected error for unknown task_id")
	}
}

func TestCompactMessagesKeepsSystemAndRecentIntact(t *testing.T) {
	var messages []ollamaMessage
	messages = append(messages, ollamaMessage{Role: "system", Content: "sys"})
	messages = append(messages, ollamaMessage{Role: "user", Content: "reqs"})
	for i := 0; i < 40; i++ {
		messages = append(messages, ollamaMessage{Role: "assistant", Content: "working"})
	}
	compacted := compactMessages(messages)
	if compacted[0].Role != "system" || compacted[1].Role != "user" {
		t.Fatal("expected system+first user message to be preserved at the head")
	}
	if len(compacted) >= len(messages) {
		t.Fatalf("expected compaction to shrink message count: got %d, had %d", len(compacted), len(messages))
	}
	tail := compacted[len(compacted)-keepRecentMessagesUncompacted:]
	for _, m := range tail {
		if m.Content != "working" {
			t.Fatal("expected the most recent messages to survive compaction untouched")
		}
	}
}

func TestCompactMessagesNoOpWhenShort(t *testing.T) {
	messages := []ollamaMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "reqs"},
		{Role: "assistant", Content: "hi"},
	}
	compacted := compactMessages(messages)
	if len(compacted) != len(messages) {
		t.Fatalf("expected no-op for short conversation, got len %d", len(compacted))
	}
}

func TestDetectProjectCommandsGoModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module x\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)
	if cmds.Label != "go" || cmds.BuildCmd != "go build ./..." || !cmds.AllowBins["go"] {
		t.Fatalf("unexpected detection for go module: %+v", cmds)
	}
}

func TestDetectProjectCommandsGeneric(t *testing.T) {
	dir := t.TempDir()
	cmds := detectProjectCommands(dir)
	if cmds.Label != "generic" || cmds.BuildCmd != "" || cmds.TestCmd != "" {
		t.Fatalf("expected generic/empty detection for an empty dir, got: %+v", cmds)
	}
}

func TestToolRunCommandExecutesAllowedCommand(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"command": "true"}
	result, err := toolRunCommand(args, map[string]bool{"true": true}, 5*time.Second)
	if err != nil {
		t.Fatalf("expected allowed command to run: %v", err)
	}
	if !strings.Contains(result, "exit_code=0") {
		t.Fatalf("expected success exit code in result, got: %s", result)
	}
}

func TestToolRunCommandRejectsDisallowedCommand(t *testing.T) {
	args := map[string]interface{}{"command": "curl http://example.com"}
	if _, err := toolRunCommand(args, map[string]bool{"go": true}, 5*time.Second); err == nil {
		t.Fatal("expected disallowed command to be rejected before execution")
	}
}

func TestRunShellCommandTimeout(t *testing.T) {
	_, exitCode, err := runShellCommand("sleep 5", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if exitCode != -1 {
		t.Fatalf("expected exitCode -1 on timeout, got %d", exitCode)
	}
}

func TestToolAddTasksAndMarkTaskDoneArgs(t *testing.T) {
	t.Cleanup(func() {
		_ = os.Remove(codingStateFile)
		_ = os.Remove(codingProgressFile)
	})
	s := newCodingState()
	raw, _ := json.Marshal(map[string]interface{}{"tasks": []string{"a", "b"}})
	var args map[string]interface{}
	_ = json.Unmarshal(raw, &args)
	result, err := toolAddTasks(args, s)
	if err != nil {
		t.Fatalf("toolAddTasks failed: %v", err)
	}
	if !strings.Contains(result, "T0") || !strings.Contains(result, "T1") {
		t.Fatalf("expected result to mention both task IDs, got: %s", result)
	}
}

// ======================================================================
// folder_delay_test.go
// ======================================================================

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

// ======================================================================
// stream_test.go
// ======================================================================

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

// ======================================================================
// notify_test.go
// ======================================================================

// TestTruncateWordsNoOpUnderLimit confirms short text passes through
// unchanged - the common case for the vast majority of real notifications,
// which shouldn't grow a truncation marker they don't need.
func TestTruncateWordsNoOpUnderLimit(t *testing.T) {
	short := "แก้ไข typo ใน notes.txt เรียบร้อยแล้ว"
	if got := truncateWords(short, maxNotificationWords); got != short {
		t.Fatalf("expected short text to pass through unchanged, got: %q", got)
	}
}

// TestTruncateWordsCapsAtMaxWords confirms a summary longer than
// maxNotificationWords is cut down to exactly that many words (plus a
// marker noting the original length), never silently left over the cap.
func TestTruncateWordsCapsAtMaxWords(t *testing.T) {
	words := make([]string, 1500)
	for i := range words {
		words[i] = "word"
	}
	long := strings.Join(words, " ")

	got := truncateWords(long, maxNotificationWords)
	kept := strings.Fields(got)
	// strings.Fields on the truncated+marker text will also pick up the
	// marker's words, so just confirm the first maxNotificationWords
	// "word" tokens are intact and a truncation note was appended.
	if len(kept) < maxNotificationWords {
		t.Fatalf("expected at least %d words to survive truncation, got %d", maxNotificationWords, len(kept))
	}
	for i := 0; i < maxNotificationWords; i++ {
		if kept[i] != "word" {
			t.Fatalf("expected word %d to be untouched content, got %q", i, kept[i])
		}
	}
	if !strings.Contains(got, "1500") {
		t.Fatalf("expected the truncation marker to note the true original word count (1500), got: %s", got)
	}
}

// TestTruncateUTF8BytesNeverSplitsMultiByteRune is the core regression test
// for the ntfy attachment-conversion bug: ntfy.sh silently turns any
// message body over ~4096 bytes into a downloadable attachment.txt instead
// of a text notification, and Thai text is ~3 bytes per character in
// UTF-8, so a naive byte slice (s[:n]) can both corrupt the text AND still
// not guarantee staying under the limit if done carelessly. This confirms
// truncateUTF8Bytes produces valid UTF-8 no matter where the cut falls.
func TestTruncateUTF8BytesNeverSplitsMultiByteRune(t *testing.T) {
	thai := strings.Repeat("ทดสอบข้อความภาษาไทยยาวๆ ", 300) // well over any reasonable byte cap
	for maxBytes := 1; maxBytes < 30; maxBytes++ {
		got := truncateUTF8Bytes(thai, maxBytes)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateUTF8Bytes(_, %d) produced invalid UTF-8: %q", maxBytes, got)
		}
	}
	// A larger, more realistic cap: the kept portion (before the marker)
	// must never exceed maxBytes.
	const byteCap = 200
	got := truncateUTF8Bytes(thai, byteCap)
	marker := "\n...(ตัดข้อความ)"
	kept := strings.TrimSuffix(got, marker)
	if len(kept) > byteCap {
		t.Fatalf("expected the kept portion to respect the %d-byte cap, got %d bytes", byteCap, len(kept))
	}
}

// TestTruncateUTF8BytesNoOpUnderLimit confirms text already within the
// byte budget is returned unchanged (no spurious marker).
func TestTruncateUTF8BytesNoOpUnderLimit(t *testing.T) {
	short := "Work Finished: ok"
	if got := truncateUTF8Bytes(short, ntfySafeBodyBytes); got != short {
		t.Fatalf("expected short text to pass through unchanged, got: %q", got)
	}
}

// TestThaiSummaryWordCapAloneIsNotEnoughForNtfyByteLimit documents and
// verifies the exact reasoning behind having both a word cap AND a byte
// cap: 1000 words of Thai text is comfortably larger than ntfy's ~4096
// byte message limit, so if sendNotification only enforced
// maxNotificationWords, a legitimate Thai session summary could still get
// silently converted into attachment.txt by ntfy. This confirms the
// byte-safety net (as applied inside sendNotification) brings even a
// maximal 1000-word Thai summary back under the safe limit.
func TestThaiSummaryWordCapAloneIsNotEnoughForNtfyByteLimit(t *testing.T) {
	words := make([]string, maxNotificationWords)
	for i := range words {
		words[i] = "ทดสอบข้อความ" // a plausible Thai "word", 3 bytes/char
	}
	wordCapped := truncateWords(strings.Join(words, " "), maxNotificationWords)

	if len(wordCapped) <= ntfySafeBodyBytes {
		t.Fatalf("test setup invalid: expected a %d-word Thai string to exceed the %d-byte safety cap on its own (got %d bytes) - otherwise this test isn't exercising the byte-cap safety net at all",
			maxNotificationWords, ntfySafeBodyBytes, len(wordCapped))
	}

	final := truncateUTF8Bytes(wordCapped, ntfySafeBodyBytes)
	if len(final) > ntfySafeBodyBytes+64 { // small allowance for the trailing marker itself
		t.Fatalf("expected the byte-safety net to bring the message back near the %d-byte cap, got %d bytes", ntfySafeBodyBytes, len(final))
	}
	if !utf8.ValidString(final) {
		t.Fatal("expected the final, byte-capped notification body to still be valid UTF-8")
	}
	// And it must stay comfortably under ntfy's real ~4096-byte limit.
	if len(final) >= 4096 {
		t.Fatalf("expected final notification body to stay under ntfy's ~4096-byte attachment-conversion threshold, got %d bytes", len(final))
	}
}

// TestBuildWorkSummaryIncludesChangesAndModelSummary confirms the
// end-of-session notification is a genuine recap - not just whatever the
// model happened to say - by listing the concrete file changes recorded
// during the session alongside the model's own closing remark.
func TestBuildWorkSummaryIncludesChangesAndModelSummary(t *testing.T) {
	changes := []string{
		"[WRITE] main.go - initial hello world program",
		"[EDIT] main.go - fixed missing closing paren",
	}
	got := buildWorkSummary("Work Finished", changes, "แก้ไขให้แล้วครับ โปรแกรมทำงานถูกต้อง")

	if !strings.HasPrefix(got, "Work Finished: แก้ไขให้แล้วครับ") {
		t.Fatalf("expected the label and model summary to lead the notification, got: %s", got)
	}
	if !strings.Contains(got, "สิ่งที่ทำ (2 รายการ)") {
		t.Fatalf("expected a count of recorded changes, got: %s", got)
	}
	for _, c := range changes {
		if !strings.Contains(got, c) {
			t.Fatalf("expected recorded change %q to appear in the summary, got: %s", c, got)
		}
	}
}

// TestBuildWorkSummaryHandlesNoChangesOrEmptySummary confirms a plain Q&A
// session (no file changes, e.g. TestCmdAskVerifyDisabledForPlainQuestions)
// still produces a sensible, non-empty notification body instead of an
// empty or malformed one.
func TestBuildWorkSummaryHandlesNoChangesOrEmptySummary(t *testing.T) {
	got := buildWorkSummary("Work Finished", nil, "")
	if got != "Work Finished" {
		t.Fatalf("expected just the label when there is nothing else to report, got: %q", got)
	}
}

// TestDispatchToolCallRecordsChangesWhenCollectorProvided exercises the
// real dispatch path (the same one cmdAsk and dispatchCodingToolCall use)
// to confirm a successful write_file call is recorded into an optional
// session change log, which is what feeds buildWorkSummary's "สิ่งที่ทำ"
// list at the end of a session.
func TestDispatchToolCallRecordsChangesWhenCollectorProvided(t *testing.T) {
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

	argsJSON, _ := json.Marshal(map[string]interface{}{
		"path": "hello.txt", "content": "hi", "reason": "test file for change tracking",
	})
	tc := toolCall{Function: toolCallFunction{Name: "write_file", Arguments: argsJSON}}

	var changes []string
	result := dispatchToolCall(tc, "", "", "", outFile, nil, &changes)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected write_file to succeed, got: %s", result)
	}
	if len(changes) != 1 {
		t.Fatalf("expected exactly 1 recorded change, got %d: %v", len(changes), changes)
	}
	if !strings.Contains(changes[0], "hello.txt") || !strings.Contains(changes[0], "test file for change tracking") {
		t.Fatalf("expected the recorded change to include the path and reason, got: %s", changes[0])
	}
}

// TestDispatchToolCallLogsLoadTimeForFileTools confirms read_file - a tool
// that loads data from local disk - gets a [tool_load_time] line logged
// alongside its normal [tool_result], so a session that feels slow can be
// diagnosed as "waiting on disk I/O" rather than assumed to be "the model
// thinking slowly".
func TestDispatchToolCallLogsLoadTimeForFileTools(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("hello.txt", []byte("hi there"), 0644); err != nil {
		t.Fatal(err)
	}

	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"path": "hello.txt"})
	tc := toolCall{Function: toolCallFunction{Name: "read_file", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, nil)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected read_file to succeed, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "[tool_load_time] read_file") {
		t.Fatalf("expected a [tool_load_time] entry for read_file, got:\n%s", logged)
	}
	if !strings.Contains(string(logged), "โหลดไฟล์") {
		t.Fatalf("expected the load-time entry to be labeled as a local file load, got:\n%s", logged)
	}
}

// TestDispatchToolCallSkipsLoadTimeForNonLoadTools confirms tools that
// don't represent a data load - get_current_time here, which does no I/O
// at all - never get a [tool_load_time] line, so the load-timing output
// stays meaningful (only appears for actual file/network loads) instead of
// becoming noise on every single tool call regardless of what it does.
func TestDispatchToolCallSkipsLoadTimeForNonLoadTools(t *testing.T) {
	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"timezone": "UTC"})
	tc := toolCall{Function: toolCallFunction{Name: "get_current_time", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, nil)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected get_current_time to succeed, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "[tool_load_time]") {
		t.Fatalf("expected no [tool_load_time] entry for get_current_time (no I/O involved), got:\n%s", logged)
	}
}

// trailing collector (the pre-existing call shape used elsewhere, e.g.
// TestDispatchToolCallGetCurrentTime in time_test.go) still compiles and
// behaves identically - the collector is opt-in, not required.
func TestDispatchToolCallWithoutCollectorStillWorks(t *testing.T) {
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

	argsJSON, _ := json.Marshal(map[string]interface{}{"path": "hello.txt", "content": "hi"})
	tc := toolCall{Function: toolCallFunction{Name: "write_file", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, nil)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected write_file to succeed without a collector, got: %s", result)
	}
}

// ======================================================================
// time_test.go
// ======================================================================

func TestToolGetCurrentTimeDefaultsToLocalTimezone(t *testing.T) {
	before := time.Now().Unix()
	result, err := toolGetCurrentTime(map[string]interface{}{})
	after := time.Now().Unix()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !strings.Contains(result, "current_time:") || !strings.Contains(result, "day_of_week:") ||
		!strings.Contains(result, "unix_timestamp:") || !strings.Contains(result, "timezone:") {
		t.Fatalf("expected result to contain all documented fields, got: %s", result)
	}

	// Extract unix_timestamp and sanity-check it against wall-clock bounds
	// taken immediately before/after the call.
	var ts int64
	for _, line := range strings.Split(result, "\n") {
		if strings.HasPrefix(line, "unix_timestamp: ") {
			v, convErr := strconv.ParseInt(strings.TrimPrefix(line, "unix_timestamp: "), 10, 64)
			if convErr != nil {
				t.Fatalf("could not parse unix_timestamp line %q: %v", line, convErr)
			}
			ts = v
		}
	}
	if ts < before || ts > after {
		t.Fatalf("expected unix_timestamp %d to fall between %d and %d", ts, before, after)
	}
}

func TestToolGetCurrentTimeWithValidTimezone(t *testing.T) {
	result, err := toolGetCurrentTime(map[string]interface{}{"timezone": "UTC"})
	if err != nil {
		t.Fatalf("expected UTC to be a valid timezone, got: %v", err)
	}
	if !strings.Contains(result, "timezone: UTC") {
		t.Fatalf("expected the result to report the requested timezone, got: %s", result)
	}
}

func TestToolGetCurrentTimeWithInvalidTimezone(t *testing.T) {
	_, err := toolGetCurrentTime(map[string]interface{}{"timezone": "Not/A_Real_Zone"})
	if err == nil {
		t.Fatal("expected an error for an invalid IANA timezone name")
	}
	if !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("expected the error to mention the bad timezone, got: %v", err)
	}
}

func TestToolGetCurrentTimeConvertsCorrectly(t *testing.T) {
	utc, err := toolGetCurrentTime(map[string]interface{}{"timezone": "UTC"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bangkok, err := toolGetCurrentTime(map[string]interface{}{"timezone": "Asia/Bangkok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Bangkok is UTC+7 with no DST, so the reported hour should differ from
	// UTC's (except in the rare instant that straddles midnight - accept
	// that as a pass too since asserting exact wall-clock math here would
	// make the test flaky around 17:00 UTC).
	if utc == bangkok {
		t.Fatal("expected UTC and Asia/Bangkok results to differ")
	}
}

// TestDispatchToolCallGetCurrentTime exercises get_current_time through the
// real dispatch path (same one "ask" and "coding" both use), confirming the
// tool name routes correctly end-to-end rather than only unit-testing the
// underlying function in isolation.
func TestDispatchToolCallGetCurrentTime(t *testing.T) {
	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"timezone": "UTC"})
	tc := toolCall{Function: toolCallFunction{Name: "get_current_time", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, nil)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected get_current_time to be recognized by dispatchToolCall, got: %s", result)
	}
	if !strings.Contains(result, "timezone: UTC") {
		t.Fatalf("expected UTC timezone in dispatched result, got: %s", result)
	}
}

// ======================================================================
// scp_test.go
// ======================================================================

// ─────────────────────────────────────────────────────────────────
// parseSCPHostEntry / resolveSCPConfig
// ─────────────────────────────────────────────────────────────────

func TestParseSCPHostEntryValid(t *testing.T) {
	cases := []struct {
		entry                               string
		alias, user, host, port, remoteRoot string
	}{
		{"backup=moo@10.0.0.5:2222/srv/backup", "backup", "moo", "10.0.0.5", "2222", "/srv/backup"},
		{"nas=moo@nas.local/mnt/data", "nas", "moo", "nas.local", defaultSSHPort, "/mnt/data"},
		{"  spaced = moo@host /root  ", "spaced", "moo", "host", defaultSSHPort, "/root"},
	}
	for _, c := range cases {
		h, err := parseSCPHostEntry(c.entry)
		if err != nil {
			t.Fatalf("parseSCPHostEntry(%q) unexpected error: %v", c.entry, err)
		}
		if h.Alias != c.alias || h.User != c.user || h.Host != c.host || h.Port != c.port || h.RemoteRoot != c.remoteRoot {
			t.Fatalf("parseSCPHostEntry(%q) = %+v, want alias=%s user=%s host=%s port=%s root=%s",
				c.entry, h, c.alias, c.user, c.host, c.port, c.remoteRoot)
		}
	}
}

func TestParseSCPHostEntryInvalid(t *testing.T) {
	bad := []string{
		"",
		"noequalsigns",             // no "="
		"alias=onlyonefield",       // "=" but no "/" -> no remote root
		"alias=missingatsign/root", // hostspec has no "@"
		"=moo@host/root",           // empty alias
		"alias=@host/root",         // empty user
		"alias=moo@/root",          // empty host
	}
	for _, entry := range bad {
		if _, err := parseSCPHostEntry(entry); err == nil {
			t.Errorf("parseSCPHostEntry(%q) expected an error, got none", entry)
		}
	}
}

func TestResolveSCPConfigDisabledByDefault(t *testing.T) {
	t.Setenv("OLA_SCP_HOSTS", "")
	cfg, warnings := resolveSCPConfig("", "", "", 0, 0)
	if cfg.enabled() {
		t.Fatal("expected scp_copy to be disabled with no OLA_SCP_HOSTS/--scp-hosts configured")
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for an empty config, got: %v", warnings)
	}
}

// TestResolveSCPConfigFlagOverridesEnv confirms the flag>env>default
// precedence used throughout ola (resolveSearchConfig, resolveSkillsDirs)
// applies identically here: an explicit --scp-hosts wins over
// OLA_SCP_HOSTS.
func TestResolveSCPConfigFlagOverridesEnv(t *testing.T) {
	t.Setenv("OLA_SCP_HOSTS", "envalias=moo@envhost/env/root")
	cfg, warnings := resolveSCPConfig("flagalias=moo@flaghost/flag/root", "", "", 0, 0)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", warnings)
	}
	if !cfg.enabled() {
		t.Fatal("expected scp_copy to be enabled")
	}
	if _, ok := cfg.Hosts["flagalias"]; !ok {
		t.Fatalf("expected the --scp-hosts flag value to win over OLA_SCP_HOSTS, got hosts: %+v", cfg.Hosts)
	}
	if _, ok := cfg.Hosts["envalias"]; ok {
		t.Fatal("expected the env-only alias to be ignored once the flag is set")
	}
}

// TestResolveSCPConfigSkipsBadEntryButKeepsOthers confirms one malformed
// OLA_SCP_HOSTS entry produces a warning and is skipped, without taking
// down every other configured host - the same non-fatal shape loadSkills
// uses for a bad skill directory.
func TestResolveSCPConfigSkipsBadEntryButKeepsOthers(t *testing.T) {
	cfg, warnings := resolveSCPConfig("good=moo@goodhost/root,bad-entry-no-root", "", "", 0, 0)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the malformed entry, got %d: %v", len(warnings), warnings)
	}
	if _, ok := cfg.Hosts["good"]; !ok {
		t.Fatalf("expected the well-formed entry to still be loaded, got hosts: %+v", cfg.Hosts)
	}
	if len(cfg.Hosts) != 1 {
		t.Fatalf("expected exactly 1 loaded host, got %d: %+v", len(cfg.Hosts), cfg.Hosts)
	}
}

// TestResolveSCPConfigWarnsOnDuplicateAlias confirms a second entry reusing
// an already-seen alias is rejected with a warning and the first
// definition wins, rather than silently overwriting it.
func TestResolveSCPConfigWarnsOnDuplicateAlias(t *testing.T) {
	cfg, warnings := resolveSCPConfig("dup=moo@first/root1,dup=moo@second/root2", "", "", 0, 0)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ซ้ำ") {
		t.Fatalf("expected exactly 1 duplicate-alias warning, got: %v", warnings)
	}
	if cfg.Hosts["dup"].Host != "first" {
		t.Fatalf("expected the FIRST definition of a duplicate alias to win, got host: %s", cfg.Hosts["dup"].Host)
	}
}

func TestResolveSCPConfigDefaultsLocalRootToCwd(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	cfg, _ := resolveSCPConfig("alias=moo@host/root", "", "", 0, 0)
	// Resolve symlinks on both sides (macOS/some CI temp dirs are
	// themselves behind a symlink) so this comparison is robust.
	wantAbs, _ := filepath.EvalSymlinks(dir)
	gotAbs, _ := filepath.EvalSymlinks(cfg.LocalRoot)
	if gotAbs != wantAbs {
		t.Fatalf("expected LocalRoot to default to cwd (%s), got %s", wantAbs, gotAbs)
	}
}

func TestResolveSCPConfigTimeoutAndMaxBytesDefaults(t *testing.T) {
	cfg, _ := resolveSCPConfig("alias=moo@host/root", "", "", 0, 0)
	if cfg.Timeout != defaultSCPTimeoutSec*time.Second {
		t.Fatalf("expected default timeout %ds, got %s", defaultSCPTimeoutSec, cfg.Timeout)
	}
	if cfg.MaxBytes != defaultSCPMaxBytes {
		t.Fatalf("expected default max bytes %d, got %d", defaultSCPMaxBytes, cfg.MaxBytes)
	}
}

// ─────────────────────────────────────────────────────────────────
// remoteSandboxedPath
// ─────────────────────────────────────────────────────────────────

func TestRemoteSandboxedPathAllowsSubpath(t *testing.T) {
	got, err := remoteSandboxedPath("/srv/backup", "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/srv/backup/sub/dir/file.txt" {
		t.Fatalf("got %q", got)
	}
}

func TestRemoteSandboxedPathRejectsTraversal(t *testing.T) {
	cases := []string{"../etc/passwd", "../../root/.ssh/id_rsa", "sub/../../etc/passwd"}
	for _, rel := range cases {
		if _, err := remoteSandboxedPath("/srv/backup", rel); err == nil {
			t.Errorf("remoteSandboxedPath(%q) expected traversal to be rejected", rel)
		}
	}
}

func TestRemoteSandboxedPathRejectsEmpty(t *testing.T) {
	if _, err := remoteSandboxedPath("/srv/backup", ""); err == nil {
		t.Fatal("expected empty remote_path to be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────
// toolSCPCopy - validation paths that never touch the network/subprocess
// ─────────────────────────────────────────────────────────────────

func testSCPConfig(t *testing.T, localRoot string) scpConfig {
	t.Helper()
	return scpConfig{
		Hosts: map[string]scpHost{
			"backup": {Alias: "backup", User: "moo", Host: "testhost", Port: "22", RemoteRoot: "/"},
		},
		HostOrder: []string{"backup"},
		LocalRoot: localRoot,
		Timeout:   5 * time.Second,
		MaxBytes:  1 << 20,
	}
}

func TestToolSCPCopyDisabledWithEmptyConfig(t *testing.T) {
	if _, err := toolSCPCopy(map[string]interface{}{}, scpConfig{}); err == nil {
		t.Fatal("expected an error when scp_copy is not configured")
	}
}

func TestToolSCPCopyRejectsBadDirection(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "sideways", "remote_alias": "backup",
		"local_path": "f.txt", "remote_path": "f.txt", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected an invalid direction to be rejected")
	}
}

func TestToolSCPCopyRejectsUnknownAlias(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "not-configured",
		"local_path": "f.txt", "remote_path": "f.txt", "reason": "test",
	}
	_, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected an unknown remote_alias to be rejected")
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Fatalf("expected the error to list the allowed alias(es), got: %v", err)
	}
}

func TestToolSCPCopyRejectsLocalPathEscape(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "../../etc/passwd", "remote_path": "f.txt", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected a local_path escaping the sandbox to be rejected")
	}
}

func TestToolSCPCopyRejectsRemotePathEscape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := testSCPConfig(t, dir)
	cfg.Hosts["backup"] = scpHost{Alias: "backup", User: "moo", Host: "testhost", Port: "22", RemoteRoot: "/srv/backup"}
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "f.txt", "remote_path": "../../etc/passwd", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected a remote_path escaping the alias's remote root to be rejected")
	}
}

func TestToolSCPCopyRejectsDirectoryUpload(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := testSCPConfig(t, dir)
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "adir", "remote_path": "adir", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected uploading a directory to be rejected (scp_copy is single-file only)")
	}
}

func TestToolSCPCopyRejectsOversizedUpload(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 2<<20) // 2MB, over the 1MB cap set in testSCPConfig
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte(big), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := testSCPConfig(t, dir)
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "big.bin", "remote_path": "big.bin", "reason": "test",
	}
	_, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected an oversized upload to be rejected before ever touching the network")
	}
	if !strings.Contains(err.Error(), "เกินขีดจำกัด") {
		t.Fatalf("expected the size-cap error to explain itself, got: %v", err)
	}
}

func TestToolSCPCopyRejectsMissingLocalSourceOnUpload(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "does-not-exist.txt", "remote_path": "f.txt", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected uploading a nonexistent local file to be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────
// Subprocess-level tests using a fake `scp` binary on PATH, mirroring how
// coding_test.go's TestRunShellCommandTimeout/TestToolRunCommandExecutesAllowedCommand
// exercise real subprocess behavior rather than mocking exec.Command away.
// The fake binary treats any "user@host:remote/path" endpoint as
// $FAKE_SCP_REMOTE_ROOT/remote/path, which lets upload/download be
// exercised end-to-end (argv construction, timeout/process-group kill,
// exit-code handling) without a real SSH server or network access.
// ─────────────────────────────────────────────────────────────────

const fakeSCPScript = `#!/bin/sh
# Fake scp for ola's tests: strips the ssh-only flags scp_copy always
# passes (-q, -P <port>, -o <opt> x2, optional -i <key>), then treats
# whichever of the two remaining positional args looks like
# "user@host:path" as living under $FAKE_SCP_REMOTE_ROOT instead of a real
# remote host.
skip_next=0
src=""
dst=""
for a in "$@"; do
  if [ "$skip_next" = "1" ]; then skip_next=0; continue; fi
  case "$a" in
    -q) continue ;;
    -P) skip_next=1; continue ;;
    -o) skip_next=1; continue ;;
    -i) skip_next=1; continue ;;
    *)
      if [ -z "$src" ]; then src="$a"; else dst="$a"; fi
      ;;
  esac
done
resolve() {
  case "$1" in
    *@*:*) printf '%s' "$FAKE_SCP_REMOTE_ROOT/${1#*:}" ;;
    *) printf '%s' "$1" ;;
  esac
}
rsrc=$(resolve "$src")
rdst=$(resolve "$dst")
mkdir -p "$(dirname "$rdst")" || exit 1
cp "$rsrc" "$rdst"
`

const fakeSCPScriptTimeout = `#!/bin/sh
sleep 5
`

// installFakeSCP writes the given script as an executable "scp" and
// prepends its directory to PATH for the duration of the test, so
// exec.Command("scp", ...) inside runSCPCommand picks it up instead of any
// real scp installed on the machine running the test.
func installFakeSCP(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake scp shell script requires a POSIX shell")
	}
	dir := t.TempDir()
	scpPath := filepath.Join(dir, "scp")
	if err := os.WriteFile(scpPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
}

func TestToolSCPCopyUploadDownloadRoundTrip(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	localDir := t.TempDir()
	remoteDir := t.TempDir() // stands in for the whole remote filesystem, rooted at "/"
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	const content = "สวัสดีจาก ola scp_copy test\n"
	if err := os.WriteFile(filepath.Join(localDir, "upload-me.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testSCPConfig(t, localDir)

	// Upload: local -> "remote" (really remoteDir on disk, via the fake binary)
	uploadArgs := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "upload-me.txt", "remote_path": "uploaded.txt", "reason": "ทดสอบ upload",
	}
	result, err := toolSCPCopy(uploadArgs, cfg)
	if err != nil {
		t.Fatalf("expected upload to succeed, got error: %v (result: %s)", err, result)
	}
	if !strings.Contains(result, "upload") {
		t.Fatalf("expected the success message to mention the direction, got: %s", result)
	}
	uploaded, err := os.ReadFile(filepath.Join(remoteDir, "uploaded.txt"))
	if err != nil {
		t.Fatalf("expected the file to land in the fake remote root: %v", err)
	}
	if string(uploaded) != content {
		t.Fatalf("expected uploaded content to match, got: %q", uploaded)
	}

	// Download: "remote" -> local, a different file this time.
	if err := os.WriteFile(filepath.Join(remoteDir, "on-remote.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	downloadArgs := map[string]interface{}{
		"direction": "download", "remote_alias": "backup",
		"local_path": "downloaded.txt", "remote_path": "on-remote.txt", "reason": "ทดสอบ download",
	}
	result, err = toolSCPCopy(downloadArgs, cfg)
	if err != nil {
		t.Fatalf("expected download to succeed, got error: %v (result: %s)", err, result)
	}
	downloaded, err := os.ReadFile(filepath.Join(localDir, "downloaded.txt"))
	if err != nil {
		t.Fatalf("expected the downloaded file to land in the local sandbox: %v", err)
	}
	if string(downloaded) != content {
		t.Fatalf("expected downloaded content to match, got: %q", downloaded)
	}
}

// TestToolSCPCopyRejectsOversizedDownloadAfterTransfer confirms the
// post-transfer size check (the only option for downloads, since scp gives
// no way to know the remote file's size up front) actually deletes the
// oversized file rather than leaving it sitting in the sandbox.
func TestToolSCPCopyRejectsOversizedDownloadAfterTransfer(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	localDir := t.TempDir()
	remoteDir := t.TempDir()
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	big := strings.Repeat("y", 2<<20) // 2MB, over the 1MB cap
	if err := os.WriteFile(filepath.Join(remoteDir, "too-big.bin"), []byte(big), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testSCPConfig(t, localDir)
	args := map[string]interface{}{
		"direction": "download", "remote_alias": "backup",
		"local_path": "too-big.bin", "remote_path": "too-big.bin", "reason": "test",
	}
	_, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected an oversized download to be rejected after transfer")
	}
	if _, statErr := os.Stat(filepath.Join(localDir, "too-big.bin")); !os.IsNotExist(statErr) {
		t.Fatal("expected the oversized downloaded file to be deleted, but it still exists")
	}
}

// TestToolSCPCopyPropagatesNonZeroExit confirms a failing transfer (fake
// scp exits non-zero because the source doesn't exist on the "remote"
// side) surfaces as an error with the exit code visible in the result,
// mirroring toolRunCommand's exit_code reporting.
func TestToolSCPCopyPropagatesNonZeroExit(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	localDir := t.TempDir()
	remoteDir := t.TempDir() // left empty - nothing to download
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	cfg := testSCPConfig(t, localDir)
	args := map[string]interface{}{
		"direction": "download", "remote_alias": "backup",
		"local_path": "nope.txt", "remote_path": "nope.txt", "reason": "test",
	}
	result, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected a failing transfer to return an error")
	}
	if !strings.Contains(result, "exit_code=") {
		t.Fatalf("expected the result to report an exit_code, got: %s", result)
	}
}

func TestRunSCPCommandTimeout(t *testing.T) {
	installFakeSCP(t, fakeSCPScriptTimeout)

	_, exitCode, err := runSCPCommand([]string{"-q", "src", "dst"}, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if exitCode != -1 {
		t.Fatalf("expected exitCode -1 on timeout, got %d", exitCode)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected the error to mention timeout, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────
// formatSCPNotification
// ─────────────────────────────────────────────────────────────────

func TestFormatSCPNotificationIncludesBothSidesAndReason(t *testing.T) {
	got := formatSCPNotification("upload", "backup", "logs/app.log", "incoming/app.log", "ส่ง log ประจำวันไปสำรอง")
	if !strings.Contains(got, "UPLOAD") {
		t.Fatalf("expected the direction to be shown uppercased, got: %s", got)
	}
	for _, want := range []string{"logs/app.log", "backup", "incoming/app.log", "ส่ง log ประจำวันไปสำรอง"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected notification to contain %q, got: %s", want, got)
		}
	}
}

func TestFormatSCPNotificationHandlesEmptyReason(t *testing.T) {
	got := formatSCPNotification("download", "nas", "a.txt", "b.txt", "")
	if strings.Contains(got, " - ") {
		t.Fatalf("expected no dangling separator when reason is empty, got: %s", got)
	}
}

// ======================================================================
// search_test.go
// ======================================================================

func TestHtmlToTextStripsScriptStyleTagsAndExtractsTitle(t *testing.T) {
	page := `<!DOCTYPE html>
<html><head><title>My Page &amp; Friends</title>
<style>body { color: red; }</style>
<script>alert('hi'); var x = "<not a real tag>";</script>
</head>
<body>
<nav>Home | About</nav>
<h1>Welcome</h1>
<p>This is a <b>paragraph</b> with <i>inline</i> tags.</p>
<!-- a comment that should vanish -->
<p>Second paragraph here.</p>
</body></html>`

	title, text := htmlToText(page)
	if title != "My Page & Friends" {
		t.Fatalf("expected title to be extracted and entity-unescaped, got %q", title)
	}
	if strings.Contains(text, "alert(") || strings.Contains(text, "color: red") {
		t.Fatalf("expected script/style contents to be stripped entirely, got: %s", text)
	}
	if strings.Contains(text, "a comment that should vanish") {
		t.Fatalf("expected HTML comments to be stripped, got: %s", text)
	}
	if strings.Contains(text, "<") || strings.Contains(text, ">") {
		t.Fatalf("expected all tags to be stripped, got: %s", text)
	}
	if !strings.Contains(text, "Welcome") || !strings.Contains(text, "paragraph") || !strings.Contains(text, "Second paragraph") {
		t.Fatalf("expected visible text to survive extraction, got: %s", text)
	}
}

// TestDoDirectFetchRunsInParallel confirms doDirectFetch is safe and fast
// to call concurrently (goroutine-safe, no shared mutable state) against a
// mock HTML server - mirroring the worker-pool pattern toolWebFetch itself
// uses (see TestToolWebFetchRunsURLsInParallel for the end-to-end version
// through toolWebFetch/fetchOneDirect). It calls doDirectFetch directly
// (not fetchOneDirect/toolWebFetch) because the mock server necessarily
// lives on loopback, which the production SSRF guard in fetchOneDirect
// correctly rejects as a fetch *target* - see
// TestToolWebFetchDirectModeRejectsPrivateURLs for that guard's own test.
func TestDoDirectFetchRunsInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><head><title>Page %s</title></head><body><p>content for %s</p></body></html>", r.URL.Path, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	paths := []string{"/a", "/b", "/c", "/d"}
	results := make([]string, len(paths))
	var wg sync.WaitGroup

	start := time.Now()
	for i, p := range paths {
		wg.Add(1)
		go func(idx int, path string) {
			defer wg.Done()
			r, err := doDirectFetch(client, srv.URL+path)
			if err != nil {
				t.Errorf("doDirectFetch(%s) failed: %v", path, err)
				return
			}
			results[idx] = r
		}(i, p)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 direct HTTP hits, got %d", hits)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent execution (~150ms), took %s - looks serial", elapsed)
	}
	for i, p := range paths {
		if !strings.Contains(results[i], "content for "+p) {
			t.Fatalf("expected extracted text for %q, got: %s", p, results[i])
		}
	}
}

func TestDoDirectFetchNonHTMLPassthrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/data.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"hello":"world"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	result, err := doDirectFetch(client, srv.URL+"/data.json")
	if err != nil {
		t.Fatalf("expected JSON content-type to pass through, got err: %v", err)
	}
	if !strings.Contains(result, `"hello":"world"`) {
		t.Fatalf("expected raw JSON body to be returned as-is, got: %s", result)
	}
}

// TestDoDirectFetchErrorsOnJSOnlyPages confirms a page whose body is
// essentially an empty shell (typical of a client-side-rendered SPA with no
// server-rendered content) produces a helpful, honest error saying
// web_fetch cannot execute JavaScript, rather than silently returning
// nothing useful or pointing at a scrape mode that no longer exists.
func TestDoDirectFetchErrorsOnJSOnlyPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><script src="/app.js"></script></head><body><div id="root"></div></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := doDirectFetch(client, srv.URL)
	if err == nil {
		t.Fatal("expected an error for a page with no text content after stripping HTML")
	}
	if !strings.Contains(err.Error(), "JavaScript") {
		t.Fatalf("expected a hint about JS-rendered content, got: %v", err)
	}
}

func TestToolWebFetchDirectModeRejectsPrivateURLs(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	result, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{"http://127.0.0.1:9/admin"}}, cfg)
	if err != nil {
		t.Fatalf("expected batch call to succeed with an ERROR slot, got err: %v", err)
	}
	if !strings.Contains(result, "ERROR") {
		t.Fatalf("expected the SSRF guard to reject a private-IP URL, got: %s", result)
	}
}

func TestResolveSearchConfigFetchEnabledByDefault(t *testing.T) {
	// web_fetch needs no configuration at all - it must be enabled the
	// instant a session isn't explicitly disabled with --no-web-search.
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	if !cfg.fetchEnabled() {
		t.Fatal("expected fetchEnabled() true by default with no flags/env set at all")
	}
}

func TestResolveSearchConfigFlagOverridesEnv(t *testing.T) {
	os.Setenv("OLA_SEARXNG_API_BASE", "http://env-searxng:8080")
	os.Setenv("OLA_SEARCH_MAX_RESULTS", "9")
	defer os.Unsetenv("OLA_SEARXNG_API_BASE")
	defer os.Unsetenv("OLA_SEARCH_MAX_RESULTS")

	cfg := resolveSearchConfig("http://flag-searxng:9000", 0, 0, 0, 0, 0, false)
	if cfg.SearXNGBase != "http://flag-searxng:9000" {
		t.Fatalf("expected flag to win over env, got %q", cfg.SearXNGBase)
	}
	if cfg.MaxResults != 9 {
		t.Fatalf("expected env fallback for max results, got %d", cfg.MaxResults)
	}
	if !cfg.searchEnabled() {
		t.Fatal("expected searchEnabled() true when SearXNGBase is set")
	}
	if !cfg.fetchEnabled() {
		t.Fatal("expected fetchEnabled() true regardless of SearXNG configuration (web_fetch is always-on)")
	}
}

func TestResolveSearchConfigDisableWins(t *testing.T) {
	os.Setenv("OLA_SEARXNG_API_BASE", "http://env-searxng:8080")
	defer os.Unsetenv("OLA_SEARXNG_API_BASE")

	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, true /* --no-web-search */)
	if cfg.searchEnabled() || cfg.fetchEnabled() {
		t.Fatalf("expected --no-web-search to force both tools off, got %+v", cfg)
	}
}

func TestResolveSearchConfigDefaults(t *testing.T) {
	cfg := resolveSearchConfig("http://x:1", 0, 0, 0, 0, 0, false)
	if cfg.MaxResults != defaultSearchMaxResults {
		t.Fatalf("expected default max results %d, got %d", defaultSearchMaxResults, cfg.MaxResults)
	}
	if cfg.SearchConcurrency != defaultSearchConcurrency {
		t.Fatalf("expected default search concurrency %d, got %d", defaultSearchConcurrency, cfg.SearchConcurrency)
	}
	if cfg.FetchConcurrency != defaultFetchConcurrency {
		t.Fatalf("expected default fetch concurrency %d, got %d", defaultFetchConcurrency, cfg.FetchConcurrency)
	}
}

// TestToolWebSearchRunsQueriesInParallel spins up a SearXNG-shaped mock that
// sleeps 150ms per request, fires 4 queries with concurrency=4, and asserts
// the whole batch finishes in well under 4*150ms - proving the fan-out is
// actually concurrent, not a serial loop with a concurrency knob that does
// nothing.
func TestToolWebSearchRunsQueriesInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		q := r.URL.Query().Get("q")
		resp := searxngResponse{Results: []searxngResult{
			{Title: "result for " + q, URL: "https://example.com/" + q, Content: "some content about " + q},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 4, 0, 5, 0, false)
	args := map[string]interface{}{
		"queries": []interface{}{"golang", "ollama", "searxng", "ripgrep"},
	}

	start := time.Now()
	result, err := toolWebSearch(args, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 upstream hits, got %d", hits)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent fan-out (~150ms), took %s - looks serial", elapsed)
	}
	for _, q := range []string{"golang", "ollama", "searxng", "ripgrep"} {
		if !strings.Contains(result, q) {
			t.Fatalf("expected result to mention query %q, got: %s", q, result)
		}
	}
}

func TestToolWebSearchDisabledReturnsError(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	_, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"x"}}, cfg)
	if err == nil {
		t.Fatal("expected error when web_search is not configured")
	}
}

func TestToolWebSearchPartialFailureStillReturnsGoodResults(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		resp := searxngResponse{Results: []searxngResult{{Title: "ok", URL: "https://example.com", Content: "fine"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 2, 0, 5, 0, false)
	result, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"good", "bad"}}, cfg)
	if err != nil {
		t.Fatalf("expected batch call itself to succeed even with one bad query, got err: %v", err)
	}
	if !strings.Contains(result, "ERROR") {
		t.Fatalf("expected the failing query's slot to carry an ERROR marker, got: %s", result)
	}
	if !strings.Contains(result, "ok") {
		t.Fatalf("expected the succeeding query's result to still be present, got: %s", result)
	}
}

// TestToolWebFetchRunsURLsInParallel mirrors the search concurrency test,
// but against a plain direct-mode HTML server (web_fetch's only mode).
//
// toolWebFetch's SSRF guard (validateFetchURL) rejects the target URL if it
// resolves to a loopback/private address, which a plain httptest.Server
// URL always does - so, unlike the old shim-mode version of this test
// (which only ever talked to a *local scrape service*, never the target
// URL itself), we can't just point straight at srv.URL. Instead: use URLs
// on the reserved, guaranteed-NXDOMAIN ".invalid" TLD (RFC 2606) - a failed
// DNS lookup makes validateFetchURL let the URL through (see its "DNS
// hiccup/offline" comment) - and swap in a RoundTripper that redirects the
// actual dial to the local test server regardless of the requested host,
// so the fetch still really happens end-to-end.
func TestToolWebFetchRunsURLsInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><p>content for %s</p></body></html>", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &redirectAllTransport{target: srv.Listener.Addr().String()}
	defer func() { http.DefaultTransport = origTransport }()

	cfg := resolveSearchConfig("", 0, 0, 4, 0, 5, false)
	urls := []interface{}{
		"http://a.example.invalid/a", "http://b.example.invalid/b",
		"http://c.example.invalid/c", "http://d.example.invalid/d",
	}

	start := time.Now()
	result, err := toolWebFetch(map[string]interface{}{"urls": urls}, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("toolWebFetch returned error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 upstream hits, got %d - result was: %s", hits, result)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent fan-out (~150ms), took %s - looks serial", elapsed)
	}
	for _, u := range []string{"/a", "/b", "/c", "/d"} {
		if !strings.Contains(result, "content for "+u) {
			t.Fatalf("expected result to mention content for %q, got: %s", u, result)
		}
	}
}

// redirectAllTransport is a test-only http.RoundTripper that dials target
// no matter what host/port the request was addressed to - used above to
// let a fetch to a fake, never-resolving hostname actually land on a local
// httptest.Server.
type redirectAllTransport struct {
	target string
	base   http.Transport
}

func (rt *redirectAllTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = rt.target
	return rt.base.RoundTrip(req)
}

func TestToolWebFetchRejectsPrivateAndLocalURLs(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	cases := []string{
		"http://localhost:11434/api/tags",
		"http://127.0.0.1:8080/admin",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
	}
	for _, u := range cases {
		result, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{u}}, cfg)
		if err != nil {
			t.Fatalf("toolWebFetch batch call itself should not hard-fail for %q, got err: %v", u, err)
		}
		if !strings.Contains(result, "ERROR") {
			t.Fatalf("expected %q to be rejected by the SSRF guard, got: %s", u, result)
		}
	}
}

func TestValidateFetchURLAllowsPublicHTTPS(t *testing.T) {
	if err := validateFetchURL("https://example.com/some/page"); err != nil {
		t.Fatalf("expected a plain public https URL to be allowed, got: %v", err)
	}
}

func TestToolWebFetchDisabledReturnsError(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, true /* --no-web-search */)
	_, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{"https://example.com"}}, cfg)
	if err == nil {
		t.Fatal("expected error when web_fetch has been explicitly disabled via --no-web-search")
	}
}

func TestTruncateText(t *testing.T) {
	short := "hello"
	if truncateText(short, 100) != short {
		t.Fatal("expected short text to pass through unchanged")
	}
	long := strings.Repeat("x", 100)
	got := truncateText(long, 10)
	if len(got) <= 10 {
		t.Fatal("expected truncation marker to be appended, making output longer than the limit")
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) {
		t.Fatalf("expected truncated output to start with the first 10 chars, got: %s", got)
	}
}

func TestWebSearchToolNotOfferedWhenDisabled_sanity(t *testing.T) {
	// Sanity check for the schema constants used by main.go/coding.go when
	// deciding whether to append these tools to a request's tool list.
	if webSearchTool.Function.Name != "web_search" {
		t.Fatalf("unexpected web_search tool name: %s", webSearchTool.Function.Name)
	}
	if webFetchTool.Function.Name != "web_fetch" {
		t.Fatalf("unexpected web_fetch tool name: %s", webFetchTool.Function.Name)
	}
}

// ─────────────────────────────────────────────────────────────────
// Ollama Web Search API backend (no self-hosted SearXNG required) +
// backend precedence + the terminal/log "found N results, here's every
// title+link" summary that rides on top of whichever backend actually ran.
// ─────────────────────────────────────────────────────────────────

func TestResolveOllamaSearchConfigFlagOverridesEnv(t *testing.T) {
	os.Setenv("OLA_OLLAMA_SEARCH_API_KEY", "env-key")
	os.Setenv("OLLAMA_API_KEY", "generic-env-key")
	os.Setenv("OLA_OLLAMA_SEARCH_API_BASE", "http://mock-ollama:1234")
	defer os.Unsetenv("OLA_OLLAMA_SEARCH_API_KEY")
	defer os.Unsetenv("OLLAMA_API_KEY")
	defer os.Unsetenv("OLA_OLLAMA_SEARCH_API_BASE")

	apiKey, base := resolveOllamaSearchConfig("flag-key")
	if apiKey != "flag-key" {
		t.Fatalf("expected --ollama-search-key flag to win over both env vars, got %q", apiKey)
	}
	if base != "http://mock-ollama:1234" {
		t.Fatalf("expected OLA_OLLAMA_SEARCH_API_BASE to override the default base, got %q", base)
	}
}

func TestResolveOllamaSearchConfigFallsBackToGenericOllamaAPIKeyEnv(t *testing.T) {
	os.Unsetenv("OLA_OLLAMA_SEARCH_API_KEY")
	os.Setenv("OLLAMA_API_KEY", "generic-env-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	// No flag, no ola-specific env var - must fall back to the standard
	// OLLAMA_API_KEY name that Ollama's own CLI/Python/JS libraries use, so
	// a machine already configured for `ollama.web_search` needs no
	// ola-specific setup at all.
	apiKey, base := resolveOllamaSearchConfig("")
	if apiKey != "generic-env-key" {
		t.Fatalf("expected fallback to $OLLAMA_API_KEY, got %q", apiKey)
	}
	if base != defaultOllamaSearchBase {
		t.Fatalf("expected default base %q when OLA_OLLAMA_SEARCH_API_BASE is unset, got %q", defaultOllamaSearchBase, base)
	}
}

func TestSearchConfigSearchEnabledViaOllamaKeyAlone(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	cfg.OllamaAPIKey, cfg.OllamaBase = resolveOllamaSearchConfig("some-key")
	if !cfg.searchEnabled() {
		t.Fatal("expected searchEnabled() true when only OllamaAPIKey is set (no SearXNG at all)")
	}
}

func TestSearchBackendLabel(t *testing.T) {
	disabled := searchConfig{}
	if got := disabled.searchBackendLabel(); got != "disabled" {
		t.Fatalf("expected %q for an all-zero config, got %q", "disabled", got)
	}
	ollamaOnly := searchConfig{OllamaAPIKey: "k", OllamaBase: "https://ollama.com"}
	if got := ollamaOnly.searchBackendLabel(); !strings.Contains(got, "Ollama") {
		t.Fatalf("expected label to mention Ollama, got %q", got)
	}
	both := searchConfig{SearXNGBase: "http://searxng:8080", OllamaAPIKey: "k", OllamaBase: "https://ollama.com"}
	if got := both.searchBackendLabel(); !strings.Contains(got, "SearXNG") {
		t.Fatalf("expected SearXNG to win the label when both backends are configured, got %q", got)
	}
}

// TestToolWebSearchOllamaBackendRunsQueriesInParallel mirrors
// TestToolWebSearchRunsQueriesInParallel but against a mock shaped like
// Ollama's hosted Web Search API (POST /api/web_search, Bearer auth,
// {"results":[{"title","url","content"}]}) instead of SearXNG - confirming
// the new backend is wired into toolWebSearch's concurrent fan-out
// identically to the original one.
func TestToolWebSearchOllamaBackendRunsQueriesInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/web_search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-ollama-key" {
			t.Errorf("expected Bearer auth with the configured key, got %q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		time.Sleep(150 * time.Millisecond)
		resp := ollamaSearchResponse{Results: []ollamaSearchResult{
			{Title: "result for " + body["query"], URL: "https://example.com/" + body["query"], Content: "some content about " + body["query"]},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No SearXNG configured at all - only the Ollama backend.
	cfg := resolveSearchConfig("", 0, 4, 0, 5, 0, false)
	cfg.OllamaAPIKey = "test-ollama-key"
	cfg.OllamaBase = srv.URL

	args := map[string]interface{}{
		"queries": []interface{}{"golang", "ollama", "searxng", "ripgrep"},
	}

	start := time.Now()
	result, err := toolWebSearch(args, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 upstream hits, got %d", hits)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent fan-out (~150ms), took %s - looks serial", elapsed)
	}
	for _, q := range []string{"golang", "ollama", "searxng", "ripgrep"} {
		if !strings.Contains(result, q) {
			t.Fatalf("expected result to mention query %q, got: %s", q, result)
		}
	}
}

// TestToolWebSearchOllamaBackendRejectsBadKey confirms an HTTP
// 401/403 from Ollama's Web Search API (bad/missing key) surfaces as a
// clear, actionable error mentioning the relevant env vars/flag, not a
// generic JSON-parse failure.
func TestToolWebSearchOllamaBackendRejectsBadKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/web_search", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig("", 0, 0, 0, 5, 0, false)
	cfg.OllamaAPIKey = "bad-key"
	cfg.OllamaBase = srv.URL

	result, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"x"}}, cfg)
	if err != nil {
		t.Fatalf("expected the batch call itself to succeed with an ERROR slot, got err: %v", err)
	}
	if !strings.Contains(result, "ERROR") || !strings.Contains(result, "API key") {
		t.Fatalf("expected a clear API-key error mentioning 'API key', got: %s", result)
	}
}

// TestToolWebSearchSearXNGWinsWhenBothBackendsConfigured confirms the
// documented precedence rule: if a session has both OLA_SEARXNG_API_BASE
// and an Ollama Web Search API key configured, SearXNG is the one actually
// used (only its mock server receives hits) - preserving prior behavior
// for anyone who already had SearXNG configured before this backend
// existed.
func TestToolWebSearchSearXNGWinsWhenBothBackendsConfigured(t *testing.T) {
	var searxngHits, ollamaHits int32
	searxngMux := http.NewServeMux()
	searxngMux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searxngHits, 1)
		resp := searxngResponse{Results: []searxngResult{{Title: "from searxng", URL: "https://searxng.example.com", Content: "c"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	searxngSrv := httptest.NewServer(searxngMux)
	defer searxngSrv.Close()

	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/web_search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaHits, 1)
		resp := ollamaSearchResponse{Results: []ollamaSearchResult{{Title: "from ollama", URL: "https://ollama.example.com", Content: "c"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	cfg := resolveSearchConfig(searxngSrv.URL, 0, 0, 0, 5, 0, false)
	cfg.OllamaAPIKey = "some-key"
	cfg.OllamaBase = ollamaSrv.URL

	result, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"x"}}, cfg)
	if err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}
	if atomic.LoadInt32(&searxngHits) != 1 {
		t.Fatalf("expected SearXNG to be hit exactly once, got %d", searxngHits)
	}
	if atomic.LoadInt32(&ollamaHits) != 0 {
		t.Fatalf("expected Ollama Web Search API to NOT be hit when SearXNG is also configured, got %d hits", ollamaHits)
	}
	if !strings.Contains(result, "from searxng") || strings.Contains(result, "from ollama") {
		t.Fatalf("expected result to come from SearXNG only, got: %s", result)
	}
}

// TestToolWebSearchPublishesStructuredItemsForTerminalSummary confirms
// toolWebSearch stashes the same title/url data (per query, including
// per-query errors) that dispatchToolCall's terminal/log summary reads via
// popLastWebSearchItems - and that popping clears it, so a session that
// runs web_search twice never shows stale results from the first call.
func TestToolWebSearchPublishesStructuredItemsForTerminalSummary(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := searxngResponse{Results: []searxngResult{
			{Title: "Title for " + q, URL: "https://example.com/" + q, Content: "content"},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 2, 0, 5, 0, false)

	if _, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"good", "bad"}}, cfg); err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}

	items := popLastWebSearchItems()
	if len(items) != 2 {
		t.Fatalf("expected 2 published query-item groups, got %d", len(items))
	}
	var sawGood, sawBad bool
	for _, qi := range items {
		switch qi.Query {
		case "good":
			sawGood = true
			if qi.Err != nil {
				t.Fatalf("expected no error for 'good' query, got: %v", qi.Err)
			}
			if len(qi.Items) != 1 || qi.Items[0].Title != "Title for good" || qi.Items[0].URL != "https://example.com/good" {
				t.Fatalf("unexpected items for 'good' query: %+v", qi.Items)
			}
		case "bad":
			sawBad = true
			if qi.Err == nil {
				t.Fatal("expected an error to be published for the 'bad' query")
			}
		}
	}
	if !sawGood || !sawBad {
		t.Fatalf("expected both 'good' and 'bad' queries to be represented, got: %+v", items)
	}

	// Popping clears the side-channel.
	if again := popLastWebSearchItems(); again != nil {
		t.Fatalf("expected popLastWebSearchItems to clear after popping once, got: %+v", again)
	}
}

// TestDispatchToolCallWebSearchLogsSummary drives web_search through the
// real dispatchToolCall path (the same one "ask" and "coding" both use) and
// confirms the -o log file gets a "[web_search_summary]" line reporting the
// total result count across all queries, plus every single result's
// title+link grouped by query - independent of, and un-truncated compared
// to, the generic 300-char [tool_result] preview dispatchToolCall already
// logs for every tool.
func TestDispatchToolCallWebSearchLogsSummary(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		resp := searxngResponse{Results: []searxngResult{
			{Title: "Result A for " + q, URL: "https://a.example.com/" + q, Content: strings.Repeat("x", 500)},
			{Title: "Result B for " + q, URL: "https://b.example.com/" + q, Content: strings.Repeat("y", 500)},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 0, 0, 5, 0, false)

	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"queries": []string{"golang"}})
	tc := toolCall{Function: toolCallFunction{Name: "web_search", Arguments: argsJSON}}
	extra := func(name string, args map[string]interface{}) (string, error, bool) {
		if name == "web_search" {
			r, e := toolWebSearch(args, cfg)
			return r, e, true
		}
		return "", nil, false
	}

	result := dispatchToolCall(tc, "", "", "", outFile, extra)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected web_search to succeed, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(logged)
	if !strings.Contains(logStr, "[web_search_summary] 2 ผลลัพธ์ทั้งหมด จาก 1 คำค้น") {
		t.Fatalf("expected a summary line with the total result count (2) and query count (1), got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "Result A for golang") || !strings.Contains(logStr, "https://a.example.com/golang") {
		t.Fatalf("expected the first result's title+link to appear in full, got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "Result B for golang") || !strings.Contains(logStr, "https://b.example.com/golang") {
		t.Fatalf("expected the second result's title+link to appear in full, got:\n%s", logStr)
	}
}

// ======================================================================
// skills_test.go
// ======================================================================

// ─────────────────────────────────────────────────────────────────
// parseSkillMD
// ─────────────────────────────────────────────────────────────────

func writeSkillMD(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseSkillMDReadsFrontmatter confirms the primary, intended path:
// an explicit name/description in a "---" frontmatter block is used
// as-is, with no fallback guessing needed at all.
func TestParseSkillMDReadsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "---\nname: pptx\ndescription: Use this whenever the user wants slides.\n---\n# PPTX\nBody text here.\n")

	name, desc, err := parseSkillMD(path, "fallback-dir-name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "pptx" {
		t.Fatalf("expected name %q from frontmatter, got %q", "pptx", name)
	}
	if desc != "Use this whenever the user wants slides." {
		t.Fatalf("expected description from frontmatter, got %q", desc)
	}
}

// TestParseSkillMDFallsBackToHeadingAndFirstLine confirms a SKILL.md with
// no frontmatter at all still yields a usable name (from its leading "#"
// heading) and description (the first substantive body line) instead of
// erroring out - most hand-written skills won't bother with frontmatter.
func TestParseSkillMDFallsBackToHeadingAndFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "# My Great Skill\n\nThis is what it does for you.\nMore detail on a second line.\n")

	name, desc, err := parseSkillMD(path, "my-great-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "My Great Skill" {
		t.Fatalf("expected name derived from the leading heading, got %q", name)
	}
	if desc != "This is what it does for you." {
		t.Fatalf("expected description to be the first substantive body line, got %q", desc)
	}
}

// TestParseSkillMDFallsBackToDirNameWithoutHeading confirms a SKILL.md
// that starts directly with prose (no frontmatter, no leading heading)
// falls all the way back to the skill's own directory name, rather than
// misreading the first prose line as a title.
func TestParseSkillMDFallsBackToDirNameWithoutHeading(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "Just a plain description with no heading above it.\n")

	name, desc, err := parseSkillMD(path, "plain-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "plain-skill" {
		t.Fatalf("expected name to fall back to the directory name, got %q", name)
	}
	if desc != "Just a plain description with no heading above it." {
		t.Fatalf("expected the first line to be used as description, got %q", desc)
	}
}

// TestParseSkillMDPartialFrontmatterFillsInMissingField confirms
// frontmatter with only "name:" set still recovers a description from the
// body, rather than leaving it as the "(no description)" placeholder just
// because frontmatter existed at all.
func TestParseSkillMDPartialFrontmatterFillsInMissingField(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "---\nname: partial\n---\n# Heading (ignored - name already set)\nRecovered description line.\n")

	name, desc, err := parseSkillMD(path, "fallback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "partial" {
		t.Fatalf("expected frontmatter name to win, got %q", name)
	}
	if desc != "Recovered description line." {
		t.Fatalf("expected description to be recovered from the body, got %q", desc)
	}
}

// TestParseSkillMDTruncatesLongDescription confirms a single skill's
// description can't blow the system-prompt budget for every session that
// happens to have a skills directory configured - see
// maxSkillDescriptionChars.
func TestParseSkillMDTruncatesLongDescription(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", maxSkillDescriptionChars+200)
	path := writeSkillMD(t, dir, "---\nname: verbose\ndescription: "+long+"\n---\n")

	_, desc, err := parseSkillMD(path, "fallback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	descRunes := []rune(desc)
	if len(descRunes) != maxSkillDescriptionChars+1 { // +1 for the trailing "…" marker
		t.Fatalf("expected description truncated to %d runes + ellipsis, got %d runes: %q", maxSkillDescriptionChars, len(descRunes), desc)
	}
	if !strings.HasSuffix(desc, "…") {
		t.Fatalf("expected a truncation marker at the end, got %q", desc)
	}
}

// TestParseSkillMDMissingFile confirms a missing SKILL.md surfaces as a
// normal Go error rather than panicking or silently returning empty
// strings - loadSkills relies on this to turn it into a warning.
func TestParseSkillMDMissingFile(t *testing.T) {
	if _, _, err := parseSkillMD(filepath.Join(t.TempDir(), "nope", "SKILL.md"), "fallback"); err == nil {
		t.Fatal("expected an error for a missing SKILL.md")
	}
}

// ─────────────────────────────────────────────────────────────────
// resolveSkillsDirs
// ─────────────────────────────────────────────────────────────────

// TestResolveSkillsDirsFlagOverridesEnv confirms the same flag > env >
// default precedence used throughout ola (see resolveSearchConfig).
func TestResolveSkillsDirsFlagOverridesEnv(t *testing.T) {
	t.Setenv("OLA_SKILLS_DIR", "/from/env")
	got := resolveSkillsDirs("/from/flag")
	if len(got) != 1 || got[0] != "/from/flag" {
		t.Fatalf("expected flag to win over env, got %v", got)
	}
}

// TestResolveSkillsDirsFallsBackToEnv confirms OLA_SKILLS_DIR is used when
// no --skills-dir flag was given.
func TestResolveSkillsDirsFallsBackToEnv(t *testing.T) {
	t.Setenv("OLA_SKILLS_DIR", "/from/env")
	got := resolveSkillsDirs("")
	if len(got) != 1 || got[0] != "/from/env" {
		t.Fatalf("expected env value, got %v", got)
	}
}

// TestResolveSkillsDirsSplitsAndTrimsCommaList mirrors --allow's
// comma-separated convention: multiple directories, extra whitespace and
// empty segments handled gracefully.
func TestResolveSkillsDirsSplitsAndTrimsCommaList(t *testing.T) {
	got := resolveSkillsDirs(" /a/skills , /b/skills ,,/c/skills")
	want := []string{"/a/skills", "/b/skills", "/c/skills"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestResolveSkillsDirsDefaultsToNil confirms skills stay off entirely
// (nil, not an empty-but-non-nil slice someone could accidentally treat as
// "configured") when neither the flag nor the env var is set - there is
// deliberately no default directory, unlike host/model/ctx.
func TestResolveSkillsDirsDefaultsToNil(t *testing.T) {
	t.Setenv("OLA_SKILLS_DIR", "")
	if got := resolveSkillsDirs(""); got != nil {
		t.Fatalf("expected nil when nothing is configured, got %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────
// loadSkills
// ─────────────────────────────────────────────────────────────────

// TestLoadSkillsScansSubdirsAndSkipsNonSkillFolders confirms only
// immediate subdirectories that actually contain a SKILL.md become
// skills - a stray subdirectory without one (e.g. some unrelated folder
// living alongside a skills root) is silently ignored, not an error.
func TestLoadSkillsScansSubdirsAndSkipsNonSkillFolders(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: Slides.\n---\n")
	mustMkdirSkill(t, root, "docx", "---\nname: docx\ndescription: Word docs.\n---\n")
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 2 {
		t.Fatalf("expected exactly 2 skills, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
	// name-sorted: docx before pptx
	if cfg.Skills[0].Name != "docx" || cfg.Skills[1].Name != "pptx" {
		t.Fatalf("expected name-sorted [docx, pptx], got [%s, %s]", cfg.Skills[0].Name, cfg.Skills[1].Name)
	}
	if !cfg.enabled() {
		t.Fatal("expected skillsConfig.enabled() to be true when skills were found")
	}
}

// TestLoadSkillsFirstDirWinsOnDuplicateName confirms a skill name found in
// more than one configured directory keeps the FIRST directory's version
// (matching the documented --skills-dir/OLA_SKILLS_DIR precedence: earlier
// directories win) and records a warning about the shadowed duplicate,
// rather than silently overwriting it or erroring out the whole run.
func TestLoadSkillsFirstDirWinsOnDuplicateName(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	mustMkdirSkill(t, dirA, "shared", "---\nname: shared\ndescription: version A (should win).\n---\n")
	mustMkdirSkill(t, dirB, "shared", "---\nname: shared\ndescription: version B (should be skipped).\n---\n")

	cfg := loadSkills([]string{dirA, dirB})
	if len(cfg.Skills) != 1 {
		t.Fatalf("expected exactly 1 skill after dedup, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
	if cfg.Skills[0].Description != "version A (should win)." {
		t.Fatalf("expected the first directory's version to win, got %q", cfg.Skills[0].Description)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "ชื่อซ้ำ") {
		t.Fatalf("expected exactly one duplicate-name warning, got: %v", cfg.Warnings)
	}
}

// TestLoadSkillsWarnsButDoesNotFailOnMissingDirectory confirms a typo'd or
// nonexistent --skills-dir/OLA_SKILLS_DIR entry degrades to "no skills
// from that directory" plus a warning, rather than making the whole
// session (ask/coding) refuse to start.
func TestLoadSkillsWarnsButDoesNotFailOnMissingDirectory(t *testing.T) {
	cfg := loadSkills([]string{filepath.Join(t.TempDir(), "does-not-exist")})
	if cfg.enabled() {
		t.Fatal("expected no skills to be found")
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "อ่านไม่ได้") {
		t.Fatalf("expected exactly one unreadable-directory warning, got: %v", cfg.Warnings)
	}
}

// TestLoadSkillsCombinesMultipleDirectories confirms distinct skills
// across several configured directories are all loaded together, not just
// the first directory - the comma-separated list is additive.
func TestLoadSkillsCombinesMultipleDirectories(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	mustMkdirSkill(t, dirA, "alpha", "---\nname: alpha\ndescription: from dir A.\n---\n")
	mustMkdirSkill(t, dirB, "beta", "---\nname: beta\ndescription: from dir B.\n---\n")

	cfg := loadSkills([]string{dirA, dirB})
	if len(cfg.Skills) != 2 {
		t.Fatalf("expected 2 skills combined from both directories, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
}

// TestLoadSkillsFindsCategoryNestedSkills is the regression test for the
// "flat scan only" bug: --skills-dir/OLA_SKILLS_DIR previously only ever
// looked one level below the configured directory
// (<dir>/<skill-name>/SKILL.md), so a directory laid out the way
// Anthropic's own Claude products organize skills - grouped one level
// deeper under a category folder, e.g. <dir>/public/pptx/SKILL.md,
// <dir>/user/rust-tokio-secure-systems/SKILL.md - was invisible: none of
// the category folders themselves ("public", "user") contain a SKILL.md,
// so the old scan found nothing at all and silently reported zero skills.
// This confirms skills nested under a category directory are now found,
// and categorized/flat layouts can be mixed under the same root.
func TestLoadSkillsFindsCategoryNestedSkills(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, filepath.Join(root, "public"), "pptx", "---\nname: pptx\ndescription: Slides.\n---\n")
	mustMkdirSkill(t, filepath.Join(root, "user"), "rust-tokio-secure-systems", "---\nname: rust-tokio-secure-systems\ndescription: Rust backends.\n---\n")
	// A flat (non-categorized) skill directly under the same root must
	// still be found too - the two layouts are not mutually exclusive.
	mustMkdirSkill(t, root, "find-skills", "---\nname: find-skills\ndescription: Meta skill.\n---\n")

	cfg := loadSkills([]string{root})
	if len(cfg.Warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", cfg.Warnings)
	}
	got := map[string]bool{}
	for _, s := range cfg.Skills {
		got[s.Name] = true
	}
	for _, want := range []string{"pptx", "rust-tokio-secure-systems", "find-skills"} {
		if !got[want] {
			t.Fatalf("expected skill %q to be found (category-nested or flat), got: %+v", want, cfg.Skills)
		}
	}
	if len(cfg.Skills) != 3 {
		t.Fatalf("expected exactly 3 skills total, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
}

// TestLoadSkillsFindsSymlinkedSkillDir is the regression test for the
// symlink-invisibility bug: os.ReadDir's fs.DirEntry.IsDir() reports the
// type of the directory entry ITSELF and does not follow symlinks, so a
// skill folder that is itself a symlink (a common shape for skills
// directories managed via dotfiles tooling like GNU stow/chezmoi, or a
// symlinked shared/mounted repo) was silently skipped by the old
// !e.IsDir() { continue } check, even though its target was a perfectly
// well-formed skill directory with its own SKILL.md. This confirms a
// symlinked skill directory is now discovered exactly like a real one.
func TestLoadSkillsFindsSymlinkedSkillDir(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	mustMkdirSkill(t, realDir, "slidev", "---\nname: slidev\ndescription: Build Slidev decks.\n---\n")

	link := filepath.Join(root, "slidev")
	if err := os.Symlink(filepath.Join(realDir, "slidev"), link); err != nil {
		t.Skipf("symlinks not supported on this filesystem: %v", err)
	}

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "slidev" {
		t.Fatalf("expected the symlinked skill directory to be found, got %+v (warnings: %v)", cfg.Skills, cfg.Warnings)
	}
}

// TestLoadSkillsStopsAtFirstSkillLevelFound confirms a directory that is
// itself already recognized as a skill (it has its own SKILL.md) is never
// searched further for additional, separately-listed skills nested inside
// it - a companion folder like "references/" is that skill's own material
// (see listSkillFiles), not a place to go looking for more top-level
// skills, even if - hypothetically - a file happened to be named SKILL.md
// somewhere further down inside it.
func TestLoadSkillsStopsAtFirstSkillLevelFound(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "slidev", "---\nname: slidev\ndescription: Build Slidev decks.\n---\n")
	// A stray, coincidentally-named SKILL.md living inside slidev's own
	// references/ folder must not be picked up as a second skill.
	nested := filepath.Join(root, "slidev", "references", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("---\nname: should-not-appear\ndescription: d.\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "slidev" {
		t.Fatalf("expected exactly 1 skill (slidev) with nested content ignored, got %+v", cfg.Skills)
	}
}

// TestLoadSkillsRespectsMaxScanDepth confirms a skill buried deeper than
// maxSkillsScanDepth allows is not found - the depth cap exists precisely
// to keep a mistakenly-broad --skills-dir (e.g. accidentally pointed at a
// huge or unrelated directory tree) from turning into an unbounded
// filesystem walk, so this documents that the cap actually bites.
func TestLoadSkillsRespectsMaxScanDepth(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < maxSkillsScanDepth+2; i++ {
		deep = filepath.Join(deep, "level")
	}
	mustMkdirSkill(t, deep, "too-deep", "---\nname: too-deep\ndescription: d.\n---\n")

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 0 {
		t.Fatalf("expected a skill beyond maxSkillsScanDepth to be out of reach, got: %+v", cfg.Skills)
	}
}

func mustMkdirSkill(t *testing.T, root, name, skillMD string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatal(err)
	}
}

// ─────────────────────────────────────────────────────────────────
// buildSkillsPromptSection
// ─────────────────────────────────────────────────────────────────

// TestBuildSkillsPromptSectionListsEveryNameAndDescription confirms the
// system-prompt injection is a genuine listing of what was loaded, not
// just a static header - the model has to see the actual names/
// descriptions to pick the right skill.
func TestBuildSkillsPromptSectionListsEveryNameAndDescription(t *testing.T) {
	section := buildSkillsPromptSection([]skillInfo{
		{Name: "pptx", Description: "Slide decks."},
		{Name: "docx", Description: "Word documents."},
	})
	if !strings.Contains(section, "AVAILABLE SKILLS") {
		t.Fatalf("expected a clearly-labeled section header, got:\n%s", section)
	}
	if !strings.Contains(section, "pptx: Slide decks.") {
		t.Fatalf("expected the pptx entry, got:\n%s", section)
	}
	if !strings.Contains(section, "docx: Word documents.") {
		t.Fatalf("expected the docx entry, got:\n%s", section)
	}
}

// ─────────────────────────────────────────────────────────────────
// toolReadSkill
// ─────────────────────────────────────────────────────────────────

// TestToolReadSkillReturnsFullContentAndSiblingListing confirms the
// default (no "file" argument) call returns the complete SKILL.md body -
// not just the truncated description used in the system prompt - plus a
// hint about any companion files so the model knows to ask for them by
// name instead of guessing paths.
func TestToolReadSkillReturnsFullContentAndSiblingListing(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "pptx")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	fullBody := "---\nname: pptx\ndescription: short.\n---\n# Full instructions\nLots of detail that would be too long for the system prompt.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(fullBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "template.pptx.md"), []byte("template contents"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	result, err := toolReadSkill(map[string]interface{}{"skill": "pptx"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lots of detail that would be too long for the system prompt.") {
		t.Fatalf("expected the full SKILL.md body, got: %s", result)
	}
	if !strings.Contains(result, "template.pptx.md") {
		t.Fatalf("expected the sibling file to be mentioned so the model knows it exists, got: %s", result)
	}
}

// TestToolReadSkillListsNestedReferenceFilesRecursively is the regression
// test for the shallow-listing bug: real skills commonly nest companion
// docs a level deep (a "references/" folder full of topic-specific .md
// files is the exact shape of Anthropic's own bundled skills, e.g. slidev's
// references/core-syntax.md, references/diagram-mermaid.md, and dozens
// more alongside them). A listing that only reports the top-level entry
// "references" itself is useless to the model - that path isn't a file
// read_skill can return content for - and gives no way to discover the
// real, fetchable paths underneath without already knowing them. This
// confirms the sibling listing walks into subdirectories and reports full,
// slash-joined relative paths instead.
func TestToolReadSkillListsNestedReferenceFilesRecursively(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "slidev")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: slidev\ndescription: Build Slidev decks.\n---\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "README.md"), []byte("readme"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "core-syntax.md"), []byte("core"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "diagram-mermaid.md"), []byte("mermaid"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	result, err := toolReadSkill(map[string]interface{}{"skill": "slidev"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"references/core-syntax.md", "references/diagram-mermaid.md", "README.md"} {
		if !strings.Contains(result, want) {
			t.Fatalf("expected the listing to mention %q, got: %s", want, result)
		}
	}
	// The bare directory name must never appear as if it were itself a
	// fetchable sibling - it isn't a file and read_skill(file="references")
	// would just error out with "is a directory".
	if strings.Contains(result, "): references,") || strings.Contains(result, "): references\n") ||
		strings.HasSuffix(strings.TrimSpace(result), "references)") {
		t.Fatalf("expected the bare 'references' directory name not to be listed as a fetchable file, got: %s", result)
	}
}

// TestToolReadSkillNestedFileFetchableViaListedPath confirms a path
// obtained from the default call's companion-file listing can be fed
// straight back into "file" and actually resolves to that nested file's
// content - i.e. the listing and the fetch mechanism agree with each
// other end-to-end, not just independently.
func TestToolReadSkillNestedFileFetchableViaListedPath(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "slidev")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: slidev\ndescription: d.\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "diagram-mermaid.md"),
		[]byte("mermaid diagram instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	listing, err := toolReadSkill(map[string]interface{}{"skill": "slidev"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error listing: %v", err)
	}
	if !strings.Contains(listing, "references/diagram-mermaid.md") {
		t.Fatalf("expected listing to include the nested path, got: %s", listing)
	}

	content, err := toolReadSkill(map[string]interface{}{"skill": "slidev", "file": "references/diagram-mermaid.md"}, cfg.Skills)
	if err != nil {
		t.Fatalf("expected the exact listed path to be readable, got error: %v", err)
	}
	if content != "mermaid diagram instructions" {
		t.Fatalf("expected the nested file's real content, got: %q", content)
	}
}

// TestListSkillFilesOnlyListsFilesNotDirectories confirms listSkillFiles
// itself never reports a directory as an entry (only leaf files), across
// multiple nesting levels and multiple sibling subfolders - the shape seen
// in real skills that combine e.g. both "references/" and "assets/"
// folders alongside a top-level README.
func TestListSkillFilesOnlyListsFilesNotDirectories(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("SKILL.md", "skill")
	mustWrite("README.md", "readme")
	mustWrite("references/core-syntax.md", "a")
	mustWrite("references/diagram-mermaid.md", "b")
	mustWrite("assets/deep/nested/template.txt", "c")

	got := listSkillFiles(root)
	want := []string{
		"README.md",
		"assets/deep/nested/template.txt",
		"references/core-syntax.md",
		"references/diagram-mermaid.md",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d files, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	for _, g := range got {
		if g == "SKILL.md" {
			t.Fatal("expected SKILL.md itself to be excluded from the companion-file listing")
		}
	}
}

// TestToolReadSkillCaseInsensitiveLookup confirms a model reproducing the
// skill name with different casing than the system prompt still resolves
// correctly, since local models don't always echo identifiers verbatim.
func TestToolReadSkillCaseInsensitiveLookup(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: Slides.\n---\nBody.\n")
	cfg := loadSkills([]string{root})

	if _, err := toolReadSkill(map[string]interface{}{"skill": "PPTX"}, cfg.Skills); err != nil {
		t.Fatalf("expected case-insensitive lookup to succeed, got: %v", err)
	}
}

// TestToolReadSkillCompanionFileReadable confirms the optional "file"
// argument reads a companion resource relative to that specific skill's
// own folder.
func TestToolReadSkillCompanionFileReadable(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "pptx")
	if err := os.MkdirAll(filepath.Join(skillDir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pptx\ndescription: d.\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "assets", "notes.md"), []byte("companion notes"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	result, err := toolReadSkill(map[string]interface{}{"skill": "pptx", "file": "assets/notes.md"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "companion notes" {
		t.Fatalf("expected companion file content, got: %q", result)
	}
}

// TestToolReadSkillCompanionFileSandboxed is the security-critical
// regression test: the optional "file" argument must not be usable to
// escape that one skill's own directory via ".." or an absolute path,
// mirroring sandboxedPath's existing guarantee for read_file but rooted at
// the skill's folder instead of the current working directory.
func TestToolReadSkillCompanionFileSandboxed(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: d.\n---\n")
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("should not be readable via read_skill"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})

	if _, err := toolReadSkill(map[string]interface{}{"skill": "pptx", "file": "../secret.txt"}, cfg.Skills); err == nil {
		t.Fatal("expected a relative path-traversal escape (..) to be rejected")
	}
	if _, err := toolReadSkill(map[string]interface{}{"skill": "pptx", "file": secret}, cfg.Skills); err == nil {
		t.Fatal("expected an absolute path escaping the skill folder to be rejected")
	}
}

// TestToolReadSkillUnknownNameListsAvailable confirms an unrecognized
// skill name fails with a helpful error that lists what IS available,
// instead of a bare "not found" the model can't act on.
func TestToolReadSkillUnknownNameListsAvailable(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: d.\n---\n")
	cfg := loadSkills([]string{root})

	_, err := toolReadSkill(map[string]interface{}{"skill": "does-not-exist"}, cfg.Skills)
	if err == nil {
		t.Fatal("expected an error for an unknown skill name")
	}
	if !strings.Contains(err.Error(), "pptx") {
		t.Fatalf("expected the error to list available skill names, got: %v", err)
	}
}

// TestToolReadSkillRequiresSkillArg confirms a missing "skill" argument is
// rejected up front with a clear error rather than panicking on a nil
// lookup.
func TestToolReadSkillRequiresSkillArg(t *testing.T) {
	if _, err := toolReadSkill(map[string]interface{}{}, nil); err == nil {
		t.Fatal("expected an error when \"skill\" is not provided")
	}
}

// ─────────────────────────────────────────────────────────────────
// dispatchToolCall integration (same pattern as
// TestDispatchToolCallGetCurrentTime in time_test.go): confirms read_skill
// routes correctly through the real "extra" tool dispatch mechanism ask/
// coding both use, not just the underlying function in isolation.
// ─────────────────────────────────────────────────────────────────

func TestDispatchToolCallReadSkillViaExtra(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: Slides.\n---\nFull body text.\n")
	skillsCfg := loadSkills([]string{root})

	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	extra := func(name string, args map[string]interface{}) (string, error, bool) {
		if name != "read_skill" {
			return "", nil, false
		}
		if !skillsCfg.enabled() {
			return "", nil, false
		}
		r, e := toolReadSkill(args, skillsCfg.Skills)
		return r, e, true
	}

	argsJSON, _ := json.Marshal(map[string]interface{}{"skill": "pptx"})
	tc := toolCall{Function: toolCallFunction{Name: "read_skill", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, extra)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected read_skill to succeed via dispatchToolCall, got: %s", result)
	}
	if !strings.Contains(result, "Full body text.") {
		t.Fatalf("expected the full SKILL.md body in the dispatched result, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "[tool_load_time] read_skill") {
		t.Fatalf("expected a [tool_load_time] entry for read_skill (it's a local-file load), got:\n%s", logged)
	}
}

// TestDispatchToolCallReadSkillUnavailableWhenDisabled confirms that if
// read_skill is somehow called without skills actually being configured
// (e.g. an "extra" wired the same way ask/coding do, but skillsCfg is
// empty), it falls through to "unknown tool" instead of a confusing
// success/failure from an empty skill list - matching how run_command/
// web_search behave when their own feature isn't enabled.
func TestDispatchToolCallReadSkillUnavailableWhenDisabled(t *testing.T) {
	var skillsCfg skillsConfig // zero value: disabled

	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	extra := func(name string, args map[string]interface{}) (string, error, bool) {
		if name != "read_skill" || !skillsCfg.enabled() {
			return "", nil, false
		}
		r, e := toolReadSkill(args, skillsCfg.Skills)
		return r, e, true
	}

	argsJSON, _ := json.Marshal(map[string]interface{}{"skill": "pptx"})
	tc := toolCall{Function: toolCallFunction{Name: "read_skill", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, extra)
	if !strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected an ERROR result when skills aren't configured, got: %s", result)
	}
}

// ─────────────────────────────────────────────────────────────────
// End-to-end: cmdAsk wired with --skills-dir, driven through a scripted
// mock model (same shape as TestCmdAskAutoVerifyLoop in
// ask_integration_test.go), confirming the full path from flag parsing
// through system-prompt injection, tool advertisement, and dispatch.
// ─────────────────────────────────────────────────────────────────

func TestCmdAskReadSkillEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	skillsDir := t.TempDir()
	mustMkdirSkill(t, skillsDir, "thai-writing",
		"---\nname: thai-writing\ndescription: แนวทางการเขียนบทความภาษาไทยแบบกระชับ\n---\n"+
			"# Thai writing\nเขียนให้กระชับและเป็นธรรมชาติ ไม่ใช้คำฟุ่มเฟือย\n")

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			// The AVAILABLE SKILLS section must have actually reached the
			// model in the system prompt for it to know to call read_skill.
			if !strings.Contains(string(body), "thai-writing") || !strings.Contains(string(body), "AVAILABLE SKILLS") {
				t.Errorf("expected the request payload to include the AVAILABLE SKILLS section mentioning thai-writing, got: %s", body)
			}
			fmt.Fprint(w, streamLine("", "read_skill", `{"skill":"thai-writing"}`, true))
			return
		}
		fmt.Fprint(w, streamLine("เขียนตามแนวทาง skill แล้วครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-skill.log", "--skills-dir", skillsDir, "เขียนบทความสั้นๆ เกี่ยวกับกาแฟ"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (read_skill, final answer), got %d", got)
	}

	log, err := os.ReadFile("ask-skill.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "read_skill") {
		t.Fatalf("expected the tool_call log to record read_skill, got:\n%s", log)
	}
	if !strings.Contains(string(log), "เขียนให้กระชับและเป็นธรรมชาติ") {
		t.Fatalf("expected the read_skill tool result (full SKILL.md body) to be logged, got:\n%s", log)
	}
	if !strings.Contains(string(log), "# skills: enabled") {
		t.Fatalf("expected the log header to report skills enabled, got:\n%s", log)
	}
}

// TestCmdAskWithoutSkillsDirNeverOffersReadSkill confirms a completely
// ordinary session (no --skills-dir/OLA_SKILLS_DIR at all) never even
// advertises read_skill - skills must stay entirely invisible/inert unless
// explicitly configured, same principle as run_command/web_search.
func TestCmdAskWithoutSkillsDirNeverOffersReadSkill(t *testing.T) {
	workDir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("รับทราบครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")
	os.Unsetenv("OLA_SKILLS_DIR")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-noskill.log", "สวัสดีครับ"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if strings.Contains(gotBody, "read_skill") {
		t.Fatalf("expected read_skill to never appear in the request when no skills directory is configured, got: %s", gotBody)
	}
	if strings.Contains(gotBody, "AVAILABLE SKILLS") {
		t.Fatalf("expected no AVAILABLE SKILLS section in the system prompt when skills are disabled, got: %s", gotBody)
	}
}

// ======================================================================
// Section: integration_test.go
// ======================================================================

// ======================================================================
// coding_integration_test.go
// ======================================================================

// streamLine renders one NDJSON chunk matching ollamaStreamChunk's shape.
func streamLine(content, toolName, toolArgsJSON string, done bool) string {
	toolCallsField := ""
	if toolName != "" {
		toolCallsField = fmt.Sprintf(`,"tool_calls":[{"function":{"name":%q,"arguments":%s}}]`, toolName, toolArgsJSON)
	}
	doneStr := "false"
	if done {
		doneStr = "true"
	}
	return fmt.Sprintf(`{"message":{"role":"assistant","content":%q%s},"done":%s}`, content, toolCallsField, doneStr) + "\n"
}

// TestCmdCodingFullLoop drives cmdCoding against a real temp Go project
// through a scripted, multi-round mock model that:
//  1. registers a task list (add_tasks)
//  2. writes a main.go that DOES NOT COMPILE
//  3. claims completion (report_complete) - expecting ola's independent
//     verify step to reject it
//  4. on the next round, fixes the file (edit_file), marks the task done,
//     and calls report_complete again - expecting verify to now pass and
//     the session to end successfully.
//
// This exercises the whole new machinery end-to-end: tool dispatch for the
// four new coding tools, the independent build/test verification gate
// (never trusting report_complete on its own), state persistence to
// .ola-coding-state.json/PROGRESS.md, and clean loop termination.
func TestCmdCodingFullLoop(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module codingtest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("requirements.md", []byte("# ระบบทดสอบ\nสร้างโปรแกรม hello world ภาษา Go\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			// Round 1: plan the single task.
			fmt.Fprint(w, streamLine("", "add_tasks", `{"tasks":["Write hello world main.go"]}`, true))
		case 2:
			// Round 2: write BROKEN Go code (missing closing brace/paren).
			broken := `package main

import "fmt"

func main() {
	fmt.Println("hello"
}
`
			argsJSON := fmt.Sprintf(`{"path":"main.go","content":%q}`, broken)
			fmt.Fprint(w, streamLine("", "write_file", argsJSON, true))
		case 3:
			// Round 3: claim done before actually verifying - ola should
			// catch this since the file doesn't compile.
			fmt.Fprint(w, streamLine("", "report_complete", `{"summary":"hello world program written"}`, true))
		case 4:
			// Round 4: model sees the VERIFY FAILED tool result for the
			// previous report_complete and fixes the file for real.
			argsJSON := fmt.Sprintf(`{"path":"main.go","old_str":%q,"new_str":%q}`,
				`fmt.Println("hello"
}`, `fmt.Println("hello")
}`)
			fmt.Fprint(w, streamLine("", "edit_file", argsJSON, true))
		case 5:
			fmt.Fprint(w, streamLine("", "mark_task_done", `{"task_id":"T0","note":"fixed and compiles"}`, true))
		case 6:
			fmt.Fprint(w, streamLine("", "report_complete", `{"summary":"hello world program written and verified"}`, true))
		default:
			t.Errorf("unexpected extra round %d", n)
			fmt.Fprint(w, streamLine("unexpected extra round", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdCoding([]string{"-m", "mock-model", "-o", "coding-output.log"})
	if exitCode != 0 {
		t.Fatalf("expected cmdCoding to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 6 {
		t.Fatalf("expected exactly 6 mock rounds (plan, break, false-complete, fix, mark-done, real-complete), got %d", got)
	}

	// The fixed program should actually compile and run correctly now.
	out, err := exec.Command("go", "run", "main.go").CombinedOutput()
	if err != nil {
		t.Fatalf("expected the final main.go to build/run cleanly, got error: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("expected program output to contain 'hello', got: %s", out)
	}

	progress, err := os.ReadFile(codingProgressFile)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", codingProgressFile, err)
	}
	if !strings.Contains(string(progress), "1 / 1 tasks") {
		t.Fatalf("expected PROGRESS.md to show 1/1 tasks done, got:\n%s", progress)
	}

	state, existed := loadCodingState(codingStateFile)
	if !existed {
		t.Fatal("expected .ola-coding-state.json to exist after a completed run")
	}
	if len(state.Tasks) != 1 || !state.Tasks[0].Done {
		t.Fatalf("expected persisted state to show 1 completed task, got: %+v", state.Tasks)
	}
}

// ======================================================================
// ask_integration_test.go
// ======================================================================

// TestCmdAskAutoVerifyLoop drives cmdAsk against a real temp Go project
// through a scripted, multi-round mock model that:
//  1. writes a main.go that DOES NOT COMPILE via write_file
//  2. gives a plain final answer (no tool call) claiming the fix is done -
//     expecting ola's independent post-answer verify step to catch that it
//     doesn't actually build and feed the failure back instead of trusting
//     the model's word
//  3. on the next round, fixes the file via edit_file
//  4. gives another plain final answer - expecting verify to now pass and
//     the session to end successfully
//
// This exercises the new "ask" auto-verify gate end-to-end: conditional
// run_command tool exposure, filesChanged tracking, the independent
// build/test re-check after a plain final answer (never trusting the
// model's own claim that a change works), and clean loop termination once
// verification actually passes.
// TestCmdAskVerifyDisabledForNonCodeFileEdit is the regression test for the
// "vibe coding" bug: editing a plain-text/doc file inside a directory that
// happens to have a detected toolchain (here: a go.mod) must NOT trigger
// ola's auto-verify machinery. Before the isVerifiableEdit fix, filesChanged
// was set for ANY successful write_file/edit_file call regardless of what
// was edited, so a "fix a typo in notes.txt" session inside a Go repo would
// still try to "go build" it - or worse, misdetect a completely unrelated
// toolchain (e.g. Node) in a directory that also happened to have a
// package.json lying around, and try to run that instead.
func TestCmdAskVerifyDisabledForNonCodeFileEdit(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module doctest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("notes.txt", []byte("this is a note\nwith a typo: helllo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			argsJSON := fmt.Sprintf(`{"path":"notes.txt","old_str":%q,"new_str":%q}`, "helllo", "hello")
			fmt.Fprint(w, streamLine("", "edit_file", argsJSON, true))
			return
		}
		// If verify had (wrongly) kicked in, this would be round 2 seeing a
		// [verify]-fed-back tool message instead of a clean final answer.
		fmt.Fprint(w, streamLine("แก้ typo ให้แล้วครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-doc.log", "fix the typo in notes.txt"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (edit, final answer - no verify round), got %d", got)
	}

	fixed, err := os.ReadFile("notes.txt")
	if err != nil {
		t.Fatalf("expected notes.txt to exist: %v", err)
	}
	if !strings.Contains(string(fixed), "hello") {
		t.Fatalf("expected the typo fix to have been applied, got: %s", fixed)
	}

	log, err := os.ReadFile("ask-doc.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if strings.Contains(string(log), "[verify]") {
		t.Fatalf("expected NO verify/build attempt for a plain .txt doc edit in a Go repo, got:\n%s", log)
	}
	if !strings.Contains(string(log), "[verify-skip]") {
		t.Fatalf("expected a '[verify-skip] ...' note explaining why verify was skipped, got:\n%s", log)
	}
}

// TestCmdAskPromptFile confirms -f/--prompt-file reads the prompt text from
// a file instead of requiring it as a positional argument, and that in this
// mode every remaining positional argument is treated as an attachment.
func TestCmdAskPromptFile(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("prompt.txt", []byte("สรุปไฟล์ที่แนบมาให้หน่อย\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("attached.txt", []byte("เนื้อหาไฟล์แนบ\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("รับทราบครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-pf.log", "-f", "prompt.txt", "attached.txt"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if !strings.Contains(gotBody, "สรุปไฟล์ที่แนบมาให้หน่อย") {
		t.Fatalf("expected the prompt file's content to be used as the prompt, got request body: %s", gotBody)
	}
	if !strings.Contains(gotBody, "เนื้อหาไฟล์แนบ") {
		t.Fatalf("expected attached.txt's content to be attached since -f leaves all positionals as attachments, got: %s", gotBody)
	}
}

func TestCmdAskAutoVerifyLoop(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module asktest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			// Round 1: write BROKEN Go code (missing closing paren/brace).
			broken := `package main

import "fmt"

func main() {
	fmt.Println("hello"
}
`
			argsJSON := fmt.Sprintf(`{"path":"main.go","content":%q}`, broken)
			fmt.Fprint(w, streamLine("", "write_file", argsJSON, true))
		case 2:
			// Round 2: plain final answer claiming success, before ola's
			// own independent verify has actually run - this is the "vibe
			// coding" failure mode: the model asserting a fix works
			// without having checked. No tool call at all this round.
			fmt.Fprint(w, streamLine("แก้ไขให้แล้วครับ โปรแกรมทำงานถูกต้อง", "", "", true))
		case 3:
			// Round 3: model sees the VERIFY FAILED tool result fed back
			// after its premature claim, and actually fixes the file.
			argsJSON := fmt.Sprintf(`{"path":"main.go","old_str":%q,"new_str":%q}`,
				`fmt.Println("hello"
}`, `fmt.Println("hello")
}`)
			fmt.Fprint(w, streamLine("", "edit_file", argsJSON, true))
		case 4:
			// Round 4: final answer again, this time verify should pass.
			fmt.Fprint(w, streamLine("แก้ไขและตรวจสอบแล้ว โปรแกรมคอมไพล์ผ่าน", "", "", true))
		default:
			t.Errorf("unexpected extra round %d", n)
			fmt.Fprint(w, streamLine("unexpected extra round", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-output.log", "fix the compile error in main.go"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 4 {
		t.Fatalf("expected exactly 4 mock rounds (break, false-claim, fix, real-final-answer), got %d", got)
	}

	// The fixed program should actually compile and run correctly now.
	out, err := exec.Command("go", "run", "main.go").CombinedOutput()
	if err != nil {
		t.Fatalf("expected the final main.go to build/run cleanly, got error: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("expected program output to contain 'hello', got: %s", out)
	}

	log, err := os.ReadFile("ask-output.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "exit_code=1") {
		t.Fatalf("expected output log to show the first, failing verify attempt (exit_code=1), got:\n%s", log)
	}
	if strings.Count(string(log), "[verify]") < 2 {
		t.Fatalf("expected at least 2 verify attempts logged (1 failed + 1 passed), got:\n%s", log)
	}
}

// TestCmdAskVerifyGivesUpAfterMaxRounds makes sure a persistently broken
// build doesn't turn "ask" into an unbounded loop: the model here never
// actually fixes anything (it keeps re-asserting success), so verify must
// keep failing, and ola must stop after maxAskVerifyRounds attempts rather
// than retrying forever.
func TestCmdAskVerifyGivesUpAfterMaxRounds(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module askgiveup\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			broken := "package main\n\nfunc main() {\n\tprintln(\"never fixed\"\n}\n"
			argsJSON := fmt.Sprintf(`{"path":"main.go","content":%q}`, broken)
			fmt.Fprint(w, streamLine("", "write_file", argsJSON, true))
			return
		}
		// Every subsequent round: just keep claiming it's done, never
		// actually fixing the syntax error.
		fmt.Fprint(w, streamLine("เสร็จแล้วครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-giveup.log", "fix main.go"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to still exit 0 (HTTP-level success even though verify never passed), got %d", exitCode)
	}
	// 1 write round + (maxAskVerifyRounds + 1) final-answer rounds: the
	// last final-answer round is the one where verifyRounds has already
	// reached the cap, so it warns and stops instead of verifying again.
	wantRounds := int32(1 + maxAskVerifyRounds + 1)
	if got := atomic.LoadInt32(&round); got != wantRounds {
		t.Fatalf("expected exactly %d rounds before giving up, got %d", wantRounds, got)
	}

	log, err := os.ReadFile("ask-giveup.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if got := strings.Count(string(log), "[verify]"); got != maxAskVerifyRounds {
		t.Fatalf("expected exactly %d verify attempts logged, got %d in:\n%s", maxAskVerifyRounds, got, log)
	}
	if !strings.Contains(string(log), "[warning]") {
		t.Fatalf("expected a [warning] entry noting verify gave up, got:\n%s", log)
	}
}

// TestCmdAskVerifyDisabledForPlainQuestions makes sure a session that never
// touches a file (a plain Q&A prompt) never triggers verification at all,
// even when run inside a directory with a detected Go toolchain - the
// auto-verify gate must be conditioned on filesChanged, not merely on cwd
// having a go.mod.
func TestCmdAskVerifyDisabledForPlainQuestions(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module asktest2\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("Go is a statically typed, compiled language.", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-output2.log", "what is Go?"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 1 {
		t.Fatalf("expected exactly 1 round for a plain question with no file edits, got %d", got)
	}

	log, err := os.ReadFile("ask-output2.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if strings.Contains(string(log), "[verify]") {
		t.Fatalf("expected no verify attempts for a session that never edited a file, got:\n%s", log)
	}
}

// ======================================================================
// scp_integration_test.go
// ======================================================================

// TestCmdAskSCPCopyEndToEnd drives cmdAsk (the same real entry point
// exercised in ask_integration_test.go) with --scp-hosts configured and a
// fake `scp` binary on PATH (see installFakeSCP/fakeSCPScript in
// scp_test.go), confirming:
//   - scp_copy is only advertised to the model once --scp-hosts is set
//     (searched for in the outgoing request body, same style as
//     TestCmdAskSystemPromptReachesModelWithFreshnessGuidanceWhenSearchEnabled
//     in freshness_test.go)
//   - a model-issued scp_copy tool call actually runs end-to-end through
//     dispatchToolCall's "extra" mechanism and moves real bytes
//   - the -o log records the call/result plus the scp_copy status line
//   - no ask_user confirmation round is inserted - the call completes in
//     the very next round, per the "no confirmation prompt" design
//     decision documented in scp.go
func TestCmdAskSCPCopyEndToEnd(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	remoteDir := t.TempDir()
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	if err := os.WriteFile("report.txt", []byte("สรุปผลประจำวัน\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			argsJSON := `{"direction":"upload","remote_alias":"backup","local_path":"report.txt","remote_path":"reports/report.txt","reason":"สำรองรายงานประจำวัน"}`
			fmt.Fprint(w, streamLine("", "scp_copy", argsJSON, true))
			return
		}
		fmt.Fprint(w, streamLine("สำรองไฟล์เรียบร้อยแล้วครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{
		"-m", "mock-model", "-o", "ask-scp.log",
		"--scp-hosts", "backup=moo@testhost/",
		"สำรอง report.txt ไปที่ backup หน่อย",
	})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (scp_copy call, then final answer - no ask_user confirmation round), got %d", got)
	}
	if !strings.Contains(gotBody, `"scp_copy"`) {
		t.Fatalf("expected scp_copy to be advertised as a tool once --scp-hosts is set, got request body: %s", gotBody)
	}

	uploaded, err := os.ReadFile(remoteDir + "/reports/report.txt")
	if err != nil {
		t.Fatalf("expected the file to actually land on the (fake) remote side: %v", err)
	}
	if !strings.Contains(string(uploaded), "สรุปผลประจำวัน") {
		t.Fatalf("expected uploaded content to match the source file, got: %q", uploaded)
	}

	log, err := os.ReadFile("ask-scp.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "# scp_copy: enabled") {
		t.Fatalf("expected the log header to report scp_copy as enabled, got:\n%s", log)
	}
	if !strings.Contains(string(log), "[tool_call] scp_copy") {
		t.Fatalf("expected a scp_copy tool_call entry in the log, got:\n%s", log)
	}
	if strings.Contains(string(log), "[ASK]") {
		t.Fatalf("expected NO ask_user confirmation before the scp_copy call, got:\n%s", log)
	}
}

// TestCmdAskSCPCopyNotOfferedWithoutConfig confirms a plain session (no
// --scp-hosts/OLA_SCP_HOSTS) never even sees scp_copy in its tool list -
// the same "only offer what's actually configured" principle web_search/
// skills/run_command already follow.
func TestCmdAskSCPCopyNotOfferedWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("รับทราบครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-noscp.log", "สวัสดี"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if strings.Contains(gotBody, `"scp_copy"`) {
		t.Fatalf("expected scp_copy NOT to be offered without --scp-hosts/OLA_SCP_HOSTS configured, got: %s", gotBody)
	}
}

// TestCmdAskAPIRequestEndToEnd drives cmdAsk against a mocked Ollama
// /api/chat endpoint whose first round calls api_request against a second,
// separate httptest.Server standing in for the "real" external API (the
// endpoint alias points at this second server, not at Ollama itself) -
// mirroring TestCmdAskSCPCopyEndToEnd's two-server shape (one mock model,
// one mock "remote side").
func TestCmdAskAPIRequestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var gotPath, gotQuery string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":["qwen3.6:27b"]}`)
	}))
	defer apiSrv.Close()

	var round int32
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			argsJSON := `{"endpoint":"ollama","path":"/api/tags","query":{"q":"list"}}`
			fmt.Fprint(w, streamLine("", "api_request", argsJSON, true))
			return
		}
		fmt.Fprint(w, streamLine("มีโมเดล qwen3.6:27b ครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{
		"-m", "mock-model", "-o", "ask-api.log",
		"--api-endpoints", "ollama=" + apiSrv.URL,
		"เช็คว่ามีโมเดลอะไรบ้างใน ollama ตอนนี้",
	})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (api_request call, then final answer), got %d", got)
	}
	if !strings.Contains(gotBody, `"api_request"`) {
		t.Fatalf("expected api_request to be advertised as a tool once --api-endpoints is set, got request body: %s", gotBody)
	}
	if gotPath != "/api/tags" {
		t.Fatalf("expected the external API to actually receive /api/tags, got %q", gotPath)
	}
	if gotQuery != "list" {
		t.Fatalf("expected the query param to reach the external API, got %q", gotQuery)
	}

	log, err := os.ReadFile("ask-api.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "# api_request: enabled") {
		t.Fatalf("expected the log header to report api_request as enabled, got:\n%s", log)
	}
	if !strings.Contains(string(log), "[tool_call] api_request") {
		t.Fatalf("expected an api_request tool_call entry in the log, got:\n%s", log)
	}
	if !strings.Contains(string(log), `"models":["qwen3.6:27b"]`) {
		t.Fatalf("expected the external API's actual response body in the tool_result, got:\n%s", log)
	}
}

// TestCmdAskAPIRequestNotOfferedWithoutConfig mirrors
// TestCmdAskSCPCopyNotOfferedWithoutConfig: a plain session (no
// --api-endpoints/OLA_API_ENDPOINTS, no --api-allow-direct-url) never even
// sees api_request in its tool list.
func TestCmdAskAPIRequestNotOfferedWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("รับทราบครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-noapi.log", "สวัสดี"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if strings.Contains(gotBody, `"api_request"`) {
		t.Fatalf("expected api_request NOT to be offered without --api-endpoints/OLA_API_ENDPOINTS or --api-allow-direct-url configured, got: %s", gotBody)
	}
}

// ======================================================================
// freshness_test.go
// ======================================================================

// ─────────────────────────────────────────────────────────────────
// Proactive time/freshness tool use: the system prompt (both "ask" and
// "coding") must explicitly tell the model to call get_current_time and/or
// web_search on its own whenever a request depends on "now" or on
// information that may be stale in its training data - WITHOUT the human
// having to spell that out in the prompt every single time (e.g. "เมื่อวาน
// วันอะไร", "หาข่าวเกี่ยวกับ AI ในรอบ 3 วันนี้แล้วสรุปให้หน่อย",
// "สถานการณ์ราคาทองคำเป็นอย่างไรบ้าง").
// ─────────────────────────────────────────────────────────────────

// TestAskSystemPromptHasProactiveTimeFreshnessGuidance confirms the "ask"
// system prompt contains a dedicated section spelling out relative-time and
// freshness triggers, names both tools involved, and gives at least one of
// the concrete Thai example phrasings from the motivating request.
func TestAskSystemPromptHasProactiveTimeFreshnessGuidance(t *testing.T) {
	p := builtinSystemPrompt
	for _, want := range []string{
		"get_current_time", "web_search",
		"เมื่อวาน",           // relative-time trigger example ("yesterday")
		"สถานการณ์ราคาทองคำ", // freshness trigger example (gold price situation)
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("expected the ask system prompt to mention %q, but it did not", want)
		}
	}
	// The guidance must be explicit that this happens WITHOUT the user
	// asking for it - that's the actual point of the feature. Collapse
	// whitespace first since the source wraps prose across lines, and a
	// multi-word phrase check would otherwise be defeated by a literal "\n"
	// landing in the middle of it.
	flat := strings.Join(strings.Fields(p), " ")
	if !strings.Contains(flat, "even when the user never") {
		t.Fatalf("expected the prompt to state tool use should happen even when not explicitly requested")
	}
}

// TestCodingSystemPromptHasProactiveTimeFreshnessGuidance mirrors the above
// for "coding"'s own (separate) system prompt constant.
func TestCodingSystemPromptHasProactiveTimeFreshnessGuidance(t *testing.T) {
	p := builtinCodingSystemPrompt
	for _, want := range []string{"get_current_time", "web_search", "PROACTIVE"} {
		if !strings.Contains(p, want) {
			t.Fatalf("expected the coding system prompt to mention %q, but it did not", want)
		}
	}
}

// TestGetCurrentTimeToolDescriptionMentionsRelativeTimePhrases confirms the
// reinforcement lives at the tool-schema level too, not just buried in the
// long system prompt - local models often attend more to a tool's own
// description than to a distant system-prompt section.
func TestGetCurrentTimeToolDescriptionMentionsRelativeTimePhrases(t *testing.T) {
	var desc string
	for _, tl := range builtinTools {
		if tl.Function.Name == "get_current_time" {
			desc = tl.Function.Description
		}
	}
	if desc == "" {
		t.Fatal("expected to find get_current_time in builtinTools")
	}
	if !strings.Contains(desc, "เมื่อวาน") {
		t.Fatalf("expected get_current_time's description to mention a relative-time example (เมื่อวาน), got: %s", desc)
	}
}

// TestWebSearchToolDescriptionMentionsProactiveUse confirms webSearchTool's
// own description instructs proactive/automatic use for freshness-sensitive
// queries, without waiting for the user to explicitly ask for a search.
func TestWebSearchToolDescriptionMentionsProactiveUse(t *testing.T) {
	desc := webSearchTool.Function.Description
	if !strings.Contains(desc, "ไม่ต้องรอให้ผู้ใช้") {
		t.Fatalf("expected web_search's description to say it should be used without waiting for the user to ask, got: %s", desc)
	}
	if !strings.Contains(desc, "get_current_time") {
		t.Fatalf("expected web_search's description to reference pairing with get_current_time for relative time windows, got: %s", desc)
	}
}

// TestCmdAskSystemPromptReachesModelWithFreshnessGuidanceWhenSearchEnabled
// is an end-to-end check (same shape as TestCmdAskReadSkillEndToEnd in
// skills_test.go): confirms that in a real cmdAsk run with web_search
// enabled (--searxng-url pointed at a mock server), the actual HTTP request
// sent to the model includes both the PROACTIVE TIME/FRESHNESS section and
// the web_search tool - i.e. the guidance genuinely reaches the wire, not
// just present in the Go source constant.
func TestCmdAskSystemPromptReachesModelWithFreshnessGuidanceWhenSearchEnabled(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	// Mock SearXNG - never actually expected to be hit in this test, since
	// the mock model below answers immediately without calling any tool;
	// we're only checking what the model WAS OFFERED and told, not
	// exercising an actual search round trip.
	searxng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer searxng.Close()

	var round int32
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&round, 1)
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("สวัสดีครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-freshness.log", "--searxng-url", searxng.URL, "สวัสดีครับ"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if atomic.LoadInt32(&round) != 1 {
		t.Fatalf("expected exactly 1 round, got %d", round)
	}

	if !strings.Contains(gotBody, "PROACTIVE TIME/FRESHNESS") {
		t.Fatalf("expected the request payload's system prompt to include the PROACTIVE TIME/FRESHNESS section, got: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"web_search"`) {
		t.Fatalf("expected web_search to be offered as a tool once --searxng-url is set, got: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"get_current_time"`) {
		t.Fatalf("expected get_current_time to always be offered as a tool, got: %s", gotBody)
	}
}

// ======================================================================
// Section: api_request_test.go
// ======================================================================

// testAPIRequestConfig builds an apiRequestConfig with a single "test"
// endpoint pointed at srv, mirroring testSCPConfig's shape in unit_test.go.
func testAPIRequestConfig(srv *httptest.Server) apiRequestConfig {
	return apiRequestConfig{
		Endpoints:        map[string]apiEndpoint{"test": {Alias: "test", BaseURL: srv.URL}},
		EndpointOrder:    []string{"test"},
		Timeout:          defaultAPIRequestTimeoutSec * 1e9,
		MaxRequestBytes:  defaultAPIRequestMaxBodyBytes,
		MaxResponseBytes: defaultAPIRequestMaxResponseBytes,
	}
}

func TestToolAPIRequestDisabledWithEmptyConfig(t *testing.T) {
	if _, err := toolAPIRequest(map[string]interface{}{"endpoint": "test", "path": "/x"}, apiRequestConfig{}); err == nil {
		t.Fatal("expected an error when api_request is not configured")
	}
}

func TestToolAPIRequestRejectsUnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	_, err := toolAPIRequest(map[string]interface{}{"endpoint": "not-configured", "path": "/x"}, cfg)
	if err == nil {
		t.Fatal("expected an unknown endpoint alias to be rejected")
	}
	if !strings.Contains(err.Error(), "test") {
		t.Fatalf("expected the error to list the allowed endpoint(s), got: %v", err)
	}
}

func TestToolAPIRequestRejectsBothEndpointAndURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowDirectURL = true

	args := map[string]interface{}{"endpoint": "test", "path": "/x", "url": "https://example.com"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected specifying both endpoint and url to be rejected")
	}
}

func TestToolAPIRequestRequiresEndpointOrURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	if _, err := toolAPIRequest(map[string]interface{}{}, cfg); err == nil {
		t.Fatal("expected omitting both endpoint and url to be rejected")
	}
}

func TestToolAPIRequestEndpointModeGETWithQuery(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	args := map[string]interface{}{
		"endpoint": "test", "path": "/api/tags",
		"query": map[string]interface{}{"q": "hello"},
	}
	result, err := toolAPIRequest(args, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/tags" {
		t.Fatalf("expected path /api/tags, got %q", gotPath)
	}
	if gotQuery != "hello" {
		t.Fatalf("expected query q=hello, got %q", gotQuery)
	}
	if !strings.Contains(result, "HTTP 200") || !strings.Contains(result, `"ok":true`) {
		t.Fatalf("expected result to include status and body, got: %s", result)
	}
}

func TestToolAPIRequestDefaultMethodIsGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	if _, err := toolAPIRequest(map[string]interface{}{"endpoint": "test", "path": "/"}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("expected default method GET, got %q", gotMethod)
	}
}

func TestToolAPIRequestRejectsMutatingMethodByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be hit - method should be rejected before any request is sent")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv) // AllowMutating defaults to false

	args := map[string]interface{}{"endpoint": "test", "path": "/x", "method": "POST"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected POST to be rejected when AllowMutating is false")
	}
}

func TestToolAPIRequestRejectsUnsupportedMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be hit")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{"endpoint": "test", "path": "/x", "method": "TRACE"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected an unsupported method (TRACE) to be rejected even with AllowMutating on")
	}
}

func TestToolAPIRequestAllowsMutatingWhenEnabled(t *testing.T) {
	var gotMethod, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/create", "method": "POST",
		"body_type": "json", "body": map[string]interface{}{"name": "moo"},
	}
	result, err := toolAPIRequest(args, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %q", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected application/json content-type, got %q", gotContentType)
	}
	if !strings.Contains(gotBody, `"name":"moo"`) {
		t.Fatalf("expected JSON body to contain name=moo, got: %s", gotBody)
	}
	if !strings.Contains(result, "HTTP 201") {
		t.Fatalf("expected result to report HTTP 201, got: %s", result)
	}
}

func TestToolAPIRequestNon2xxIsNotAGoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad input"}`))
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	result, err := toolAPIRequest(map[string]interface{}{"endpoint": "test", "path": "/x"}, cfg)
	if err != nil {
		t.Fatalf("expected a 4xx response to NOT be a Go error, got: %v", err)
	}
	if !strings.Contains(result, "HTTP 400") || !strings.Contains(result, "bad input") {
		t.Fatalf("expected result to surface the 400 status and body, got: %s", result)
	}
}

func TestToolAPIRequestFormBody(t *testing.T) {
	var gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/x", "method": "POST",
		"body_type": "form", "body": map[string]interface{}{"a": "1", "b": "two"},
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("expected form content-type, got %q", gotContentType)
	}
	if !strings.Contains(gotBody, "a=1") || !strings.Contains(gotBody, "b=two") {
		t.Fatalf("expected form-encoded body, got: %s", gotBody)
	}
}

func TestToolAPIRequestTextBody(t *testing.T) {
	var gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/x", "method": "PUT",
		"body_type": "text", "body": "hello plain text",
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(gotContentType, "text/plain") {
		t.Fatalf("expected text/plain content-type, got %q", gotContentType)
	}
	if gotBody != "hello plain text" {
		t.Fatalf("expected raw text body, got: %s", gotBody)
	}
}

func TestToolAPIRequestBinaryBody(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	raw := []byte{0x00, 0x01, 0xFF, 0x10}
	args := map[string]interface{}{
		"endpoint": "test", "path": "/x", "method": "POST",
		"body_type": "binary", "body": base64.StdEncoding.EncodeToString(raw),
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/octet-stream" {
		t.Fatalf("expected application/octet-stream content-type, got %q", gotContentType)
	}
	if string(gotBody) != string(raw) {
		t.Fatalf("expected raw bytes to round-trip, got: %v want: %v", gotBody, raw)
	}
}

func TestToolAPIRequestMultipartBody(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile(filepath.Join(dir, "attach.txt"), []byte("file contents"), 0644); err != nil {
		t.Fatal(err)
	}

	var gotContentType string
	var gotFieldValue, gotFileContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("server failed to parse multipart form: %v", err)
			return
		}
		gotFieldValue = r.FormValue("note")
		f, _, err := r.FormFile("file0")
		if err != nil {
			t.Errorf("server failed to read file0: %v", err)
			return
		}
		defer f.Close()
		buf := make([]byte, 1024)
		n, _ := f.Read(buf)
		gotFileContent = string(buf[:n])
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/upload", "method": "POST",
		"body_type":       "multipart",
		"body":            map[string]interface{}{"note": "hi"},
		"multipart_files": []interface{}{"attach.txt"},
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("expected multipart/form-data content-type, got %q", gotContentType)
	}
	if gotFieldValue != "hi" {
		t.Fatalf("expected field note=hi, got %q", gotFieldValue)
	}
	if gotFileContent != "file contents" {
		t.Fatalf("expected attached file contents to round-trip, got %q", gotFileContent)
	}
}

func TestToolAPIRequestMultipartRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be hit - path escape should be rejected before sending")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/upload", "method": "POST",
		"body_type":       "multipart",
		"multipart_files": []interface{}{"../../etc/passwd"},
	}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected a multipart_files path escaping the sandbox to be rejected")
	}
}

func TestToolAPIRequestReservedHeadersAreFilteredAndEndpointAuthWins(t *testing.T) {
	var gotAuth, gotHost, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Host
		gotCustom = r.Header.Get("X-Custom")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	ep := cfg.Endpoints["test"]
	ep.AuthHeader = "Authorization"
	ep.AuthValue = "Bearer secret-token"
	cfg.Endpoints["test"] = ep

	args := map[string]interface{}{
		"endpoint": "test", "path": "/x",
		"headers": map[string]interface{}{
			"Authorization": "Bearer model-supplied-should-be-ignored",
			"Host":          "evil.example",
			"X-Custom":      "hello",
		},
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected endpoint's own auth to win, got Authorization=%q", gotAuth)
	}
	if strings.Contains(gotHost, "evil.example") {
		t.Fatalf("expected model-supplied Host header to be ignored, got Host=%q", gotHost)
	}
	if gotCustom != "hello" {
		t.Fatalf("expected non-reserved custom header to pass through, got %q", gotCustom)
	}
}

func TestToolAPIRequestDirectURLDisabledByDefault(t *testing.T) {
	cfg := apiRequestConfig{
		Endpoints: map[string]apiEndpoint{"test": {Alias: "test", BaseURL: "http://example.invalid"}},
	}
	args := map[string]interface{}{"url": "https://example.com"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected direct-URL mode to be rejected when AllowDirectURL is false")
	}
}

func TestToolAPIRequestDirectURLRejectsPrivateAndLocalURLs(t *testing.T) {
	cfg := apiRequestConfig{AllowDirectURL: true, Timeout: defaultAPIRequestTimeoutSec * 1e9, MaxResponseBytes: defaultAPIRequestMaxResponseBytes}
	cases := []string{
		"http://localhost:11434/api/tags",
		"http://127.0.0.1:8080/admin",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
	}
	for _, u := range cases {
		if _, err := toolAPIRequest(map[string]interface{}{"url": u}, cfg); err == nil {
			t.Fatalf("expected %q to be rejected by the SSRF guard", u)
		}
	}
}

// redirectAllTransportAPI mirrors redirectAllTransport in unit_test.go
// (web_fetch's own parallel-fetch test) - a test-only RoundTripper that
// dials target no matter what host/port the request was addressed to, so
// a direct-mode call to a fake public hostname can land on a local
// httptest.Server without tripping the SSRF guard (which only rejects
// obviously-local/private hosts, not made-up public-looking ones).
type redirectAllTransportAPI struct {
	target string
	base   http.Transport
}

func (rt *redirectAllTransportAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = rt.target
	return rt.base.RoundTrip(req)
}

func TestToolAPIRequestDirectURLAllowsPublicHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &redirectAllTransportAPI{target: srv.Listener.Addr().String()}
	defer func() { http.DefaultTransport = origTransport }()

	cfg := apiRequestConfig{AllowDirectURL: true, Timeout: defaultAPIRequestTimeoutSec * 1e9, MaxResponseBytes: defaultAPIRequestMaxResponseBytes}
	result, err := toolAPIRequest(map[string]interface{}{"url": "http://public.example.invalid/hello"}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "ok") {
		t.Fatalf("expected response body to come through, got: %s", result)
	}
}

func TestJoinEndpointPathIgnoresHostInPath(t *testing.T) {
	// A path that looks like a full URL (with its own scheme/host) must
	// never redirect the request away from the endpoint's own base host -
	// only its Path/RawQuery should ever be used.
	full, err := joinEndpointPath("https://trusted.example/api", "http://evil.example/steal?x=1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, err := url.Parse(full)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "trusted.example" {
		t.Fatalf("expected host to stay trusted.example, got %q (full: %s)", u.Host, full)
	}
	if u.Path != "/api/steal" {
		t.Fatalf("expected joined path /api/steal, got %q (full: %s)", u.Path, full)
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"/api", "/x", "/api/x"},
		{"/api/", "/x", "/api/x"},
		{"/api", "x", "/api/x"},
		{"/api/", "x", "/api/x"},
		{"/api", "", "/api"},
	}
	for _, c := range cases {
		if got := singleJoiningSlash(c.a, c.b); got != c.want {
			t.Errorf("singleJoiningSlash(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestFormatAPIResponseTruncatesLongTextBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/plain"}}, StatusCode: 200}
	long := strings.Repeat("x", maxAPIResultOutput+500)
	out := formatAPIResponse(resp, []byte(long))
	if len(out) >= len(long) {
		t.Fatalf("expected long text body to be truncated, output was %d bytes (input %d bytes)", len(out), len(long))
	}
}

func TestFormatAPIResponseHidesBinaryBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"image/png"}}, StatusCode: 200}
	binary := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x01, 0x02}
	out := formatAPIResponse(resp, binary)
	if strings.Contains(out, string(binary)) {
		t.Fatal("expected binary body to never be echoed back verbatim")
	}
	if !strings.Contains(out, "binary") {
		t.Fatalf("expected a note that the body is binary and hidden, got: %s", out)
	}
}

func TestResolveAPIRequestConfigParsesEndpointsAndAuth(t *testing.T) {
	os.Setenv("OLA_API_ENDPOINT_OLLAMA_AUTH_HEADER", "Authorization")
	os.Setenv("OLA_API_ENDPOINT_OLLAMA_AUTH_VALUE", "Bearer xyz")
	defer os.Unsetenv("OLA_API_ENDPOINT_OLLAMA_AUTH_HEADER")
	defer os.Unsetenv("OLA_API_ENDPOINT_OLLAMA_AUTH_VALUE")

	cfg, warnings := resolveAPIRequestConfig("ollama=http://localhost:11434,bad-entry,openwebui=http://localhost:8080", false, false, 0, 0, 0)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the malformed entry, got %d: %v", len(warnings), warnings)
	}
	if !cfg.enabled() {
		t.Fatal("expected config with endpoints to be enabled")
	}
	ollama, ok := cfg.Endpoints["ollama"]
	if !ok {
		t.Fatal("expected 'ollama' endpoint to be parsed")
	}
	if ollama.BaseURL != "http://localhost:11434" {
		t.Fatalf("unexpected BaseURL: %q", ollama.BaseURL)
	}
	if ollama.AuthHeader != "Authorization" || ollama.AuthValue != "Bearer xyz" {
		t.Fatalf("expected auth header/value to be picked up from env, got %q/%q", ollama.AuthHeader, ollama.AuthValue)
	}
	if _, ok := cfg.Endpoints["openwebui"]; !ok {
		t.Fatal("expected 'openwebui' endpoint to be parsed despite the earlier bad entry")
	}
}

func TestResolveAPIRequestConfigDisabledWhenNothingSet(t *testing.T) {
	cfg, warnings := resolveAPIRequestConfig("", false, false, 0, 0, 0)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", warnings)
	}
	if cfg.enabled() {
		t.Fatal("expected config to be disabled when nothing was configured")
	}
}

func TestAPIRequestToolNotOfferedWhenDisabled_sanity(t *testing.T) {
	// Mirrors TestWebSearchToolNotOfferedWhenDisabled_sanity's shape: a
	// quick sanity check that the gating condition itself behaves as
	// expected, independent of the CLI wiring in main.go/cmdCoding.
	var cfg apiRequestConfig
	if cfg.enabled() {
		t.Fatal("expected zero-value apiRequestConfig to be disabled")
	}
}

// ======================================================================
// quiet_test.go (new: -q/--quiet and $OLA_QUIET)
// ======================================================================
// Drives the real cmdAsk entry point (same httptest pattern as the rest of
// this file) and captures os.Stdout to check what quiet mode actually
// trims vs. what it always leaves alone. Notification gating (WRITE/EDIT/
// MKDIR/TASK/scp_copy/api_request suppressed, ask_user + end-of-session
// summary always sent) isn't covered here since sendNotification posts to
// the real https://ntfy.sh unconditionally - there's no seam to point it
// at a mock server without changing sendNotification's own signature, and
// these tests would rather stay hermetic than depend on network access.
// Likewise, ask_user's own terminal banner isn't covered here: toolAskUser
// checks isStdinTerminal() before printing anything, and that's false for
// a go test process's stdin, so the banner never fires either way in this
// harness - that behavior is simply unmodified by any of the qprint*
// changes (see main.go's dispatchToolCall), which is verifiable by
// inspection rather than by a test that would need a real pty to exercise.

// captureStdout temporarily redirects os.Stdout to a pipe for the duration
// of fn, returning everything written to it. Used to inspect exactly what
// a cmdAsk/cmdCoding run printed to the terminal without relying on any
// particular buffering behavior of fmt.Print* itself.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf strings.Builder
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// mockThinkToolAnswerServer returns an /api/chat mock that streams a
// thinking chunk + a get_current_time tool call on round 1, then a plain
// final answer on round 2 - enough surface area to exercise every
// terminal-chrome code path quiet mode is supposed to touch (thinking
// banner/tokens, tool_call echo + result preview, load-timing line, the
// round/token stats footer) in a single small script.
func mockThinkToolAnswerServer(t *testing.T, finalAnswer string) *httptest.Server {
	t.Helper()
	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			fmt.Fprint(w, `{"message":{"role":"assistant","thinking":"hmm let me think","content":""},"done":false}`+"\n")
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_current_time","arguments":{}}}]},"done":true}`+"\n")
		} else {
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"done":true}`+"\n", finalAnswer)
		}
	})
	return httptest.NewServer(mux)
}

// TestQuietModeDefaultStillShowsChrome pins down the "quiet mode is opt-in"
// half of the contract: with neither -q nor $OLA_QUIET set, behavior must
// stay exactly as it was before this feature existed.
func TestQuietModeDefaultStillShowsChrome(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	srv := mockThinkToolAnswerServer(t, "THE-FINAL-ANSWER-TEXT")
	defer srv.Close()
	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	out := captureStdout(t, func() {
		cmdAsk([]string{"-m", "mock-model", "-o", "noisy.log", "สวัสดี"})
	})

	if !strings.Contains(out, "THE-FINAL-ANSWER-TEXT") {
		t.Fatalf("expected answer text in stdout, got: %q", out)
	}
	if !strings.Contains(out, "tool_call") {
		t.Errorf("expected tool_call echo in non-quiet stdout, got: %q", out)
	}
	if !strings.Contains(out, "Thinking") {
		t.Errorf("expected thinking banner in non-quiet stdout, got: %q", out)
	}
	if !strings.Contains(out, "⏱") {
		t.Errorf("expected round timing footer in non-quiet stdout, got: %q", out)
	}
}

// TestQuietModeFlagTrimsTerminalButNotLogFile is the core of the feature:
// -q must trim the terminal down to just the answer, while the -o log file
// stays exactly as complete as it's always been.
func TestQuietModeFlagTrimsTerminalButNotLogFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	srv := mockThinkToolAnswerServer(t, "THE-FINAL-ANSWER-TEXT")
	defer srv.Close()
	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	out := captureStdout(t, func() {
		cmdAsk([]string{"-m", "mock-model", "-q", "-o", "quiet.log", "สวัสดี"})
	})
	quietMode = false // package-global set by cmdAsk; reset for later tests in this binary

	if !strings.Contains(out, "THE-FINAL-ANSWER-TEXT") {
		t.Fatalf("expected answer text to still be in stdout under -q, got: %q", out)
	}
	for _, unwanted := range []string{"tool_call", "Thinking", "⏱", "📥", "🔧"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("did not expect %q in quiet stdout, got: %q", unwanted, out)
		}
	}

	logData, err := os.ReadFile("quiet.log")
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(logData)
	for _, wanted := range []string{"tool_call", "Thinking"} {
		if !strings.Contains(logStr, wanted) {
			t.Errorf("expected -o log file to still contain %q even in quiet mode, got: %q", wanted, logStr)
		}
	}
}

// TestQuietModeEnvVarMatchesFlag confirms $OLA_QUIET is a real substitute
// for -q, not just a documented-but-unwired env var.
func TestQuietModeEnvVarMatchesFlag(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	srv := mockThinkToolAnswerServer(t, "ENV-QUIET-ANSWER")
	defer srv.Close()
	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	os.Setenv("OLA_QUIET", "1")
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")
	defer os.Unsetenv("OLA_QUIET")

	out := captureStdout(t, func() {
		cmdAsk([]string{"-m", "mock-model", "-o", "envquiet.log", "สวัสดี"})
	})
	quietMode = false

	if !strings.Contains(out, "ENV-QUIET-ANSWER") {
		t.Fatalf("expected answer text in stdout, got: %q", out)
	}
	if strings.Contains(out, "⏱") {
		t.Errorf("expected OLA_QUIET=1 to trim timing lines same as -q, got: %q", out)
	}
}

// ======================================================================
// Section: OpenAI-compatible chat completions provider (openai_test.go)
// ======================================================================
// Tests for the "Section: OpenAI-compatible chat completions provider" in
// main.go: provider/host/apiKey/model resolution, ollamaMessage <->
// OpenAI wire-format conversion, SSE stream parsing (including
// incrementally-streamed tool_calls deltas, which Ollama's own format
// never has to deal with since it sends each tool call whole), and one
// end-to-end test driving cmdAsk against a mocked /chat/completions
// endpoint to confirm the whole tool-calling loop - including
// tool_call_id round-tripping - actually works over the wire, not just at
// the unit level.

// ---- resolveProvider ----

func TestResolveProviderDefaultsToOllamaWhenUnset(t *testing.T) {
	os.Unsetenv("OLA_PROVIDER")
	p, err := resolveProvider("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != providerOllama {
		t.Fatalf("expected default provider ollama, got %q", p)
	}
}

func TestResolveProviderEnvSelectsOpenAI(t *testing.T) {
	os.Setenv("OLA_PROVIDER", "openai")
	defer os.Unsetenv("OLA_PROVIDER")
	p, err := resolveProvider("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != providerOpenAI {
		t.Fatalf("expected OLA_PROVIDER=openai to select openai, got %q", p)
	}
}

func TestResolveProviderFlagWinsOverEnv(t *testing.T) {
	os.Setenv("OLA_PROVIDER", "openai")
	defer os.Unsetenv("OLA_PROVIDER")
	p, err := resolveProvider("ollama")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != providerOllama {
		t.Fatalf("expected -P/--provider flag to win over $OLA_PROVIDER, got %q", p)
	}
}

func TestResolveProviderRejectsUnknownValue(t *testing.T) {
	if _, err := resolveProvider("bogus"); err == nil {
		t.Fatal("expected an error for an unrecognized --provider value, got nil")
	}
}

// ---- resolveProviderConfig ----

func TestResolveProviderConfigOllamaDefaultsUnchanged(t *testing.T) {
	os.Unsetenv("OLA_PROVIDER")
	os.Unsetenv("OLA_OLLAMA_API_BASE")
	os.Unsetenv("OLA_OLLAMA_MODEL")

	cfg, err := resolveProviderConfig("", "", "mock-model", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != providerOllama {
		t.Errorf("expected provider ollama, got %q", cfg.Provider)
	}
	if cfg.Host != "http://localhost:11434" {
		t.Errorf("expected ola's original default Ollama host, got %q", cfg.Host)
	}
	if cfg.Model != "mock-model" {
		t.Errorf("expected the -m/--model flag value to pass through, got %q", cfg.Model)
	}
}

func TestResolveProviderConfigOpenAIDefaultsToOllamaCompatEndpoint(t *testing.T) {
	os.Unsetenv("OLA_OPENAI_API_BASE")
	os.Setenv("OLA_OPENAI_MODEL", "gpt-mock")
	defer os.Unsetenv("OLA_OPENAI_MODEL")

	cfg, err := resolveProviderConfig("openai", "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != defaultOpenAICompatBase {
		t.Errorf("expected default openai-compat host %q (Ollama's own /v1), got %q", defaultOpenAICompatBase, cfg.Host)
	}
	if cfg.Model != "gpt-mock" {
		t.Errorf("expected model from OLA_OPENAI_MODEL, got %q", cfg.Model)
	}
}

func TestResolveProviderConfigAPIBaseFlagOverridesEnv(t *testing.T) {
	os.Setenv("OLA_OPENAI_API_BASE", "http://env-should-lose:1234/v1")
	defer os.Unsetenv("OLA_OPENAI_API_BASE")

	cfg, err := resolveProviderConfig("openai", "http://flag-wins:9999/v1", "m", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "http://flag-wins:9999/v1" {
		t.Errorf("expected --api-base flag to win over $OLA_OPENAI_API_BASE, got %q", cfg.Host)
	}
}

func TestResolveProviderConfigOllamaAndOpenAIUseSeparateEnvNamespaces(t *testing.T) {
	os.Setenv("OLA_OLLAMA_API_BASE", "http://ollama-host:11434")
	os.Setenv("OLA_OPENAI_API_BASE", "http://openai-host:8080/v1")
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")
	defer os.Unsetenv("OLA_OPENAI_API_BASE")

	ollamaCfg, err := resolveProviderConfig("ollama", "", "m", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ollamaCfg.Host != "http://ollama-host:11434" {
		t.Errorf("expected ollama provider to read OLA_OLLAMA_API_BASE, got %q", ollamaCfg.Host)
	}

	openaiCfg, err := resolveProviderConfig("openai", "", "m", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if openaiCfg.Host != "http://openai-host:8080/v1" {
		t.Errorf("expected openai provider to read OLA_OPENAI_API_BASE, got %q", openaiCfg.Host)
	}
}

func TestResolveProviderConfigMissingAPIKeyReferencesCorrectEnvVar(t *testing.T) {
	os.Unsetenv("OLA_OPENAI_API_KEY")
	_, err := resolveProviderConfig("openai", "", "m", true)
	if err == nil || !strings.Contains(err.Error(), "OLA_OPENAI_API_KEY") {
		t.Fatalf("expected an error mentioning OLA_OPENAI_API_KEY, got: %v", err)
	}
}

func TestResolveProviderConfigMissingModelReferencesCorrectEnvVar(t *testing.T) {
	os.Unsetenv("OLA_OPENAI_MODEL")
	_, err := resolveProviderConfig("openai", "", "", false)
	if err == nil || !strings.Contains(err.Error(), "OLA_OPENAI_MODEL") {
		t.Fatalf("expected an error mentioning OLA_OPENAI_MODEL, got: %v", err)
	}
}

func TestResolveProviderConfigRejectsBadProvider(t *testing.T) {
	if _, err := resolveProviderConfig("bogus", "", "m", false); err == nil {
		t.Fatal("expected an error for an unrecognized provider, got nil")
	}
}

// ---- toOpenAIMessage / toOpenAIRequest ----

func TestToOpenAIMessagePlainText(t *testing.T) {
	om := toOpenAIMessage(ollamaMessage{Role: "user", Content: "hello"})
	if om.Role != "user" || om.Content != "hello" {
		t.Fatalf("unexpected message: %+v", om)
	}
	if len(om.ToolCalls) != 0 || om.ToolCallID != "" {
		t.Fatalf("expected no tool-call fields on a plain text message, got: %+v", om)
	}
}

func TestToOpenAIMessageToolCallArgumentsAreAByteForByteStringCopy(t *testing.T) {
	raw := json.RawMessage(`{"path":"a.txt","recursive":true}`)
	om := toOpenAIMessage(ollamaMessage{
		Role:      "assistant",
		ToolCalls: []toolCall{{ID: "call_abc", Function: toolCallFunction{Name: "read_file", Arguments: raw}}},
	})
	if len(om.ToolCalls) != 1 {
		t.Fatalf("expected exactly 1 tool call, got %d", len(om.ToolCalls))
	}
	tc := om.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Type != "function" || tc.Function.Name != "read_file" {
		t.Fatalf("unexpected tool call shape: %+v", tc)
	}
	if tc.Function.Arguments != string(raw) {
		t.Fatalf("expected arguments string to be a byte-for-byte copy of the raw JSON (%s), got %q", raw, tc.Function.Arguments)
	}
}

func TestToOpenAIMessageSynthesizesIDWhenSourceCallHasNone(t *testing.T) {
	om := toOpenAIMessage(ollamaMessage{
		Role:      "assistant",
		ToolCalls: []toolCall{{Function: toolCallFunction{Name: "get_current_time", Arguments: json.RawMessage(`{}`)}}},
	})
	if om.ToolCalls[0].ID == "" {
		t.Fatal("expected a synthesized (non-empty) tool_call id when the source toolCall had none")
	}
}

func TestToOpenAIMessageToolResultUsesToolCallID(t *testing.T) {
	om := toOpenAIMessage(ollamaMessage{Role: "tool", Name: "read_file", Content: "file contents", ToolCallID: "call_xyz"})
	if om.Role != "tool" || om.ToolCallID != "call_xyz" {
		t.Fatalf("expected role=tool, tool_call_id=call_xyz, got: %+v", om)
	}
	if om.Content != "file contents" {
		t.Fatalf("unexpected content: %v", om.Content)
	}
}

func TestToOpenAIMessageToolResultFallsBackToNameWhenIDMissing(t *testing.T) {
	// Exercises the fallback path used by the synthetic "verify" message
	// this codebase used to send as role:"tool" - see verifyFeedbackMessage,
	// which no longer routes through this fallback for the openai provider
	// (it sends role:"user" instead) but the fallback itself stays as a
	// defensive best-effort for any other caller that forgets ToolCallID.
	om := toOpenAIMessage(ollamaMessage{Role: "tool", Name: "verify", Content: "x"})
	if om.ToolCallID != "verify" {
		t.Fatalf("expected fallback to Name when ToolCallID is empty, got %q", om.ToolCallID)
	}
}

func TestToOpenAIMessageWithImageProducesSniffedDataURL(t *testing.T) {
	// The 8-byte PNG file signature, padded out - enough for
	// http.DetectContentType to positively identify it as image/png
	// without needing a fully valid PNG file.
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0, 0, 0, 0, 0}
	b64 := base64.StdEncoding.EncodeToString(pngHeader)

	om := toOpenAIMessage(ollamaMessage{Role: "user", Content: "what is this?", Images: []string{b64}})
	parts, ok := om.Content.([]openAIContentPart)
	if !ok {
		t.Fatalf("expected content to be a []openAIContentPart once an image is attached, got %T", om.Content)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 content parts (text + image), got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this?" {
		t.Fatalf("unexpected first content part: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("unexpected second content part: %+v", parts[1])
	}
	if !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("expected a sniffed data:image/png URL, got %q", parts[1].ImageURL.URL)
	}
	if !strings.HasSuffix(parts[1].ImageURL.URL, b64) {
		t.Fatalf("expected the original base64 payload to be preserved verbatim in the data URL")
	}
}

func TestToOpenAIRequestOmitsNumCtxAndRequestsStreamUsage(t *testing.T) {
	req := ollamaRequest{
		Model:   "m",
		Options: ollamaOptions{NumCtx: 32768},
		Stream:  true,
		Messages: []ollamaMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
		Tools: []ollamaTool{{Type: "function", Function: ollamaToolFunction{
			Name: "get_current_time", Description: "d", Parameters: map[string]interface{}{"type": "object"},
		}}},
	}
	out := toOpenAIRequest(req)
	payload, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if strings.Contains(string(payload), "num_ctx") {
		t.Fatalf("expected num_ctx to never appear in an OpenAI-compatible request (no standard equivalent), got: %s", payload)
	}
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Fatal("expected stream_options.include_usage=true to be requested on a streaming request, so token usage can be reported")
	}
	if len(out.Messages) != 2 {
		t.Fatalf("expected both messages to carry over, got %d", len(out.Messages))
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_current_time" {
		t.Fatalf("expected the tool schema to carry over unchanged, got: %+v", out.Tools)
	}
}

func TestToOpenAIRequestNonStreamingOmitsStreamOptions(t *testing.T) {
	out := toOpenAIRequest(ollamaRequest{Model: "m", Stream: false})
	if out.StreamOptions != nil {
		t.Fatalf("expected no stream_options on a non-streaming request, got: %+v", out.StreamOptions)
	}
}

// ---- streamOpenAIResponse ----

// openAISSELine renders one SSE "data: {...}\n\n" chunk matching
// openAIStreamChunk's shape - the OpenAI-compatible counterpart to
// streamLine (coding_integration_test.go, above) for Ollama's NDJSON.
func openAISSELine(content, reasoning string, toolCallsJSON string) string {
	var parts []string
	if content != "" {
		parts = append(parts, fmt.Sprintf(`"content":%q`, content))
	}
	if reasoning != "" {
		parts = append(parts, fmt.Sprintf(`"reasoning_content":%q`, reasoning))
	}
	if toolCallsJSON != "" {
		parts = append(parts, fmt.Sprintf(`"tool_calls":%s`, toolCallsJSON))
	}
	return fmt.Sprintf(`data: {"choices":[{"delta":{%s}}]}`, strings.Join(parts, ",")) + "\n\n"
}

func TestStreamOpenAIResponseAccumulatesContentAndUsage(t *testing.T) {
	body := openAISSELine("Hello, ", "", "") +
		openAISSELine("world!", "", "") +
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		"data: [DONE]\n\n"

	outFile, err := os.CreateTemp(t.TempDir(), "openai-stream-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamOpenAIResponse(strings.NewReader(body), outFile, "", "", "", "")
	if out.Content != "Hello, world!" {
		t.Fatalf("expected accumulated content \"Hello, world!\", got %q", out.Content)
	}
	if out.PromptTokens != 10 || out.EvalTokens != 5 {
		t.Fatalf("expected usage tokens prompt=10/eval=5 from the final chunk, got prompt=%d eval=%d", out.PromptTokens, out.EvalTokens)
	}
}

func TestStreamOpenAIResponseReasoningContentBecomesThinking(t *testing.T) {
	body := openAISSELine("", "let me think...", "") +
		openAISSELine("the answer is 4", "", "") +
		"data: [DONE]\n\n"

	outFile, err := os.CreateTemp(t.TempDir(), "openai-stream-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamOpenAIResponse(strings.NewReader(body), outFile, "", "", "", "")
	if out.Thinking != "let me think..." {
		t.Fatalf("expected reasoning_content to populate Thinking, got %q", out.Thinking)
	}
	if out.Content != "the answer is 4" {
		t.Fatalf("expected content after the reasoning to populate Content, got %q", out.Content)
	}
	if out.ThinkDuration <= 0 {
		t.Fatal("expected a non-zero ThinkDuration once thinking was followed by a real answer")
	}

	logged, _ := os.ReadFile(outFile.Name())
	if !strings.Contains(string(logged), "<<<--Thinking-->>>") || !strings.Contains(string(logged), "<<<--Answer-->>>") {
		t.Fatalf("expected the log file to contain both the thinking and answer banners, got:\n%s", logged)
	}
}

func TestStreamOpenAIResponseAccumulatesToolCallDeltasByIndex(t *testing.T) {
	// A realistic OpenAI-compatible stream: id+name arrive on the tool
	// call's first delta, then only argument fragments on subsequent
	// deltas sharing the same index - split across three chunks here to
	// actually exercise the accumulation, not just a single whole call.
	body := openAISSELine("", "", `[{"index":0,"id":"call_1","type":"function","function":{"name":"create_folder","arguments":""}}]`) +
		openAISSELine("", "", `[{"index":0,"function":{"arguments":"{\"path\":"}}]`) +
		openAISSELine("", "", `[{"index":0,"function":{"arguments":"\"reports\"}"}}]`) +
		"data: [DONE]\n\n"

	outFile, err := os.CreateTemp(t.TempDir(), "openai-stream-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamOpenAIResponse(strings.NewReader(body), outFile, "", "", "", "")
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected exactly 1 accumulated tool call, got %d: %+v", len(out.ToolCalls), out.ToolCalls)
	}
	tc := out.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "create_folder" {
		t.Fatalf("unexpected accumulated tool call id/name: %+v", tc)
	}
	var args map[string]interface{}
	if err := json.Unmarshal(tc.Function.Arguments, &args); err != nil {
		t.Fatalf("expected accumulated arguments to be valid JSON, got %q (err: %v)", tc.Function.Arguments, err)
	}
	if args["path"] != "reports" {
		t.Fatalf("expected accumulated arguments {\"path\":\"reports\"}, got %v", args)
	}
}

func TestStreamOpenAIResponseMultipleToolCallsByDifferentIndex(t *testing.T) {
	body := openAISSELine("", "", `[{"index":0,"id":"call_a","type":"function","function":{"name":"tool_a","arguments":"{}"}},{"index":1,"id":"call_b","type":"function","function":{"name":"tool_b","arguments":"{}"}}]`) +
		"data: [DONE]\n\n"

	outFile, err := os.CreateTemp(t.TempDir(), "openai-stream-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamOpenAIResponse(strings.NewReader(body), outFile, "", "", "", "")
	if len(out.ToolCalls) != 2 {
		t.Fatalf("expected 2 distinct tool calls (one per index), got %d: %+v", len(out.ToolCalls), out.ToolCalls)
	}
	if out.ToolCalls[0].ID != "call_a" || out.ToolCalls[1].ID != "call_b" {
		t.Fatalf("expected tool calls to preserve their original index order, got: %+v", out.ToolCalls)
	}
}

func TestStreamOpenAIResponseMalformedArgumentsFallBackToEmptyObject(t *testing.T) {
	// Simulates a stream that got cut off mid-argument (e.g. a dropped
	// connection) - the accumulated "arguments" text is never valid JSON.
	body := openAISSELine("", "", `[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"unterminat"}}]`) +
		"data: [DONE]\n\n"

	outFile, err := os.CreateTemp(t.TempDir(), "openai-stream-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamOpenAIResponse(strings.NewReader(body), outFile, "", "", "", "")
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected the (malformed) call to still surface as 1 tool call, got %d", len(out.ToolCalls))
	}
	if string(out.ToolCalls[0].Function.Arguments) != "{}" {
		t.Fatalf("expected malformed accumulated arguments to fall back to \"{}\", got %q", out.ToolCalls[0].Function.Arguments)
	}
}

func TestStreamOpenAIResponseErrorChunkDoesNotPanic(t *testing.T) {
	body := `data: {"error":{"message":"something went wrong upstream"}}` + "\n\n" + "data: [DONE]\n\n"

	outFile, err := os.CreateTemp(t.TempDir(), "openai-stream-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	out := streamOpenAIResponse(strings.NewReader(body), outFile, "", "", "", "")
	if out.Content != "" {
		t.Fatalf("expected no content from an error-only stream, got %q", out.Content)
	}
	logged, _ := os.ReadFile(outFile.Name())
	if !strings.Contains(string(logged), "something went wrong upstream") {
		t.Fatalf("expected the error message to be logged, got:\n%s", logged)
	}
}

// ---- end-to-end: cmdAsk against a mocked /chat/completions endpoint ----

// TestCmdAskOpenAIProviderEndToEndToolCallRoundTrip drives the real cmdAsk
// entry point (same style as TestCmdAskCreateFolderAndDelayEndToEnd above,
// just against a mocked OpenAI-compatible /chat/completions endpoint
// instead of Ollama's /api/chat) through one tool-calling round trip, to
// confirm two things end-to-end rather than just at the unit level: (1)
// the tool_calls delta gets accumulated and dispatched correctly, and (2)
// the resulting tool-result message threads the correct tool_call_id back
// into the next round's request - the one piece of this whole feature
// that a purely-unit-level test of toOpenAIMessage/streamOpenAIResponse in
// isolation couldn't actually prove wires together correctly.
func TestCmdAskOpenAIProviderEndToEndToolCallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var round int32
	var secondRoundBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_folder","arguments":""}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"reports\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			b, _ := io.ReadAll(r.Body)
			secondRoundBody = string(b)
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"เสร็จเรียบร้อยครับ"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exitCode := cmdAsk([]string{
		"--provider", "openai", "--api-base", srv.URL,
		"-m", "mock-model", "-o", "ask-openai.log",
		"สร้างโฟลเดอร์ reports หน่อย",
	})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (tool call, then final answer), got %d", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "reports")); statErr != nil {
		t.Fatalf("expected the reports/ directory to actually be created: %v", statErr)
	}
	if !strings.Contains(secondRoundBody, `"tool_call_id":"call_1"`) {
		t.Fatalf("expected the second round's tool-result message to carry tool_call_id=call_1, got request body: %s", secondRoundBody)
	}
	if !strings.Contains(secondRoundBody, `"role":"tool"`) {
		t.Fatalf("expected the second round's request to include a role:tool message, got: %s", secondRoundBody)
	}

	log, err := os.ReadFile("ask-openai.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "provider: openai") {
		t.Fatalf("expected the log header to record provider: openai, got:\n%s", log)
	}
}
