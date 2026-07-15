// unit_test.go - focused, no-network unit tests, consolidated from
// coding_test.go, folder_delay_test.go, stream_test.go, notify_test.go,
// time_test.go, scp_test.go, search_test.go, and skills_test.go during a
// file-count cleanup (see integration_test.go for the end-to-end/
// httptest-driven tests, kept separate on purpose so the two styles of
// test don't get mixed together in one file).

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
