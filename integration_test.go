// integration_test.go - end-to-end tests that drive the real cmdAsk/
// cmdCoding entry points against a mocked Ollama /api/chat endpoint
// (httptest), consolidated from ask_integration_test.go,
// coding_integration_test.go, scp_integration_test.go, and
// freshness_test.go during a file-count cleanup. streamLine (originally
// defined in coding_integration_test.go) is the shared NDJSON chunk
// builder used throughout this file.

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

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
