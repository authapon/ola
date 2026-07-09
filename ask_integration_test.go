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
