package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

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
