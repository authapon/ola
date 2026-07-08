package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

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
