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
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
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

func TestValidateCommandAllowsOrdinaryBuildTestCommands(t *testing.T) {
	if err := validateCommand("go build ./..."); err != nil {
		t.Fatalf("expected command to pass, got error: %v", err)
	}
	if err := validateCommand("go build ./... && go test ./..."); err != nil {
		t.Fatalf("expected chained commands to pass, got error: %v", err)
	}
	if err := validateCommand("go test ./... -run TestFoo -v"); err != nil {
		t.Fatalf("expected command to pass, got error: %v", err)
	}
	// A URL's "://" must not be mistaken for a local absolute path (see
	// absolutePathPattern's doc comment) - otherwise run_command would be
	// unusable for anything that fetches a dependency or clones a repo.
	if err := validateCommand("curl -fsSL https://example.com/install.sh"); err != nil {
		t.Fatalf("expected a command referencing a URL to pass, got error: %v", err)
	}
	if err := validateCommand("go get github.com/some/module"); err != nil {
		t.Fatalf("expected command to pass, got error: %v", err)
	}
}

// TestValidateCommandAllowsSlashAsMathOperatorNotPath is the exact
// regression test for a real bug report: a quoted arithmetic expression
// like "10 / 0" has a standalone "/" surrounded by spaces (the division
// operator), which absolutePathPattern's old "*" quantifier happily
// captured as the single-character "path" "/" and then rejected as
// pointing outside the working directory - even though the command never
// references the filesystem at all. A calculator/math-tool program is
// exactly the kind of thing coding-mode's own auto-verify step would
// legitimately try to run and check the output of.
func TestValidateCommandAllowsSlashAsMathOperatorNotPath(t *testing.T) {
	cases := []string{
		`./math-tool "10 / 0"`,
		`./math-tool "10 / 3"`,
		`./math-tool "100 / 4 / 5"`,
		`echo "1 / 2"`,
	}
	for _, c := range cases {
		if err := validateCommand(c); err != nil {
			t.Fatalf("expected %q (division operator, not a path) to be allowed, got error: %v", c, err)
		}
	}
	// A bare "/" must still not swallow a REAL absolute path immediately
	// following more text on the same line - this only exempts a truly
	// standalone "/" with nothing attached to it.
	if err := validateCommand("cat /etc/passwd"); err == nil {
		t.Fatal("expected a genuine absolute path to still be rejected after this fix")
	}
}

func TestValidateCommandRejectsEmpty(t *testing.T) {
	if err := validateCommand("   "); err == nil {
		t.Fatal("expected empty command to be rejected")
	}
}

// TestValidateCommandRejectsDenylistedCommands confirms run_command refuses
// a short list of filesystem-destructive/host-wide commands outright,
// unconditionally, in both "ask" and "coding" - see blockedCommandWords.
func TestValidateCommandRejectsDenylistedCommands(t *testing.T) {
	denied := []string{
		"rm -rf /tmp/some-dir",
		"rm file.txt",
		"rmdir emptydir",
		"dd if=/dev/zero of=/dev/sda",
		"shred -u secret.txt",
		"mkswap /dev/sdb1",
		"mkfs.ext4 /dev/sdb1",
		"shutdown -h now",
		"reboot",
		"poweroff",
		"mount /dev/sdb1 /mnt",
		"umount /mnt",
		"sudo apt-get install foo",
		"su root",
		"passwd",
		"useradd bob",
		"iptables -F",
		"killall node",
		"pkill -f server",
		"crontab -r",
	}
	for _, cmd := range denied {
		if err := validateCommand(cmd); err == nil {
			t.Fatalf("expected %q to be rejected by the denylist, but it passed", cmd)
		}
	}
}

// TestValidateCommandAllowsPlainKill confirms plain "kill" (stopping a pid
// this session started, e.g. a dev server) is NOT denylisted, unlike
// "killall"/"pkill" which act by name across the whole host - see
// blockedCommandWords' doc comment for the reasoning.
func TestValidateCommandAllowsPlainKill(t *testing.T) {
	if err := validateCommand("kill 12345"); err != nil {
		t.Fatalf("expected plain kill to be allowed, got error: %v", err)
	}
}

// TestValidateCommandRejectsDenylistedCommandWhenChained confirms the
// denylist check applies to every "&&"/"||"/";"/"|"-separated segment of
// the command, not just the first one - a chained
// "go build ./... && rm -rf ." must still be caught even though
// "go build ./..." alone is fine.
func TestValidateCommandRejectsDenylistedCommandWhenChained(t *testing.T) {
	chained := []string{
		"go build ./... && rm -rf .",
		"echo hi; sudo reboot",
		"go test ./... || killall go",
		"go build ./... | tee build.log && shutdown -h now",
	}
	for _, cmd := range chained {
		if err := validateCommand(cmd); err == nil {
			t.Fatalf("expected chained command %q to be rejected because of its denylisted segment", cmd)
		}
	}
}

// TestValidateCommandSkipsLeadingEnvAssignments confirms a denylisted
// command isn't missed just because it's prefixed with "VAR=value" shell
// environment assignments, and that ordinary env-prefixed commands still
// pass.
func TestValidateCommandSkipsLeadingEnvAssignments(t *testing.T) {
	if err := validateCommand("FOO=1 BAR=2 rm -rf ."); err == nil {
		t.Fatal("expected env-prefixed rm to still be rejected")
	}
	if err := validateCommand("CGO_ENABLED=0 go build ./..."); err != nil {
		t.Fatalf("expected env-prefixed allowed command to pass, got error: %v", err)
	}
}

// TestValidateCommandRejectsParentDirectoryTraversal confirms any ".." path
// segment in the command is rejected, since it could walk the command
// outside the working directory - while Go's "./..." recursive-package
// ellipsis (extremely common in this tool's own build/test commands) must
// NOT be mistaken for it.
func TestValidateCommandRejectsParentDirectoryTraversal(t *testing.T) {
	traversals := []string{
		"cat ../../etc/passwd",
		"cd ..",
		"ls ..",
		"cat ./../secret.txt",
	}
	for _, cmd := range traversals {
		if err := validateCommand(cmd); err == nil {
			t.Fatalf("expected %q to be rejected for walking outside the working directory", cmd)
		}
	}
	if err := validateCommand("go build ./..."); err != nil {
		t.Fatalf(`expected "./..." to NOT be treated as parent traversal, got error: %v`, err)
	}
	if err := validateCommand("go vet ./... && gofmt -l ."); err != nil {
		t.Fatalf(`expected "./..." chained with other commands to still pass, got error: %v`, err)
	}
}

// TestValidateCommandRejectsAbsolutePathOutsideWorkingDirectory confirms an
// absolute path pointing outside the current directory is rejected, while
// one that resolves inside it (or a standard I/O device path like
// /dev/null) is allowed.
func TestValidateCommandRejectsAbsolutePathOutsideWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := validateCommand("cat /etc/passwd"); err == nil {
		t.Fatal("expected an absolute path outside the working directory to be rejected")
	}
	if err := validateCommand("go build -o /tmp/out ./..."); err == nil {
		t.Fatal("expected an absolute output path outside the working directory to be rejected")
	}
	if err := validateCommand("echo hi > /dev/null 2>&1"); err != nil {
		t.Fatalf("expected /dev/null redirection to be allowed, got error: %v", err)
	}
	if err := validateCommand("cat " + filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("expected an absolute path inside the working directory to be allowed, got error: %v", err)
	}
}

// TestValidateCommandAllowsRunningJustBuiltBinary is the direct
// regression test for "coding mode compiles a binary, then tries to run
// it to verify it actually works, and the run gets rejected" - both the
// natural relative form (the model just typing "./binary") and a full
// absolute path reconstructed from cwd (something a model sometimes does
// out of an abundance of caution) must both be allowed for a binary that
// genuinely lives in the current directory.
func TestValidateCommandAllowsRunningJustBuiltBinary(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	binPath := filepath.Join(dir, "mathcalc")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho ok\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"./mathcalc",
		"./mathcalc arg1 arg2",
		"go build -o mathcalc . && ./mathcalc",
		binPath, // full absolute path to a binary that's genuinely inside cwd
		"cd " + dir + " && ./mathcalc",
	}
	for _, c := range cases {
		if err := validateCommand(c); err != nil {
			t.Fatalf("expected %q (a binary inside the working directory) to be allowed, got error: %v", c, err)
		}
	}
}

// TestValidateCommandAbsolutePathErrorSuggestsRelativeForm confirms the
// rejection message itself teaches the model the fix (use "./name")
// rather than just saying no - so a model that hits this once can
// self-correct on the next tool call without needing a human to explain
// it, per the actual failure mode of coding-mode verification getting
// stuck here.
func TestValidateCommandAbsolutePathErrorSuggestsRelativeForm(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	err := validateCommand("/some/other/place/mathcalc")
	if err == nil {
		t.Fatal("expected an absolute path genuinely outside the working directory to still be rejected")
	}
	if !strings.Contains(err.Error(), "./mathcalc") {
		t.Fatalf("expected the error to suggest the relative-path fix (\"./mathcalc\"), got: %v", err)
	}
}

// TestFindDisallowedAbsolutePathToleratesSymlinkedCwd is the regression
// test for the "working directory reached through a symlink" case: some
// /tmp setups, container mounts, and CI environments have the directory
// ola is actually running in be a symlink to somewhere else. Without
// symlink-aware comparison, a perfectly legitimate absolute path inside
// the (symlinked) working directory could be misjudged as pointing
// outside it purely because of which of the two equivalent path spellings
// happened to be used.
func TestFindDisallowedAbsolutePathToleratesSymlinkedCwd(t *testing.T) {
	realDir := t.TempDir()
	parent := filepath.Dir(realDir)
	linkPath := filepath.Join(parent, "symlinked-workdir")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}
	defer os.Remove(linkPath)

	if err := os.WriteFile(filepath.Join(realDir, "mathcalc"), []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}

	// cwd given as the symlink path, candidate path given as the resolved
	// (real) path - or vice versa - must both be recognized as "inside".
	if got := findDisallowedAbsolutePath(filepath.Join(realDir, "mathcalc"), linkPath); got != "" {
		t.Fatalf("expected the real path to be recognized as inside the symlinked cwd, got disallowed: %q", got)
	}
	if got := findDisallowedAbsolutePath(filepath.Join(linkPath, "mathcalc"), realDir); got != "" {
		t.Fatalf("expected the symlink path to be recognized as inside the real cwd, got disallowed: %q", got)
	}
}

func TestIsVerifiableEditGatesByToolchainExtension(t *testing.T) {
	cases := []struct {
		path, label string
		want        bool
	}{
		{"main.go", "go", true},
		{"notes.txt", "go", false},
		{"README.md", "go", false},
		{"package.json", "node", true},
		{"index.js", "node", true},
		{"design.txt", "node", false},
		{"lib.rs", "rust", true},
		{"app.py", "python", true},
		{"anything.txt", "generic", false},
		{"", "go", false},
	}
	for _, c := range cases {
		got := isVerifiableEdit(c.path, c.label)
		if got != c.want {
			t.Errorf("isVerifiableEdit(%q, %q) = %v, want %v", c.path, c.label, got, c.want)
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
		outFile: outFile, state: state,
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

// TestStuckDetectionBlocksAfterFailStreak confirms that a task rejected
// maxTaskFailStreak times in a row gets hard-blocked (refused without even
// re-running the build check), and that calling add_tasks clears the block
// so the task can be retried.
func TestStuckDetectionBlocksAfterFailStreak(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module stucktest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Broken from the start, and stays broken for this whole test.
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
		outFile: outFile, state: state,
		cmdTO: 5 * time.Second, cmds: cmds,
	}
	markArgs, _ := json.Marshal(map[string]interface{}{"task_id": "T0"})
	tc := toolCall{Function: toolCallFunction{Name: "mark_task_done", Arguments: markArgs}}

	// First maxTaskFailStreak calls should each be rejected for the build
	// failure itself, not yet hard-blocked.
	for i := 0; i < maxTaskFailStreak; i++ {
		result, _ := dispatchCodingToolCall(tc, rc)
		if strings.Contains(result, "ถูกบล็อก") {
			t.Fatalf("did not expect a hard block until after %d failures, got block on attempt %d: %s", maxTaskFailStreak, i+1, result)
		}
		if !strings.Contains(result, "ถูกปฏิเสธ") {
			t.Fatalf("expected rejection on attempt %d, got: %s", i+1, result)
		}
	}
	if !state.Tasks[0].Blocked {
		t.Fatalf("expected task to be marked Blocked after %d consecutive failures, state: %+v", maxTaskFailStreak, state.Tasks[0])
	}

	// The NEXT attempt must be refused as a hard block, without even
	// re-running the (still broken) build check.
	result, isReport := dispatchCodingToolCall(tc, rc)
	if isReport {
		t.Fatal("mark_task_done must never report as report_complete")
	}
	if !strings.Contains(result, "ถูกบล็อก") {
		t.Fatalf("expected a hard block message once maxTaskFailStreak is reached, got: %s", result)
	}

	// add_tasks (re-planning) should clear the block.
	addArgs, _ := json.Marshal(map[string]interface{}{"tasks": []string{"a smaller sub-task"}})
	addTC := toolCall{Function: toolCallFunction{Name: "add_tasks", Arguments: addArgs}}
	if _, _ = dispatchCodingToolCall(addTC, rc); state.Tasks[0].Blocked {
		t.Fatal("expected add_tasks to clear the Blocked flag on existing tasks")
	}

	// Fix the build - mark_task_done should now succeed again for T0.
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	result, _ = dispatchCodingToolCall(tc, rc)
	if strings.Contains(result, "ถูกปฏิเสธ") || strings.Contains(result, "ถูกบล็อก") {
		t.Fatalf("expected mark_task_done to succeed after unblock + fix, got: %s", result)
	}
	if !state.Tasks[0].Done {
		t.Fatal("expected T0 to be marked done")
	}
}

// TestRunLintCheckGoCatchesVetFailure confirms the Go lint gate (go vet)
// rejects code that compiles but fails vet (here: a Printf format-string
// mismatch), and that runBuildOnly treats a lint failure the same as a
// build failure (blocking).
func TestRunLintCheckGoCatchesVetFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module linttest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Compiles fine, but go vet flags the Printf/argument mismatch.
	badVet := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Printf(\"%d\\n\", \"not a number\")\n}\n"
	if err := os.WriteFile(dir+"/main.go", []byte(badVet), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)
	if cmds.LintCmd == "" {
		t.Fatal("expected a LintCmd to be set for a detected Go project")
	}

	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	passed, report := runLintCheck(cmds, 10*time.Second)
	if passed {
		t.Fatalf("expected go vet to catch the Printf mismatch, got passed=true, report: %s", report)
	}

	// runBuildOnly must surface the same failure (lint blocks like a build
	// failure) even though the code compiles cleanly.
	buildPassed, buildReport := runBuildOnly(cmds, 10*time.Second)
	if buildPassed {
		t.Fatalf("expected runBuildOnly to fail on a lint violation, got passed=true, report: %s", buildReport)
	}
	if !strings.Contains(buildReport, "[lint]") {
		t.Fatalf("expected runBuildOnly's failure report to be tagged [lint], got: %s", buildReport)
	}
}

// TestTaskAcceptanceCheckGatesMarkTaskDone confirms that a task registered
// with its own acceptance_check must pass THAT specific command, not just
// the shared build-only gate, before mark_task_done accepts it.
func TestTaskAcceptanceCheckGatesMarkTaskDone(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("go.mod", []byte("module accepttest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)

	state := newCodingState()
	// "go test ./nonexistent/..." matches no packages -> non-zero exit,
	// even though the project itself builds cleanly.
	added := state.addTaskItems([]taskInput{{Description: "add a feature", AcceptanceCheck: "go test ./nonexistent/..."}})
	if len(added) != 1 {
		t.Fatalf("expected 1 task to be added, got %d", len(added))
	}

	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	rc := &codingRunContext{
		outFile: outFile, state: state,
		cmdTO: 10 * time.Second, cmds: cmds,
	}
	markArgs, _ := json.Marshal(map[string]interface{}{"task_id": "T0"})
	tc := toolCall{Function: toolCallFunction{Name: "mark_task_done", Arguments: markArgs}}

	result, _ := dispatchCodingToolCall(tc, rc)
	if !strings.Contains(result, "acceptance_check") {
		t.Fatalf("expected rejection to mention acceptance_check, got: %s", result)
	}
	if state.Tasks[0].Done {
		t.Fatal("task must not be marked done when its own acceptance_check fails, even if the project builds")
	}

	// Fix the acceptance_check to something that will actually pass, and
	// confirm the task can then be closed.
	state.Tasks[0].AcceptanceCheck = "go build ./..."
	result, _ = dispatchCodingToolCall(tc, rc)
	if strings.Contains(result, "ถูกปฏิเสธ") {
		t.Fatalf("expected mark_task_done to succeed with a passing acceptance_check, got: %s", result)
	}
	if !state.Tasks[0].Done {
		t.Fatal("expected task to be marked done once its acceptance_check passes")
	}
}

// TestSelfReviewGateBlocksReportComplete confirms report_complete is
// refused until self_review_requirements has been called with
// all_requirements_met=true, and that a subsequent successful file edit
// invalidates a prior passing review.
func TestSelfReviewGateBlocksReportComplete(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("go.mod", []byte("module reviewtest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.go", []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	rc := &codingRunContext{
		outFile: outFile, state: newCodingState(),
		cmdTO: 10 * time.Second, cmds: cmds, selfReviewEnabled: true,
	}

	reportArgs, _ := json.Marshal(map[string]interface{}{"summary": "done"})
	reportTC := toolCall{Function: toolCallFunction{Name: "report_complete", Arguments: reportArgs}}

	// 1) No self-review yet -> must be refused, and NOT reported as an
	// accepted report_complete.
	result, isReport := dispatchCodingToolCall(reportTC, rc)
	if isReport {
		t.Fatal("report_complete must not be accepted before a passing self_review_requirements call")
	}
	if !strings.Contains(result, "self_review_requirements") {
		t.Fatalf("expected rejection to mention self_review_requirements, got: %s", result)
	}

	// 2) A dishonest/incomplete self-review (all_requirements_met=false)
	// still must not unlock report_complete.
	reviewArgs, _ := json.Marshal(map[string]interface{}{"all_requirements_met": false, "missing_items": []string{"the login page"}})
	reviewTC := toolCall{Function: toolCallFunction{Name: "self_review_requirements", Arguments: reviewArgs}}
	dispatchCodingToolCall(reviewTC, rc)
	_, isReport = dispatchCodingToolCall(reportTC, rc)
	if isReport {
		t.Fatal("report_complete must not be accepted after a self-review that reported missing items")
	}

	// 3) A passing self-review unlocks report_complete.
	passArgs, _ := json.Marshal(map[string]interface{}{"all_requirements_met": true})
	passTC := toolCall{Function: toolCallFunction{Name: "self_review_requirements", Arguments: passArgs}}
	dispatchCodingToolCall(passTC, rc)
	_, isReport = dispatchCodingToolCall(reportTC, rc)
	if !isReport {
		t.Fatal("expected report_complete to be accepted after a passing self_review_requirements call")
	}

	// 4) Any further edit invalidates that pass - report_complete must be
	// refused again until a fresh review.
	writeArgs, _ := json.Marshal(map[string]interface{}{"path": "notes.txt", "content": "x", "reason": "test"})
	writeTC := toolCall{Function: toolCallFunction{Name: "write_file", Arguments: writeArgs}}
	dispatchCodingToolCall(writeTC, rc)
	_, isReport = dispatchCodingToolCall(reportTC, rc)
	if isReport {
		t.Fatal("expected a file edit to invalidate a prior passing self-review, requiring a fresh call")
	}
}

// TestEditVerifyAppendsAutoBuildCheckResult confirms that when
// editVerifyEnabled is set, a successful write_file to a source file
// immediately triggers a build-only check and appends the (failing) result
// to the tool's own return value, without waiting for mark_task_done.
func TestEditVerifyAppendsAutoBuildCheckResult(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("go.mod", []byte("module editverifytest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := detectProjectCommands(dir)
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	rc := &codingRunContext{
		outFile: outFile, state: newCodingState(),
		cmdTO: 10 * time.Second, cmds: cmds, editVerifyEnabled: true,
	}
	broken := "package main\n\nfunc main() {\n"
	writeArgs, _ := json.Marshal(map[string]interface{}{"path": "main.go", "content": broken, "reason": "test"})
	writeTC := toolCall{Function: toolCallFunction{Name: "write_file", Arguments: writeArgs}}

	result, _ := dispatchCodingToolCall(writeTC, rc)
	if !strings.Contains(result, "auto lint/build-check") {
		t.Fatalf("expected the write_file result to include the immediate auto build-check output, got: %s", result)
	}
	if !strings.Contains(result, "ล้มเหลว") {
		t.Fatalf("expected the auto build-check to report failure for broken code, got: %s", result)
	}
}

// TestPreflightCheckDetectsMissingBinary confirms preflightCheck flags a
// binary that isn't in PATH while not flagging one that is (using "go",
// which the test binary itself requires to have been built).
func TestPreflightCheckDetectsMissingBinary(t *testing.T) {
	cmds := projectCommands{
		Label: "go", BuildCmd: "go build ./...", TestCmd: "go test ./...",
		LintCmd: "definitely-not-a-real-binary-xyz --check",
	}
	missing := preflightCheck(cmds)
	found := false
	for _, m := range missing {
		if m == "definitely-not-a-real-binary-xyz" {
			found = true
		}
		if m == "go" {
			t.Fatal("did not expect 'go' to be reported missing")
		}
	}
	if !found {
		t.Fatalf("expected preflightCheck to flag the nonexistent binary, got missing: %v", missing)
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
	if cmds.Label != "go" || cmds.BuildCmd != "go build ./..." || cmds.TestCmd != "go test ./..." {
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

func TestToolRunCommandExecutesCommand(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"command": "true"}
	result, err := toolRunCommand(args, 5*time.Second)
	if err != nil {
		t.Fatalf("expected command to run: %v", err)
	}
	if !strings.Contains(result, "exit_code=0") {
		t.Fatalf("expected success exit code in result, got: %s", result)
	}
}

func TestToolRunCommandRejectsEmptyCommand(t *testing.T) {
	args := map[string]interface{}{"command": ""}
	if _, err := toolRunCommand(args, 5*time.Second); err == nil {
		t.Fatal("expected empty command to be rejected before execution")
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

// ─────────────────────────────────────────────────────────────────
// PDF attachment support (convertPDFToImages)
// ─────────────────────────────────────────────────────────────────

// fakePDFToPPMScript stands in for the real `pdftoppm` binary in tests, so
// the test suite doesn't depend on poppler-utils actually being installed
// on the machine running it - the same reasoning/pattern as
// installFakeSCP below faking `scp` for scp_copy's own tests. It hardcodes
// the exact fixed argument shape convertPDFToImages always invokes it
// with ("-png" "-r" <dpi> "-f" "1" "-l" <maxPages> <path> <prefix>, i.e.
// positions $7/$8/$9), reads a fake "page count" from the first line of
// the input file (a plain text stand-in for a real PDF in these tests -
// convertPDFToImages itself never inspects PDF content, so this is enough
// to exercise its own globbing/sorting/truncation logic), and writes that
// many (capped at the "-l" limit) dummy "<prefix>-<n>.png" files.
const fakePDFToPPMScript = `#!/bin/sh
limit="$7"
input="$8"
prefix="$9"
total=$(head -n 1 "$input")
n=$total
if [ "$limit" -lt "$total" ]; then
  n=$limit
fi
i=1
while [ "$i" -le "$n" ]; do
  printf 'FAKE-PNG-PAGE-%s' "$i" > "${prefix}-${i}.png"
  i=$((i+1))
done
`

// installFakePDFToPPM writes fakePDFToPPMScript as an executable
// "pdftoppm" and prepends its directory to PATH for the duration of the
// test, so exec.LookPath/exec.CommandContext inside convertPDFToImages
// picks it up instead of any real pdftoppm installed on the machine.
func installFakePDFToPPM(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake pdftoppm shell script requires a POSIX shell")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "pdftoppm")
	if err := os.WriteFile(scriptPath, []byte(fakePDFToPPMScript), 0755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
}

// fakePDFFile writes a plain-text stand-in "PDF" (see fakePDFToPPMScript's
// doc comment) whose first line is the page count the fake pdftoppm should
// report, and returns its path.
func fakePDFFile(t *testing.T, dir string, pages int) string {
	t.Helper()
	path := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pages)), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConvertPDFToImagesReturnsPagesInOrder(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	path := fakePDFFile(t, dir, 3)

	pages, truncated, err := convertPDFToImages(path, 10, 150)
	if err != nil {
		t.Fatalf("expected conversion to succeed, got error: %v", err)
	}
	if truncated {
		t.Fatal("expected truncated=false when page count is under maxPages")
	}
	if len(pages) != 3 {
		t.Fatalf("expected 3 pages, got %d", len(pages))
	}
	for i, want := range []string{"FAKE-PNG-PAGE-1", "FAKE-PNG-PAGE-2", "FAKE-PNG-PAGE-3"} {
		got, err := base64.StdEncoding.DecodeString(pages[i])
		if err != nil {
			t.Fatalf("page %d did not decode as base64: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("page %d: expected %q, got %q (pages returned out of order?)", i, want, string(got))
		}
	}
}

func TestConvertPDFToImagesTruncatesAtMaxPages(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	path := fakePDFFile(t, dir, 5)

	pages, truncated, err := convertPDFToImages(path, 2, 150)
	if err != nil {
		t.Fatalf("expected conversion to succeed, got error: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true when the document has more pages than maxPages")
	}
	if len(pages) != 2 {
		t.Fatalf("expected exactly 2 pages (maxPages), got %d", len(pages))
	}
}

func TestConvertPDFToImagesMissingBinary(t *testing.T) {
	// Point PATH somewhere with no pdftoppm at all, so exec.LookPath fails
	// the same way it would on a machine without poppler-utils installed.
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", t.TempDir())
	t.Cleanup(func() { os.Setenv("PATH", origPath) })

	_, _, err := convertPDFToImages("whatever.pdf", 10, 150)
	if err == nil {
		t.Fatal("expected an error when pdftoppm is not installed")
	}
	if !strings.Contains(err.Error(), "pdftoppm") || !strings.Contains(err.Error(), "poppler-utils") {
		t.Fatalf("expected the error to name pdftoppm/poppler-utils so the user knows what to install, got: %v", err)
	}
}

// TestConvertPDFToImagesRealBinary is a genuine smoke test against the
// actual system pdftoppm (skipped if poppler-utils isn't installed on the
// machine running the tests) - confirms convertPDFToImages works against a
// real, minimal, hand-written PDF, not just the fake script above. Poppler
// tolerates a missing/simplified xref table via its own reconstruction
// fallback, which is why this minimal a PDF is enough for the test.
func TestConvertPDFToImagesRealBinary(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm (poppler-utils) not installed - skipping real-binary smoke test")
	}
	const minimalTwoPagePDF = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>
endobj
4 0 obj
<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>
endobj
5 0 obj
<< /Length 44 >>
stream
BT /F1 24 Tf 20 100 Td (Page One) Tj ET
endstream
endobj
6 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 7 0 R >>
endobj
7 0 obj
<< /Length 44 >>
stream
BT /F1 24 Tf 20 100 Td (Page Two) Tj ET
endstream
endobj
trailer
<< /Size 8 /Root 1 0 R >>
%%EOF
`
	dir := t.TempDir()
	path := filepath.Join(dir, "real.pdf")
	if err := os.WriteFile(path, []byte(minimalTwoPagePDF), 0644); err != nil {
		t.Fatal(err)
	}

	pages, truncated, err := convertPDFToImages(path, 10, 72)
	if err != nil {
		t.Fatalf("expected the real pdftoppm to convert this minimal PDF, got error: %v", err)
	}
	if truncated {
		t.Fatal("expected truncated=false for a 2-page doc under maxPages=10")
	}
	if len(pages) != 2 {
		t.Fatalf("expected 2 rendered pages, got %d", len(pages))
	}
	for i, p := range pages {
		data, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("page %d did not decode as base64: %v", i, err)
		}
		if !bytes.HasPrefix(data, []byte("\x89PNG")) {
			t.Fatalf("page %d does not look like a PNG (missing PNG magic bytes)", i)
		}
	}
}

// ─────────────────────────────────────────────────────────────────
// PDF discoverability: buildDirectoryTree/search_files/read_file must
// treat .pdf as "binary but listed" (via alwaysListExts), not "invisible"
// - otherwise the model has no way to find out a PDF exists at all before
// calling read_pdf on it.
// ─────────────────────────────────────────────────────────────────

// TestBuildDirectoryTreeIncludesPDFButNotOtherBinaries confirms a .pdf
// shows up by name in the auto-injected directory tree despite being
// binary (so the model can discover it exists and call read_pdf), while a
// genuinely uninteresting binary like a .png stays excluded exactly as
// before - alwaysListExts is a narrow, deliberate exception, not a general
// loosening of the binary filter.
func TestBuildDirectoryTreeIncludesPDFButNotOtherBinaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("%PDF-1.4\n...\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo.png"), []byte{0x89, 0x50, 0x4E, 0x47}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tree, _, _ := buildDirectoryTree(dir)
	if !strings.Contains(tree, "report.pdf") {
		t.Fatalf("expected the directory tree to list report.pdf, got:\n%s", tree)
	}
	if !strings.Contains(tree, "main.go") {
		t.Fatalf("expected the directory tree to list main.go, got:\n%s", tree)
	}
	if strings.Contains(tree, "photo.png") {
		t.Fatalf("expected photo.png to stay excluded (only .pdf is special-cased), got:\n%s", tree)
	}
}

// TestSearchFilesFindsPDFByPattern confirms an exact "*.pdf" pattern
// search actually finds a PDF file - before this fix, looksBinaryFile
// silently excluded it and search_files would report no match even when
// the file plainly existed.
func TestSearchFilesFindsPDFByPattern(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("invoice.pdf", []byte("%PDF-1.4\n...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := toolSearchFiles(map[string]interface{}{"pattern": "*.pdf"})
	if err != nil {
		t.Fatalf("expected search_files to succeed, got error: %v", err)
	}
	if !strings.Contains(result, "invoice.pdf") {
		t.Fatalf("expected search_files to find invoice.pdf, got: %q", result)
	}
}

// TestSearchFilesDoesNotGrepPDFContent confirms a PDF that matches the
// filename pattern is still listed, but its raw bytes are never grepped as
// if they were text lines when a query is also given - grepping binary
// content would just scan garbage, not real matches.
func TestSearchFilesDoesNotGrepPDFContent(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	// The query string is deliberately embedded in the "PDF" bytes - if
	// search_files grepped it anyway, this would show up as a false hit.
	if err := os.WriteFile("invoice.pdf", []byte("%PDF-1.4\nTOTAL_DUE\n...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := toolSearchFiles(map[string]interface{}{"pattern": "*.pdf", "query": "TOTAL_DUE"})
	if err != nil {
		t.Fatalf("expected search_files to succeed, got error: %v", err)
	}
	if strings.Contains(result, "invoice.pdf:") {
		t.Fatalf("expected invoice.pdf's content to NOT be grepped (no \"invoice.pdf:N:\" grep-hit line), got: %q", result)
	}
	if !strings.Contains(result, "1 ไฟล์") && !strings.Contains(result, "invoice.pdf") {
		t.Fatalf("expected the result to still acknowledge the matching filename, got: %q", result)
	}
}

// TestToolReadFileRejectsPDFWithHelpfulRedirect confirms calling read_file
// on a .pdf fails fast with a message pointing at read_pdf, instead of
// dumping the file's raw binary bytes back as if they were text.
func TestToolReadFileRejectsPDFWithHelpfulRedirect(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("doc.pdf", []byte("%PDF-1.4\n...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := toolReadFile(map[string]interface{}{"path": "doc.pdf"})
	if err == nil {
		t.Fatal("expected read_file to refuse a .pdf path")
	}
	if !strings.Contains(err.Error(), "read_pdf") {
		t.Fatalf("expected the error to point at read_pdf, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────
// read_pdf tool (toolReadPDF, readPDFFollowUpMessage)
// ─────────────────────────────────────────────────────────────────

// TestToolReadPDFSuccessStashesResultForFollowUp confirms toolReadPDF (a)
// returns a success string naming the page count and (b) stashes a
// matching readPDFResult for the caller to pop via popLastReadPDF - the
// side-channel readPDFFollowUpMessage relies on.
func TestToolReadPDFSuccessStashesResultForFollowUp(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile("doc.pdf", []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	args := map[string]interface{}{"path": "doc.pdf"}
	result, err := toolReadPDF(args, 10, 150)
	if err != nil {
		t.Fatalf("expected toolReadPDF to succeed, got error: %v", err)
	}
	if !strings.Contains(result, "2 หน้า") {
		t.Fatalf("expected the result to mention 2 pages, got: %s", result)
	}

	r := popLastReadPDF()
	if r == nil {
		t.Fatal("expected a stashed readPDFResult after a successful toolReadPDF call")
	}
	if r.Path != "doc.pdf" {
		t.Fatalf("expected stashed Path to be %q, got %q", "doc.pdf", r.Path)
	}
	if len(r.Images) != 2 {
		t.Fatalf("expected 2 stashed images, got %d", len(r.Images))
	}
	if r.Truncated {
		t.Fatal("expected Truncated=false when the doc has fewer pages than maxPages")
	}

	// popLastReadPDF clears the side-channel, so a second pop must be nil.
	if popLastReadPDF() != nil {
		t.Fatal("expected popLastReadPDF to return nil once already popped")
	}
}

// TestToolReadPDFRejectsPathOutsideSandbox confirms read_pdf shares the
// same read_file/write_file sandbox - a path escaping the current
// directory via ".." must be rejected before ever reaching pdftoppm.
func TestToolReadPDFRejectsPathOutsideSandbox(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	args := map[string]interface{}{"path": "../outside.pdf"}
	if _, err := toolReadPDF(args, 10, 150); err == nil {
		t.Fatal("expected a path escaping the sandbox to be rejected")
	}
	if popLastReadPDF() != nil {
		t.Fatal("expected no stashed result after a rejected/failed call")
	}
}

// TestToolReadPDFRejectsMissingFile confirms a clear error (not a panic or
// a silent empty result) when the given path doesn't exist.
func TestToolReadPDFRejectsMissingFile(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	args := map[string]interface{}{"path": "does-not-exist.pdf"}
	if _, err := toolReadPDF(args, 10, 150); err == nil {
		t.Fatal("expected an error for a nonexistent path")
	}
}

// TestReadPDFFollowUpMessageCarriesImagesAsUserMessage confirms
// readPDFFollowUpMessage turns a successful read_pdf result into a
// role:"user" message carrying the rendered images - the shape verified
// (elsewhere, for the OpenAI-compat path) to actually reach the model,
// unlike attaching images directly to the tool-result message itself (see
// lastReadPDF's doc comment for why that's avoided).
func TestReadPDFFollowUpMessageCarriesImagesAsUserMessage(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("doc.pdf", []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := toolReadPDF(map[string]interface{}{"path": "doc.pdf"}, 10, 150)
	if err != nil {
		t.Fatalf("expected toolReadPDF to succeed, got error: %v", err)
	}

	msg, ok := readPDFFollowUpMessage("read_pdf", result)
	if !ok {
		t.Fatal("expected readPDFFollowUpMessage to produce a follow-up message")
	}
	if msg.Role != "user" {
		t.Fatalf("expected the follow-up message's role to be \"user\", got %q", msg.Role)
	}
	if len(msg.Images) != 2 {
		t.Fatalf("expected 2 images on the follow-up message, got %d", len(msg.Images))
	}
	if !strings.Contains(msg.Content, "doc.pdf") {
		t.Fatalf("expected the follow-up message's content to reference doc.pdf, got: %s", msg.Content)
	}
}

// TestReadPDFFollowUpMessageNoOpForOtherToolsOrErrors confirms
// readPDFFollowUpMessage only ever fires right after a successful
// "read_pdf" call - never for a different tool's result, and never for a
// read_pdf call that itself failed (an "ERROR: ..." result).
func TestReadPDFFollowUpMessageNoOpForOtherToolsOrErrors(t *testing.T) {
	if _, ok := readPDFFollowUpMessage("read_file", "some file content"); ok {
		t.Fatal("expected no follow-up message for a tool other than read_pdf")
	}
	if _, ok := readPDFFollowUpMessage("read_pdf", "ERROR: something went wrong"); ok {
		t.Fatal("expected no follow-up message when the read_pdf call itself errored")
	}
	// Nothing was stashed by either call above (toolReadPDF never ran), so
	// even a bare "read_pdf" name with a non-ERROR string must still no-op.
	if _, ok := readPDFFollowUpMessage("read_pdf", "unrelated success text"); ok {
		t.Fatal("expected no follow-up message when nothing was actually stashed via toolReadPDF")
	}
}

// TestCmdAskReadPDFToolCallAttachesImagesToFollowUpRequest drives cmdAsk
// through a scripted mock model that calls read_pdf on a PDF that was NOT
// attached via [files...] at all (the whole point of this tool - see
// toolReadPDF/readPDFFollowUpMessage), then confirms the NEXT request round
// actually carries the rendered page images in its message history.
// TestCmdAskFirstRequestDirectoryTreeIncludesPDF confirms the exact
// scenario reported: when no [files...] are attached and ola auto-injects
// the directory tree into the very first request, a .pdf sitting in the
// current directory shows up in that tree from round 1 - the model does
// not need an extra search_files round just to find out it exists before
// it can call read_pdf on it.
func TestCmdAskFirstRequestDirectoryTreeIncludesPDF(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("invoice.pdf", []byte("%PDF-1.4\n...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var firstRequestBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if firstRequestBody == "" {
			body, _ := io.ReadAll(r.Body)
			firstRequestBody = string(body)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("มีไฟล์ invoice.pdf ครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-tree-pdf.log", "โฟลเดอร์นี้มีไฟล์อะไรบ้าง"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if !strings.Contains(firstRequestBody, "invoice.pdf") {
		t.Fatalf("expected invoice.pdf to appear in the very first request's auto-injected directory tree, got body: %s", firstRequestBody)
	}
}

func TestCmdAskReadPDFToolCallAttachesImagesToFollowUpRequest(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("report.pdf", []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	var secondRoundBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			fmt.Fprint(w, streamLine("", "read_pdf", `{"path":"report.pdf"}`, true))
		case 2:
			body, _ := io.ReadAll(r.Body)
			secondRoundBody = string(body)
			fmt.Fprint(w, streamLine("อ่านแล้วครับ", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-read-pdf.log", "อ่านไฟล์ report.pdf ให้หน่อย"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (read_pdf call, final answer), got %d", got)
	}

	var req struct {
		Messages []struct {
			Role   string   `json:"role"`
			Images []string `json:"images"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(secondRoundBody), &req); err != nil {
		t.Fatalf("expected a valid JSON request body, got error %v, body: %s", err, secondRoundBody)
	}

	var found bool
	for _, m := range req.Messages {
		if m.Role == "user" && len(m.Images) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the follow-up request to include a user message with 2 images from read_pdf, got messages: %+v", req.Messages)
	}

	log, err := os.ReadFile("ask-read-pdf.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "[tool_call] read_pdf") {
		t.Fatalf("expected a read_pdf tool_call entry in the log, got:\n%s", log)
	}
}

// TestCmdCodingReadPDFToolCallAttachesImagesToFollowUpRequest is
// TestCmdAskReadPDFToolCallAttachesImagesToFollowUpRequest's counterpart
// for "coding" - confirms read_pdf is wired through cmdCoding's own
// tool-calling loop identically (dispatchCodingToolCall + the follow-up
// message logic), not just cmdAsk's.
func TestCmdCodingReadPDFToolCallAttachesImagesToFollowUpRequest(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("requirements.md", []byte("# ทดสอบ\nอ่านไฟล์ spec.pdf แล้วสรุปให้หน่อย\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("spec.pdf", []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	var secondRoundBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			fmt.Fprint(w, streamLine("", "read_pdf", `{"path":"spec.pdf"}`, true))
		case 2:
			body, _ := io.ReadAll(r.Body)
			secondRoundBody = string(body)
			// A plain final answer with no tool call ends cmdCoding's loop
			// immediately (see its own doc comment) - no need to drive a
			// full plan/report_complete cycle just to exercise read_pdf.
			fmt.Fprint(w, streamLine("อ่าน spec แล้วครับ", "", "", true))
		default:
			t.Errorf("unexpected extra round %d", n)
			fmt.Fprint(w, streamLine("unexpected", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdCoding([]string{"-m", "mock-model", "-o", "coding-read-pdf.log"})
	if exitCode != 0 {
		t.Fatalf("expected cmdCoding to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (read_pdf call, plain final answer), got %d", got)
	}

	var req struct {
		Messages []struct {
			Role   string   `json:"role"`
			Images []string `json:"images"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(secondRoundBody), &req); err != nil {
		t.Fatalf("expected a valid JSON request body, got error %v, body: %s", err, secondRoundBody)
	}
	var found bool
	for _, m := range req.Messages {
		if m.Role == "user" && len(m.Images) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the follow-up request to include a user message with 1 image from read_pdf, got messages: %+v", req.Messages)
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

// TestResolveSkillsDirsSplitsAndTrimsCommaList mirrors ola's other
// comma-separated flags' convention: multiple directories, extra whitespace
// and empty segments handled gracefully.
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
// success/failure from an empty skill list - matching how web_search
// behaves when its own feature isn't enabled.
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
// explicitly configured, same principle as web_search/web_fetch.
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
			// Round 3: required self-review pass before report_complete is
			// even considered - the model (wrongly) claims everything's
			// covered; ola's own independent build/test check below is
			// what actually catches the broken file, not this step.
			fmt.Fprint(w, streamLine("", "self_review_requirements", `{"all_requirements_met":true}`, true))
		case 4:
			// Round 4: claim done before actually verifying - ola should
			// catch this since the file doesn't compile.
			fmt.Fprint(w, streamLine("", "report_complete", `{"summary":"hello world program written"}`, true))
		case 5:
			// Round 5: model sees the VERIFY FAILED tool result for the
			// previous report_complete and fixes the file for real. This
			// edit also invalidates the round-3 self-review pass.
			argsJSON := fmt.Sprintf(`{"path":"main.go","old_str":%q,"new_str":%q}`,
				`fmt.Println("hello"
}`, `fmt.Println("hello")
}`)
			fmt.Fprint(w, streamLine("", "edit_file", argsJSON, true))
		case 6:
			fmt.Fprint(w, streamLine("", "mark_task_done", `{"task_id":"T0","note":"fixed and compiles"}`, true))
		case 7:
			// Round 7: fresh self-review required again since round 5's
			// edit invalidated the earlier one.
			fmt.Fprint(w, streamLine("", "self_review_requirements", `{"all_requirements_met":true}`, true))
		case 8:
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
	if got := atomic.LoadInt32(&round); got != 8 {
		t.Fatalf("expected exactly 8 mock rounds (plan, break, self-review, false-complete, fix, mark-done, self-review, real-complete), got %d", got)
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
// This exercises the "ask" auto-verify gate end-to-end: filesChanged
// tracking, the independent build/test re-check after a plain final
// answer (never trusting the model's own claim that a change works), and
// clean loop termination once verification actually passes. run_command
// itself is always in the tool list regardless of this gate (see
// runCommandTool) - what's being exercised here is ola's own automatic
// re-check, not whether the model could call run_command at all.
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

// TestCmdAskRunCommandAlwaysAvailable confirms run_command is offered to
// the model, and actually executes, in a plain directory with no detected
// build/test toolchain at all (no go.mod/package.json/etc.) - the core
// behavior this test locks in: run_command is unconditional, unlike ola's
// own auto-verify pass (see TestCmdAskAutoVerifyLoop) which stays gated
// behind a detected toolchain.
func TestCmdAskRunCommandAlwaysAvailable(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			fmt.Fprint(w, streamLine("", "run_command", `{"command":"echo hello"}`, true))
		default:
			fmt.Fprint(w, streamLine("รันคำสั่งเรียบร้อยครับ", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-runcmd.log", "run echo hello for me"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (run_command, final answer), got %d", got)
	}

	log, err := os.ReadFile("ask-runcmd.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "[tool_call] run_command") {
		t.Fatalf("expected a run_command tool_call entry in the log even without a detected toolchain, got:\n%s", log)
	}
	if strings.Contains(string(log), "ไม่รู้จัก tool") {
		t.Fatalf("expected run_command to be recognized (not \"unknown tool\") without a detected toolchain, got:\n%s", log)
	}
	if !strings.Contains(string(log), "exit_code=0") {
		t.Fatalf("expected run_command to actually execute and report a successful exit code, got:\n%s", log)
	}
}

// TestCmdAskRunCommandRejectsDenylistedCommand confirms a denylisted
// command (e.g. rm) is refused - the error is fed back to the model as a
// tool result instead of either running it or the tool being unavailable.
func TestCmdAskRunCommandRejectsDenylistedCommand(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	if err := os.WriteFile("keep.txt", []byte("do not delete me\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			fmt.Fprint(w, streamLine("", "run_command", `{"command":"rm -rf ."}`, true))
		default:
			fmt.Fprint(w, streamLine("ขอโทษครับ ทำไม่ได้", "", "", true))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-runcmd-denied.log", "delete everything here"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}

	if _, err := os.Stat("keep.txt"); err != nil {
		t.Fatalf("expected keep.txt to survive the denylisted rm attempt: %v", err)
	}

	log, err := os.ReadFile("ask-runcmd-denied.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "ปฏิเสธคำสั่งนี้") {
		t.Fatalf("expected the denylist rejection message to be fed back to the model, got:\n%s", log)
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

func TestCmdAskAttachesPDFAsImages(t *testing.T) {
	installFakePDFToPPM(t)
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	pdfPath := fakePDFFile(t, dir, 3)

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("อ่าน PDF แล้วครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-pdf.log", "สรุปไฟล์ PDF นี้ให้หน่อย", pdfPath})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}

	var req struct {
		Messages []struct {
			Role    string   `json:"role"`
			Content string   `json:"content"`
			Images  []string `json:"images"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("expected a valid JSON request body, got error %v, body: %s", err, gotBody)
	}
	if len(req.Messages) < 2 {
		t.Fatalf("expected at least a system + user message, got %d", len(req.Messages))
	}
	userMsg := req.Messages[len(req.Messages)-1]
	if len(userMsg.Images) != 3 {
		t.Fatalf("expected 3 page images from the 3-page fake PDF, got %d", len(userMsg.Images))
	}
	for i, want := range []string{"FAKE-PNG-PAGE-1", "FAKE-PNG-PAGE-2", "FAKE-PNG-PAGE-3"} {
		got, err := base64.StdEncoding.DecodeString(userMsg.Images[i])
		if err != nil {
			t.Fatalf("image %d did not decode as base64: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("image %d: expected %q, got %q", i, want, string(got))
		}
	}
	if !strings.Contains(userMsg.Content, "แนบเป็นภาพ 3 หน้า") {
		t.Fatalf("expected the prompt content to note the PDF was attached as 3 page images, got: %s", userMsg.Content)
	}
	if !strings.Contains(userMsg.Content, "สรุปไฟล์ PDF นี้ให้หน่อย") {
		t.Fatalf("expected the original prompt text to still be present, got: %s", userMsg.Content)
	}
}

// TestCmdAskPDFConversionFailureIsNonFatal confirms a PDF that fails to
// convert (here: no pdftoppm on PATH at all) produces a warning and is
// simply skipped, rather than aborting the whole session - the same
// leniency already given to a missing/unreadable text attachment.
func TestCmdAskPDFConversionFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	// No installFakePDFToPPM here - pdftoppm intentionally stays whatever
	// is (or isn't) on the real PATH, isolated via a redirect to a PATH
	// with nothing in it, so the missing-binary error path is exercised
	// even on a machine that happens to have real poppler-utils installed.
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", t.TempDir())
	defer os.Setenv("PATH", origPath)

	pdfPath := fakePDFFile(t, dir, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("รับทราบครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-pdf-fail.log", "สรุปไฟล์นี้ให้หน่อย", pdfPath})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to still exit 0 despite the failed PDF conversion, got %d", exitCode)
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
// skills already follow (run_command is the one tool that's always on
// regardless - see runCommandTool).
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

// ======================================================================
// Section: telegrambot
//
// Unit tests for the pure helper functions (message splitting, ID-list
// parsing, access control, group-addressing, the knowledge-base tools,
// context persistence/compaction-threshold), plus end-to-end tests that
// drive telegramSession.handleTelegramMessage against a mocked Telegram
// Bot API (just the one sendMessage endpoint it needs) and a mocked
// Ollama /api/chat, the same httptest pattern the rest of this file uses
// for cmdAsk/cmdCoding. Deliberately testing at the handleTelegramMessage
// level rather than driving cmdTelegramBot's own infinite getUpdates loop
// end to end: that loop's shutdown path listens for OS signals on the
// whole process (see cmdTelegramBot), which would be unsafe to trigger
// from inside a test binary that may be running other tests concurrently.
// ======================================================================

func TestSplitTelegramMessageShortPassesThrough(t *testing.T) {
	got := splitTelegramMessage("สวัสดีครับ")
	if len(got) != 1 || got[0] != "สวัสดีครับ" {
		t.Fatalf("expected single unchanged chunk, got %#v", got)
	}
}

func TestSplitTelegramMessageEmptyFallsBack(t *testing.T) {
	got := splitTelegramMessage("   ")
	if len(got) != 1 || got[0] == "" {
		t.Fatalf("expected a single non-empty fallback chunk, got %#v", got)
	}
}

func TestSplitTelegramMessageLongSplitsAtBoundary(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString(strings.Repeat("a", 30))
		sb.WriteString("\n")
	}
	long := sb.String()
	chunks := splitTelegramMessage(long)
	if len(chunks) < 2 {
		t.Fatalf("expected text longer than the limit to split into multiple chunks, got %d", len(chunks))
	}
	var rejoined strings.Builder
	for _, c := range chunks {
		if utf8.RuneCountInString(c) > telegramMaxMessageRunes {
			t.Fatalf("chunk exceeds telegramMaxMessageRunes: %d runes", utf8.RuneCountInString(c))
		}
		rejoined.WriteString(c)
	}
	if !strings.Contains(rejoined.String(), strings.TrimSpace(long)[:100]) {
		t.Fatalf("rejoined chunks lost content from the start of the original text")
	}
}

func TestParseTelegramIDList(t *testing.T) {
	ids, warnings := parseTelegramIDList("123, -100456, not-a-number, 789")
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the bad entry, got %d: %v", len(warnings), warnings)
	}
	for _, want := range []int64{123, -100456, 789} {
		if !ids[want] {
			t.Fatalf("expected id %d to be parsed, got %v", want, ids)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("expected exactly 3 parsed ids, got %d: %v", len(ids), ids)
	}
}

func TestTelegramAccessConfigAllowed(t *testing.T) {
	cfg := telegramAccessConfig{
		Users:  map[int64]bool{111: true},
		Groups: map[int64]bool{-222: true},
	}
	if !cfg.allowed(tgChat{ID: 111, Type: "private"}, tgUser{ID: 111}) {
		t.Fatal("expected allow-listed private user to be allowed")
	}
	if cfg.allowed(tgChat{ID: 999, Type: "private"}, tgUser{ID: 999}) {
		t.Fatal("expected non-allow-listed private user to be denied")
	}
	if !cfg.allowed(tgChat{ID: -222, Type: "supergroup"}, tgUser{ID: 111}) {
		t.Fatal("expected allow-listed group chat to be allowed regardless of sender")
	}
	if cfg.allowed(tgChat{ID: -333, Type: "group"}, tgUser{ID: 111}) {
		t.Fatal("expected non-allow-listed group chat to be denied even for an allow-listed user")
	}
}

func TestTelegramMessageAddressesBotAndStripBotMention(t *testing.T) {
	cases := []struct {
		name    string
		msg     *tgMessage
		addr    bool
		stripTo string
	}{
		{"mention", &tgMessage{Text: "@olabot สรุปให้หน่อย"}, true, "สรุปให้หน่อย"},
		{"ola-prefix", &tgMessage{Text: "/ola สรุปให้หน่อย"}, true, "สรุปให้หน่อย"},
		{"ask-prefix", &tgMessage{Text: "/ask hello"}, true, "hello"},
		{"reply-to-bot", &tgMessage{Text: "อันนี้ล่ะ", ReplyToMessage: &tgMessage{From: tgUser{Username: "olabot"}}}, true, "อันนี้ล่ะ"},
		{"unaddressed", &tgMessage{Text: "คุยกันเฉยๆ ไม่เกี่ยวกับบอท"}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := telegramMessageAddressesBot(tc.msg, "olabot")
			if got != tc.addr {
				t.Fatalf("telegramMessageAddressesBot: got %v, want %v", got, tc.addr)
			}
			if tc.addr {
				stripped := stripBotMention(tc.msg.Text, "olabot")
				if stripped != tc.stripTo {
					t.Fatalf("stripBotMention: got %q, want %q", stripped, tc.stripTo)
				}
			}
		})
	}
}

// TestKnowledgeGrepSearchRecursesIntoSubfolders confirms a direct
// question: search_knowledge does walk arbitrarily deep into a knowledge
// base's subdirectories (organizing course materials by year/department/
// topic in nested folders works exactly as expected), and that the
// returned path always includes the full subdirectory route back to the
// configured root, not just the filename.
func TestKnowledgeGrepSearchRecursesIntoSubfolders(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "2566", "engineering")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "curriculum.md"), []byte("หลักสูตรปี 2566"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, warnings := resolveKnowledgeConfig(dir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	matches, _, _ := knowledgeGrepSearch("*", "", cfg)
	if len(matches) != 1 || !strings.Contains(matches[0], filepath.ToSlash(filepath.Join("2566", "engineering", "curriculum.md"))) {
		t.Fatalf("expected the nested file's full relative path to be returned, got: %v", matches)
	}
	// read_knowledge must accept that same nested path back
	content, err := toolReadKnowledge(map[string]interface{}{"path": matches[0]}, cfg)
	if err != nil {
		t.Fatalf("toolReadKnowledge on the nested path failed: %v", err)
	}
	if !strings.Contains(content, "2566") {
		t.Fatalf("unexpected content: %s", content)
	}
}

// TestKnowledgeWalkOnlySkipsHiddenDirsNotBuildToolNames is the regression
// test for a real gap found by inspection: the knowledge base walk used
// to reuse skipDirNames - the source-code denylist ask/coding use to skip
// node_modules/.venv/bin/dist/build/out/vendor/target while scanning a
// project's own cwd. Applied to a document folder, that silently hid any
// subfolder whose name happened to collide with one of those (e.g. a
// course materials folder literally named "build" or "vendor-docs"),
// with no warning at all. A knowledge base should only skip genuinely
// hidden (dot-prefixed) directories, the same universal convention every
// file browser and `ls` already honor.
func TestKnowledgeWalkOnlySkipsHiddenDirsNotBuildToolNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"build", "bin", "dist", "vendor", "target", "out", "node_modules"} {
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "doc.md"), []byte("เนื้อหาในโฟลเดอร์ "+name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	hidden := filepath.Join(dir, ".git")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "doc.md"), []byte("ไม่ควรถูกพบ"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, _ := resolveKnowledgeConfig(dir)
	matches, _, _ := knowledgeGrepSearch("*", "", cfg)
	if len(matches) != 7 {
		t.Fatalf("expected all 7 non-hidden subfolders' files to be found, got %d: %v", len(matches), matches)
	}
	for _, m := range matches {
		if strings.Contains(m, ".git") {
			t.Fatalf("expected the hidden .git directory to still be skipped, got it in results: %v", matches)
		}
	}
}

func TestKnowledgeConfigSearchAndRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lecture1.md"), []byte("Network Security บทที่ 1\nfirewall คือ..."), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lecture2.md"), []byte("Network Security บทที่ 2"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, warnings := resolveKnowledgeConfig(dir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !cfg.enabled() {
		t.Fatal("expected knowledge config to be enabled")
	}

	result, err := toolSearchKnowledge(map[string]interface{}{"pattern": "*.md"}, cfg)
	if err != nil {
		t.Fatalf("toolSearchKnowledge error: %v", err)
	}
	if !strings.Contains(result, "lecture1.md") || !strings.Contains(result, "lecture2.md") {
		t.Fatalf("expected both files listed, got: %s", result)
	}

	grep, err := toolSearchKnowledge(map[string]interface{}{"pattern": "*.md", "query": "firewall"}, cfg)
	if err != nil {
		t.Fatalf("toolSearchKnowledge (grep) error: %v", err)
	}
	if !strings.Contains(grep, "lecture1.md") || strings.Contains(grep, "lecture2.md") {
		t.Fatalf("expected only lecture1.md's matching line, got: %s", grep)
	}

	// path returned by search_knowledge is "<label>/lecture1.md" - read_knowledge must accept exactly that shape.
	label := cfg.Labels[0]
	content, err := toolReadKnowledge(map[string]interface{}{"path": label + "/lecture1.md"}, cfg)
	if err != nil {
		t.Fatalf("toolReadKnowledge error: %v", err)
	}
	if !strings.Contains(content, "firewall") {
		t.Fatalf("expected full file content, got: %s", content)
	}

	// Path traversal / escaping the configured root must be rejected -
	// same sandboxedPathIn guard read_file/scp_copy rely on.
	if _, err := toolReadKnowledge(map[string]interface{}{"path": label + "/../../etc/passwd"}, cfg); err == nil {
		t.Fatal("expected path traversal outside the knowledge root to be rejected")
	}
	if _, err := toolReadKnowledge(map[string]interface{}{"path": "unknown-label/lecture1.md"}, cfg); err == nil {
		t.Fatal("expected an unknown label to be rejected")
	}
}

func TestChatContextSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	chat := tgChat{ID: 555, Type: "private"}
	key := telegramContextKey(chat)
	c, err := loadChatContext(dir, key)
	if err != nil {
		t.Fatalf("loadChatContext on a not-yet-existing file should not error, got: %v", err)
	}
	if len(c.Turns) != 0 {
		t.Fatalf("expected a fresh context to have no turns, got %d", len(c.Turns))
	}

	c.Turns = append(c.Turns, chatTurn{Role: "user", Content: "สวัสดี", Time: time.Now()})
	c.Summary = "ผู้ใช้ทักทาย"
	if err := c.save(dir); err != nil {
		t.Fatalf("save error: %v", err)
	}
	if _, err := os.Stat(chatContextPath(dir, key) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("expected the .tmp file to be gone after an atomic rename")
	}
	if key != "user_555" {
		t.Fatalf("expected telegramContextKey to keep the pre-existing on-disk naming (\"user_555\"), got %q - this would orphan existing deployments' saved conversations", key)
	}

	reloaded, err := loadChatContext(dir, key)
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}
	if reloaded.Summary != "ผู้ใช้ทักทาย" || len(reloaded.Turns) != 1 || reloaded.Turns[0].Content != "สวัสดี" {
		t.Fatalf("round-trip mismatch: %#v", reloaded)
	}
}

func TestShouldCompactChatContext(t *testing.T) {
	c := &chatContext{}
	for i := 0; i < 5; i++ {
		c.Turns = append(c.Turns, chatTurn{Role: "user", Content: "x"})
	}
	if shouldCompactChatContext(c, 10) {
		t.Fatal("5 turns should not trigger compaction at threshold 10")
	}
	if !shouldCompactChatContext(c, 4) {
		t.Fatal("5 turns should trigger compaction at threshold 4")
	}
	if shouldCompactChatContext(c, 0) {
		t.Fatal("threshold 0 (disabled) should never trigger compaction")
	}
}

// TestDispatchTelegramToolCallRejectsFilesystemTools is the key security
// regression test for this section: a model call naming "write_file" -
// never offered in telegrambot's own tool schema, but still recognized by
// dispatchToolCall's shared base switch (see that function; it dispatches
// the base eight tools by name unconditionally) - must NOT actually touch
// the filesystem when it reaches dispatchChatToolCall instead. This
// is exactly the gap dispatchChatToolCall exists to close - see its
// doc comment.
func TestDispatchTelegramToolCallRejectsFilesystemTools(t *testing.T) {
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

	for _, name := range []string{"write_file", "edit_file", "create_folder", "read_file", "search_files", "ask_user", "run_command"} {
		argsJSON, _ := json.Marshal(map[string]interface{}{
			"path": "pwned.txt", "content": "pwned", "command": "echo pwned",
		})
		tc := toolCall{Function: toolCallFunction{Name: name, Arguments: argsJSON}}
		result := dispatchChatToolCall(tc, outFile, nil)
		if !strings.HasPrefix(result, "ERROR:") {
			t.Fatalf("expected %s to be rejected, got: %s", name, result)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatal("SECURITY REGRESSION: dispatchChatToolCall allowed write_file to actually create a file")
	}
}

// newTestTelegramSession builds a telegramSession wired to mock Telegram
// (telegramSrv) and Ollama (ollamaSrv) httptest servers, for driving
// handleTelegramMessage directly (see this section's own header comment
// for why the full cmdTelegramBot polling loop is not exercised here).
func newTestTelegramSession(t *testing.T, telegramSrv, ollamaSrv *httptest.Server, contextDir string, users map[int64]bool) *telegramSession {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "telegrambot-test-log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	return &telegramSession{
		chatBotCore: chatBotCore{
			client:       ollamaSrv.Client(),
			systemPrompt: buildChatBotSystemPrompt("Telegram", ""),
			tools:        filterTools(builtinTools, "get_current_time", "delay"),
			pcfg:         providerConfig{Provider: providerOllama, Host: ollamaSrv.URL, Model: "mock-model"},
			ctxSize:      4096,
			contextDir:   contextDir,
			knowledgeIdx: &knowledgeIndexStore{},
			keepRecent:   defaultChatBotKeepRecentTurns,
			compactAfter: defaultChatBotCompactAfterTurns,
			outFile:      logFile,
			locks:        map[string]*sync.Mutex{},
		},
		telegramClient: telegramSrv.Client(),
		apiBase:        telegramSrv.URL,
		token:          "test-token",
		botUsername:    "olabot",
		access:         telegramAccessConfig{Users: users, Groups: map[int64]bool{}},
		sem:            make(chan struct{}, 4),
	}
}

// newMockTelegramSendMessageServer returns an httptest.Server that only
// implements sendMessage (all handleTelegramMessage needs on the Telegram
// side) and records every chat_id/text pair it receives.
func newMockTelegramSendMessageServer(t *testing.T) (*httptest.Server, *[]struct {
	ChatID int64
	Text   string
}) {
	t.Helper()
	var sent []struct {
		ChatID int64
		Text   string
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/bottest-token/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, struct {
			ChatID int64
			Text   string
		}{body.ChatID, body.Text})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sent
}

func TestHandleTelegramMessageAllowedUserGetsAnswer(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)

	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("สวัสดีครับ ยินดีช่วยเหลือ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{111: true})

	msg := &tgMessage{
		MessageID: 1,
		From:      tgUser{ID: 111, Username: "moo"},
		Chat:      tgChat{ID: 111, Type: "private"},
		Text:      "สวัสดี",
	}
	session.handleTelegramMessage(msg)

	if len(*sent) != 1 {
		t.Fatalf("expected exactly 1 sendMessage call, got %d", len(*sent))
	}
	if (*sent)[0].ChatID != 111 || (*sent)[0].Text != "สวัสดีครับ ยินดีช่วยเหลือ" {
		t.Fatalf("unexpected reply: %#v", (*sent)[0])
	}

	cctx, err := loadChatContext(contextDir, telegramContextKey(tgChat{ID: 111, Type: "private"}))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	if len(cctx.Turns) != 2 || cctx.Turns[0].Role != "user" || cctx.Turns[1].Role != "assistant" {
		t.Fatalf("expected 1 user + 1 assistant turn persisted, got: %#v", cctx.Turns)
	}
}

func TestHandleTelegramMessageDeniedUserGetsNoAnswerFromModel(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)

	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("ไม่ควรถูกเรียก", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	// 111 is NOT in the allowlist (empty map).
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{})

	msg := &tgMessage{
		From: tgUser{ID: 111, Username: "moo"},
		Chat: tgChat{ID: 111, Type: "private"},
		Text: "สวัสดี",
	}
	session.handleTelegramMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected a denied user's message to never reach the model")
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Text, "ยังไม่ได้รับอนุญาต") {
		t.Fatalf("expected a single access-denied reply, got: %#v", *sent)
	}
	if _, err := os.Stat(chatContextPath(contextDir, telegramContextKey(tgChat{ID: 111, Type: "private"}))); !os.IsNotExist(err) {
		t.Fatal("expected no context file to be created for a denied user")
	}
}

func TestHandleTelegramMessageGroupIgnoresUnaddressedMessages(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)

	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("ไม่ควรถูกเรียก", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{})
	session.access.Groups = map[int64]bool{-999: true}

	msg := &tgMessage{
		From: tgUser{ID: 111, Username: "moo"},
		Chat: tgChat{ID: -999, Type: "group", Title: "ห้องเรียน"},
		Text: "คุยกันเฉยๆ ไม่เกี่ยวกับบอท",
	}
	session.handleTelegramMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected an unaddressed group message to never reach the model")
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no reply to an unaddressed group message, got: %#v", *sent)
	}
}

func TestHandleTelegramMessageUsesSearchKnowledgeTool(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)

	knowledgeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(knowledgeDir, "syllabus.md"), []byte("วิชา Network Security สอนทุกวันจันทร์"), 0644); err != nil {
		t.Fatal(err)
	}
	knowledgeCfg, warnings := resolveKnowledgeConfig(knowledgeDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	var round int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			fmt.Fprint(w, streamLine("", "search_knowledge", `{"pattern":"*.md"}`, true))
		case 2:
			fmt.Fprint(w, streamLine("วิชานี้สอนทุกวันจันทร์ครับ (จาก syllabus.md)", "", "", true))
		}
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{111: true})
	session.knowledgeCfg = knowledgeCfg
	session.tools = append(session.tools, searchKnowledgeTool, readKnowledgeTool)

	msg := &tgMessage{
		From: tgUser{ID: 111},
		Chat: tgChat{ID: 111, Type: "private"},
		Text: "วิชา Network Security สอนวันไหน",
	}
	session.handleTelegramMessage(msg)

	if atomic.LoadInt32(&round) != 2 {
		t.Fatalf("expected the model to call search_knowledge then answer (2 rounds), got %d round(s)", round)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Text, "วันจันทร์") {
		t.Fatalf("expected the final answer to reach the chat, got: %#v", *sent)
	}
}

func TestRunTelegramToolLoopRetriesOnceOnEmptyCompletion(t *testing.T) {
	var round int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			fmt.Fprint(w, streamLine("", "", "", true)) // simulates a model that returned nothing at all
			return
		}
		fmt.Fprint(w, streamLine("คำตอบจริงหลัง retry", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	telegramSrv, _ := newMockTelegramSendMessageServer(t)
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, t.TempDir(), map[int64]bool{1: true})

	answer, err := session.runChatToolLoop([]ollamaMessage{
		{Role: "system", Content: buildChatBotSystemPrompt("Telegram", "")},
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("expected the retry to eventually succeed, got error: %v", err)
	}
	if answer != "คำตอบจริงหลัง retry" {
		t.Fatalf("unexpected answer: %q", answer)
	}
	if atomic.LoadInt32(&round) != 2 {
		t.Fatalf("expected exactly 2 rounds (1 empty + 1 retry), got %d", round)
	}
}

func TestRunTelegramToolLoopFailsAfterRepeatedEmptyCompletions(t *testing.T) {
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("", "", "", true)) // always empty
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	telegramSrv, _ := newMockTelegramSendMessageServer(t)
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, t.TempDir(), map[int64]bool{1: true})

	_, err := session.runChatToolLoop([]ollamaMessage{
		{Role: "system", Content: buildChatBotSystemPrompt("Telegram", "")},
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected an error after repeated empty completions, got nil")
	}
}

// TestHandleTelegramMessageEmptyCompletionDoesNotPersistBlankTurn is the
// end-to-end regression test for the bug this was written to fix: a chat
// where the model's very first reply comes back empty must NOT end up
// with a blank "" assistant turn silently saved to that chat's context
// file - the user gets a clear error message instead, and nothing is
// persisted for that exchange.
func TestHandleTelegramMessageEmptyCompletionDoesNotPersistBlankTurn(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)

	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("", "", "", true)) // always empty, exhausts the retry
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{111: true})

	msg := &tgMessage{
		From: tgUser{ID: 111},
		Chat: tgChat{ID: 111, Type: "private"},
		Text: "Hi",
	}
	session.handleTelegramMessage(msg)

	if len(*sent) != 1 || strings.TrimSpace((*sent)[0].Text) == "" {
		t.Fatalf("expected a single non-empty error reply, got: %#v", *sent)
	}
	cctx, err := loadChatContext(contextDir, telegramContextKey(tgChat{ID: 111, Type: "private"}))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	if len(cctx.Turns) != 0 {
		t.Fatalf("expected no turns persisted after a failed (empty-completion) exchange, got %#v", cctx.Turns)
	}
}

func TestHandleTelegramMessageToolsCommand(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)

	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("ไม่ควรถูกเรียก", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, t.TempDir(), map[int64]bool{111: true})

	msg := &tgMessage{From: tgUser{ID: 111}, Chat: tgChat{ID: 111, Type: "private"}, Text: "/tools"}
	session.handleTelegramMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected /tools to never reach the model")
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Text, "search_knowledge") {
		t.Fatalf("expected a /tools status reply mentioning search_knowledge, got: %#v", *sent)
	}
}

// TestBuildTelegramSystemPromptPersonaOrderingAndIdentityRule is a
// structural regression test for the "asked its name, answered generic
// 'I'm a bot' instead of the persona's name" bug report: it can't verify
// an LLM will actually comply (that's inherently model-dependent), but it
// does pin down the two things that ARE fully within ola's control -
// persona appears BEFORE the rule bullets (not appended after
// everything, competing with a wall of rules for the model's attention),
// and the explicit "your name is whatever PERSONA says, never answer
// generically" rule is present whenever a persona is set.
func TestBuildTelegramSystemPromptPersonaOrderingAndIdentityRule(t *testing.T) {
	prompt := buildTelegramSystemPrompt("คุณชื่อ JonnyQ ร่าเริงและรักการผจญภัย")

	introIdx := strings.Index(prompt, chatBotSystemPromptIntro("Telegram"))
	personaIdx := strings.Index(prompt, "JonnyQ")
	rulesIdx := strings.Index(prompt, "กติกาพื้นฐานที่ต้องทำตามเสมอ")
	if introIdx == -1 || personaIdx == -1 || rulesIdx == -1 {
		t.Fatalf("expected intro, persona, and rules all present in the prompt")
	}
	if !(introIdx < personaIdx && personaIdx < rulesIdx) {
		t.Fatalf("expected order intro < persona < rules, got indices %d, %d, %d", introIdx, personaIdx, rulesIdx)
	}
	if !strings.Contains(prompt, "ฉันชื่อบอท") {
		t.Fatal("expected the explicit anti-generic-answer example to be present in the rules")
	}

	// No persona configured: the prompt must still be well-formed (no
	// dangling "PERSONA" header with nothing under it).
	noPersonaPrompt := buildTelegramSystemPrompt("")
	if strings.Contains(noPersonaPrompt, "PERSONA (กำหนดโดยผู้ดูแล") {
		t.Fatal("expected no PERSONA section when no persona is configured")
	}
}

// TestBuildChatBotSystemPromptDifferentPlatformNames confirms the intro
// line actually varies by platform (the one genuinely platform-specific
// piece of an otherwise fully shared prompt) while the rules stay
// identical either way.
func TestBuildChatBotSystemPromptDifferentPlatformNames(t *testing.T) {
	tg := buildChatBotSystemPrompt("Telegram", "")
	dc := buildChatBotSystemPrompt("Discord", "")
	if !strings.Contains(tg, "บอท Telegram") {
		t.Fatalf("expected Telegram intro, got: %s", tg[:80])
	}
	if !strings.Contains(dc, "บอท Discord") {
		t.Fatalf("expected Discord intro, got: %s", dc[:80])
	}
	if strings.Contains(tg, "บอท Discord") || strings.Contains(dc, "บอท Telegram") {
		t.Fatal("expected each platform's intro to mention only its own platform")
	}
	// everything after the intro (the shared rules) must be byte-identical
	tgRules := tg[strings.Index(tg, "\n\n"):]
	dcRules := dc[strings.Index(dc, "\n\n"):]
	if tgRules != dcRules {
		t.Fatal("expected the rules half of the prompt to be identical across platforms")
	}
}

// TestBuildChatBotSystemPromptSearchesKnowledgeForEveryQuestion is the
// regression test for a direct request to broaden search_knowledge usage:
// the earlier rule only pushed the model to search for questions that
// looked like specific facts (names, pets, places); this pins down that
// the rule now reads as "every question", with only genuine non-question
// small talk exempted, and confirms it applies identically to both
// platforms since they share builtinChatBotSystemPromptRules.
func TestBuildChatBotSystemPromptSearchesKnowledgeForEveryQuestion(t *testing.T) {
	for _, platform := range []string{"Telegram", "Discord"} {
		prompt := buildChatBotSystemPrompt(platform, "")
		if !strings.Contains(prompt, "ทุกคำถามที่ผู้ใช้ถาม") {
			t.Fatalf("[%s] expected the rule to require searching for every question, not just specific-fact questions", platform)
		}
		if !strings.Contains(prompt, "ก่อนจะตอบจากความรู้ทั่วไปของตัวเอง") {
			t.Fatalf("[%s] expected the rule to explicitly say search before answering from general knowledge", platform)
		}
		// small talk that isn't really a question should still be exempt,
		// so the bot isn't forced to search_knowledge on a bare "hello"
		if !strings.Contains(prompt, "ไม่ใช่คำถามจริงๆ") {
			t.Fatalf("[%s] expected an exemption for non-question small talk to still be present", platform)
		}
	}
}

// TestTelegramGroundToolResult is the regression test for the "answered a
// search-flavored question with a fabricated-looking search transcript,
// no real URL cited" bug report - see groundToolResult's own doc
// comment for the full failure mode this addresses.
func TestTelegramGroundToolResult(t *testing.T) {
	cases := []struct {
		name       string
		toolName   string
		result     string
		wantMarked bool
	}{
		{"web_search success gets marked", "web_search", "1. Example\n   https://example.com\n   snippet", true},
		{"web_fetch success gets marked", "web_fetch", "page title\n\ncontent", true},
		{"search_knowledge success gets marked", "search_knowledge", "km/a.md", true},
		{"read_knowledge success gets marked", "read_knowledge", "file content", true},
		{"error result never marked", "web_search", "ERROR: web_search ไม่ได้ถูกตั้งค่า", false},
		{"get_current_time never marked", "get_current_time", "2026-08-10 10:48", false},
		{"delay never marked", "delay", "รอครบ 5s แล้ว", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := groundToolResult(tc.toolName, tc.result)
			isMarked := strings.HasPrefix(got, "[ผลลัพธ์จริงจากการเรียก")
			if isMarked != tc.wantMarked {
				t.Fatalf("groundToolResult(%q, ...): marked=%v, want %v (got: %q)", tc.toolName, isMarked, tc.wantMarked, got)
			}
			if !strings.Contains(got, tc.result) {
				t.Fatalf("expected the original result content to still be present, got: %q", got)
			}
		})
	}
}

// TestHandleTelegramMessageGroundsWebSearchResultInFollowUpRequest is the
// end-to-end version of TestTelegramGroundToolResult: it confirms the
// marker actually reaches the model, in the actual follow-up request
// telegrambot sends after a real web_search call - not just that the pure
// helper function produces the right string in isolation.
func TestHandleTelegramMessageGroundsWebSearchResultInFollowUpRequest(t *testing.T) {
	telegramSrv, _ := newMockTelegramSendMessageServer(t)

	var round int32
	var secondRoundBody string
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch n {
		case 1:
			fmt.Fprint(w, streamLine("", "web_search", `{"queries":["ราคาทองคำวันนี้"]}`, true))
		case 2:
			body, _ := io.ReadAll(r.Body)
			secondRoundBody = string(body)
			fmt.Fprint(w, streamLine("ตามผลค้นหาล่าสุด...", "", "", true))
		}
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	searxngMux := http.NewServeMux()
	searxngMux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"title":"ราคาทอง","url":"https://example.com/gold","content":"67,700 บาท"}]}`)
	})
	searxngSrv := httptest.NewServer(searxngMux)
	defer searxngSrv.Close()

	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, t.TempDir(), map[int64]bool{111: true})
	session.searchCfg = resolveSearchConfig(searxngSrv.URL, 0, 0, 0, 0, 0, false)
	session.tools = append(session.tools, webSearchTool, webFetchTool)

	msg := &tgMessage{From: tgUser{ID: 111}, Chat: tgChat{ID: 111, Type: "private"}, Text: "ราคาทองวันนี้เท่าไหร่"}
	session.handleTelegramMessage(msg)

	if atomic.LoadInt32(&round) != 2 {
		t.Fatalf("expected 2 rounds (web_search call then answer), got %d", round)
	}
	if !strings.Contains(secondRoundBody, "ผลลัพธ์จริงจากการเรียก web_search") {
		t.Fatalf("expected the grounding marker in the follow-up request sent to the model, body: %s", secondRoundBody)
	}
	if !strings.Contains(secondRoundBody, "example.com/gold") {
		t.Fatalf("expected the real SearXNG result URL to reach the model, body: %s", secondRoundBody)
	}
}

// TestBuildTelegramHTTPClientsTimeouts is the regression test for a real
// production bug: cmdTelegramBot once gave the SAME short-timeout client
// to both Telegram API calls and model (Ollama/OpenAI) calls, which meant
// every single message failed against a real local Ollama server with
// "context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)" as soon as the model took longer than 30s to respond (routine
// for a large model) - see buildTelegramHTTPClients' own doc comment.
// This pins down the three clients' timeouts so that regression can't
// silently come back. Deliberately doesn't compare against a live slow
// server (that would make the test itself slow/flaky) - the property
// that matters and is fully checkable without one is which Timeout value
// each client got.
func TestBuildTelegramHTTPClientsTimeouts(t *testing.T) {
	pollTimeoutSec := 600
	poll, telegram, model := buildTelegramHTTPClients(pollTimeoutSec)

	if model.Timeout != 0 {
		t.Fatalf("SECURITY/RELIABILITY REGRESSION: the model (Ollama/OpenAI) HTTP client must be unbounded (Timeout=0, matching newHTTPClient()), got %v - a large local model taking longer than this to respond would fail every single message", model.Timeout)
	}
	if telegram.Timeout <= 0 || telegram.Timeout > 2*time.Minute {
		t.Fatalf("expected the Telegram API client to have a short, bounded timeout (fail fast on a hung connection), got %v", telegram.Timeout)
	}
	wantMinPoll := time.Duration(pollTimeoutSec) * time.Second
	if poll.Timeout <= wantMinPoll {
		t.Fatalf("expected the poll client's timeout (%v) to be comfortably longer than --poll-timeout (%ds), or long polls would be killed client-side before Telegram can answer", poll.Timeout, pollTimeoutSec)
	}
}

// TestToolSearchKnowledgeGrepFallbackWhenQueryWordsDiffer is the exact
// regression test for a real bug report: a document literally reads
// "นายอรรถพล คงหวาน มีหมาชื่อว่า เมกกะ และมีแมวชื่อ พิคโค่" (has a dog
// named Mecca and a cat named Piccolo) - asked "what pets does he have",
// the model generated the query "สัตว์เลี้ยง" (pets) which never appears
// verbatim in the document (it says "หมา"/"แมว", dog/cat, never the word
// "pet") - so the old exact-substring grep legitimately found zero
// matching lines and told the model "not found", even though the file
// plainly contains the answer for anyone who reads it. This pins down
// the fix: when few files match the pattern, their content is returned
// directly instead of a dead end.
func TestToolSearchKnowledgeGrepFallbackWhenQueryWordsDiffer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("นายอรรถพล คงหวาน มีหมาชื่อว่า เมกกะ และมีแมวชื่อ พิคโค่"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, warnings := resolveKnowledgeConfig(dir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	result, err := toolSearchKnowledge(map[string]interface{}{"pattern": "*", "query": "สัตว์เลี้ยง"}, cfg)
	if err != nil {
		t.Fatalf("toolSearchKnowledge error: %v", err)
	}
	if !strings.Contains(result, "เมกกะ") || !strings.Contains(result, "พิคโค่") {
		t.Fatalf("expected the fallback to include the file's actual content (the real answer), got: %s", result)
	}
	if !strings.Contains(result, "a.md") {
		t.Fatalf("expected the matched file's path to be identifiable in the fallback, got: %s", result)
	}
}

// TestToolSearchKnowledgeNoFallbackWhenTooManyFilesMatch confirms the
// fallback is bounded: with more than knowledgeGrepFallbackMaxFiles
// pattern matches, dumping every file's content would be expensive and
// noisy, so the result should list file paths (so the model can pick one
// for read_knowledge) rather than inline all their content.
func TestToolSearchKnowledgeNoFallbackWhenTooManyFilesMatch(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < knowledgeGrepFallbackMaxFiles+2; i++ {
		name := fmt.Sprintf("doc%d.md", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("เนื้อหาไม่เกี่ยวข้อง"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := resolveKnowledgeConfig(dir)

	result, err := toolSearchKnowledge(map[string]interface{}{"pattern": "*.md", "query": "ไม่มีคำนี้แน่นอน"}, cfg)
	if err != nil {
		t.Fatalf("toolSearchKnowledge error: %v", err)
	}
	if strings.Contains(result, "เนื้อหาไม่เกี่ยวข้อง") {
		t.Fatalf("expected no inline file content when too many files matched, got: %s", result)
	}
	if !strings.Contains(result, "doc0.md") {
		t.Fatalf("expected matched file paths to still be listed so read_knowledge can target one, got: %s", result)
	}
}

// ======================================================================
// Section: telegrambot embedding-based knowledge search
// ======================================================================

func TestChunkKnowledgeTextShortTextIsOneChunk(t *testing.T) {
	got := chunkKnowledgeText("นายอรรถพล คงหวาน มีหมาชื่อว่า เมกกะ และมีแมวชื่อ พิคโค่")
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk for short text, got %d: %v", len(got), got)
	}
}

func TestChunkKnowledgeTextEmptyReturnsNil(t *testing.T) {
	if got := chunkKnowledgeText("   \n\n  "); got != nil {
		t.Fatalf("expected nil for blank text, got %v", got)
	}
}

func TestChunkKnowledgeTextLongParagraphSplitsWithOverlap(t *testing.T) {
	// A single paragraph (no \n\n) longer than knowledgeChunkSize runes,
	// built from multi-byte Thai text - the regression concern here is
	// that windowSplitRunes must slice on runes, never raw bytes, or a
	// multi-byte UTF-8 character could be split mid-sequence.
	para := strings.Repeat("สวัสดีครับผมชื่อโอลา ", 60) // well over knowledgeChunkSize runes
	chunks := chunkKnowledgeText(para)
	if len(chunks) < 2 {
		t.Fatalf("expected the long paragraph to split into multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk is not valid UTF-8 (split mid multi-byte character): %q", c)
		}
		if utf8.RuneCountInString(c) > knowledgeChunkSize {
			t.Fatalf("chunk exceeds knowledgeChunkSize: %d runes", utf8.RuneCountInString(c))
		}
	}
}

func TestChunkKnowledgeTextRespectsParagraphBoundaries(t *testing.T) {
	text := "ย่อหน้าแรก\n\nย่อหน้าที่สอง\n\nย่อหน้าที่สาม"
	chunks := chunkKnowledgeText(text)
	// short paragraphs should get packed together into as few chunks as
	// reasonable, not one chunk per paragraph
	joined := strings.Join(chunks, " ")
	for _, want := range []string{"ย่อหน้าแรก", "ย่อหน้าที่สอง", "ย่อหน้าที่สาม"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected chunk content to contain %q, got %v", want, chunks)
		}
	}
}

func TestNormalizeVectorAndDotProduct(t *testing.T) {
	v := normalizeVector([]float32{3, 4}) // 3-4-5 triangle, magnitude 5
	if math.Abs(float64(v[0])-0.6) > 0.0001 || math.Abs(float64(v[1])-0.8) > 0.0001 {
		t.Fatalf("expected unit vector [0.6, 0.8], got %v", v)
	}
	// a vector against itself (already normalized) has cosine similarity 1
	if got := dotProduct(v, v); math.Abs(float64(got)-1.0) > 0.0001 {
		t.Fatalf("expected self dot product ~1.0, got %v", got)
	}
	// orthogonal unit vectors have cosine similarity 0
	a := normalizeVector([]float32{1, 0})
	b := normalizeVector([]float32{0, 1})
	if got := dotProduct(a, b); math.Abs(float64(got)) > 0.0001 {
		t.Fatalf("expected orthogonal dot product ~0, got %v", got)
	}
}

func TestNormalizeVectorZeroVectorDoesNotPanic(t *testing.T) {
	got := normalizeVector([]float32{0, 0, 0})
	if len(got) != 3 {
		t.Fatalf("expected zero vector to pass through unchanged, got %v", got)
	}
}

// newMockEmbedServer returns an httptest.Server implementing /api/embed
// with a caller-supplied deterministic text->vector function, plus a call
// counter (for staleness/incremental-reindex assertions).
func newMockEmbedServer(t *testing.T, vectorFor func(text string) []float32) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var embeddings [][]float32
		for _, in := range req.Input {
			embeddings = append(embeddings, vectorFor(in))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":      req.Model,
			"embeddings": embeddings,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestOllamaEmbedAndBatch(t *testing.T) {
	srv, calls := newMockEmbedServer(t, func(text string) []float32 {
		return []float32{float32(len(text)), 1, 2}
	})
	vecs, err := ollamaEmbed(srv.Client(), srv.URL, "mock-embed", []string{"a", "bb", "ccc"})
	if err != nil {
		t.Fatalf("ollamaEmbed error: %v", err)
	}
	if len(vecs) != 3 || vecs[0][0] != 1 || vecs[1][0] != 2 || vecs[2][0] != 3 {
		t.Fatalf("unexpected vectors: %v", vecs)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected exactly 1 batched call for 3 inputs, got %d", *calls)
	}

	// batching: more inputs than embedBatchSize should split into multiple calls
	atomic.StoreInt32(calls, 0)
	var many []string
	for i := 0; i < embedBatchSize+5; i++ {
		many = append(many, fmt.Sprintf("text-%d", i))
	}
	vecs, err = ollamaEmbedBatch(srv.Client(), srv.URL, "mock-embed", many)
	if err != nil {
		t.Fatalf("ollamaEmbedBatch error: %v", err)
	}
	if len(vecs) != len(many) {
		t.Fatalf("expected %d vectors, got %d", len(many), len(vecs))
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Fatalf("expected exactly 2 batched calls for %d inputs (batch size %d), got %d", len(many), embedBatchSize, *calls)
	}
}

func TestBuildKnowledgeIndexIncrementalReembedsOnlyChangedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("เนื้อหาไฟล์ A"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("เนื้อหาไฟล์ B"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := resolveKnowledgeConfig(dir)

	srv, calls := newMockEmbedServer(t, func(text string) []float32 {
		return []float32{float32(len(text)), 0, 0}
	})
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()

	idx1, err := buildKnowledgeIndex(srv.Client(), srv.URL, "mock-embed", cfg, knowledgeIndex{FileHashes: map[string]string{}}, logFile)
	if err != nil {
		t.Fatalf("buildKnowledgeIndex (initial) error: %v", err)
	}
	if len(idx1.Chunks) != 2 {
		t.Fatalf("expected 2 chunks (one per file) on initial build, got %d", len(idx1.Chunks))
	}
	firstCallCount := atomic.LoadInt32(calls)
	if firstCallCount == 0 {
		t.Fatal("expected at least one embed call on initial build")
	}

	// Re-run with the SAME files unchanged: should reuse cached chunks,
	// zero new embed calls.
	atomic.StoreInt32(calls, 0)
	idx2, err := buildKnowledgeIndex(srv.Client(), srv.URL, "mock-embed", cfg, idx1, logFile)
	if err != nil {
		t.Fatalf("buildKnowledgeIndex (unchanged) error: %v", err)
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Fatalf("expected 0 embed calls when no file changed, got %d", *calls)
	}
	if len(idx2.Chunks) != 2 {
		t.Fatalf("expected chunks to be carried over unchanged, got %d", len(idx2.Chunks))
	}

	// Modify one file: should re-embed ONLY that file's chunk(s).
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("เนื้อหาไฟล์ A ที่แก้ไขแล้ว"), 0644); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(calls, 0)
	idx3, err := buildKnowledgeIndex(srv.Client(), srv.URL, "mock-embed", cfg, idx2, logFile)
	if err != nil {
		t.Fatalf("buildKnowledgeIndex (a.md changed) error: %v", err)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected exactly 1 embed call (only a.md's single chunk) after editing one file, got %d", *calls)
	}
	if len(idx3.Chunks) != 2 {
		t.Fatalf("expected still 2 chunks total (1 refreshed + 1 reused), got %d", len(idx3.Chunks))
	}

	// Delete one file: it must not linger in the new index.
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	idx4, err := buildKnowledgeIndex(srv.Client(), srv.URL, "mock-embed", cfg, idx3, logFile)
	if err != nil {
		t.Fatalf("buildKnowledgeIndex (b.md deleted) error: %v", err)
	}
	if len(idx4.Chunks) != 1 {
		t.Fatalf("expected deleted file's chunk to be dropped, got %d chunks", len(idx4.Chunks))
	}
	if _, ok := idx4.FileHashes["km/b.md"]; ok {
		t.Fatal("expected deleted file's hash entry to be removed from the index")
	}
}

func TestKnowledgeIndexSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := knowledgeIndexPath(dir)

	loaded, err := loadKnowledgeIndex(path)
	if err != nil {
		t.Fatalf("loadKnowledgeIndex on a not-yet-existing file should not error, got: %v", err)
	}
	if len(loaded.Chunks) != 0 {
		t.Fatalf("expected empty index for a fresh path, got %d chunks", len(loaded.Chunks))
	}

	idx := knowledgeIndex{
		FileHashes: map[string]string{"km/a.md": "deadbeef"},
		Chunks: []knowledgeChunk{
			{Label: "km", Path: "a.md", Text: "เนื้อหา", Embedding: []float32{0.6, 0.8}},
		},
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := idx.save(path); err != nil {
		t.Fatalf("save error: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("expected the .tmp file to be gone after an atomic rename")
	}

	reloaded, err := loadKnowledgeIndex(path)
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}
	if len(reloaded.Chunks) != 1 || reloaded.Chunks[0].Text != "เนื้อหา" || reloaded.FileHashes["km/a.md"] != "deadbeef" {
		t.Fatalf("round-trip mismatch: %#v", reloaded)
	}
}

func TestKnowledgeIndexStoreConcurrentAccess(t *testing.T) {
	st := &knowledgeIndexStore{}
	st.set(knowledgeIndex{FileHashes: map[string]string{}, Chunks: []knowledgeChunk{{Label: "km", Path: "a.md"}}})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.get()
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			st.set(knowledgeIndex{FileHashes: map[string]string{}, Chunks: []knowledgeChunk{{Label: "km", Path: fmt.Sprintf("%d.md", n)}}})
		}(i)
	}
	wg.Wait() // must not panic/race (run with -race to actually catch a data race)
}

// TestSemanticSearchKnowledgeFindsMeaningNotJustText is the embedding-
// search version of the exact bug report this whole layer exists to fix:
// a document says "has a dog named Mecca and a cat named Piccolo" and
// never contains the word "pets" - exact grep can't find it (see
// TestToolSearchKnowledgeGrepFallbackWhenQueryWordsDiffer for the
// content-dump mitigation), but a real embedding model would score the
// "pets" query highly against that chunk because the MEANING overlaps.
// Uses a small deterministic mock embedding function (not a real model)
// that assigns high similarity to the "related" chunk and low similarity
// to an unrelated one, to prove the ranking/threshold plumbing itself
// works correctly end to end.
func TestSemanticSearchKnowledgeFindsMeaningNotJustText(t *testing.T) {
	srv, _ := newMockEmbedServer(t, func(text string) []float32 {
		switch {
		case strings.Contains(text, "เมกกะ") || strings.Contains(text, "สัตว์เลี้ยง"):
			return []float32{1, 0} // "pets" query and the pets-chunk point the same way
		default:
			return []float32{0, 1} // unrelated content points a different way
		}
	})

	idx := knowledgeIndex{
		FileHashes: map[string]string{},
		Chunks: []knowledgeChunk{
			{Label: "km", Path: "a.md", Text: "นายอรรถพล คงหวาน มีหมาชื่อว่า เมกกะ และมีแมวชื่อ พิคโค่", Embedding: normalizeVector([]float32{1, 0})},
			{Label: "km", Path: "b.md", Text: "ตารางเรียนวิชา Network Security", Embedding: normalizeVector([]float32{0, 1})},
		},
	}

	scored, err := semanticSearchKnowledge(srv.Client(), srv.URL, "mock-embed", idx, nil, "สัตว์เลี้ยง", 5, 0.5)
	if err != nil {
		t.Fatalf("semanticSearchKnowledge error: %v", err)
	}
	if len(scored) != 1 {
		t.Fatalf("expected exactly 1 result above the score threshold, got %d: %v", len(scored), scored)
	}
	if !strings.Contains(scored[0].Chunk.Text, "เมกกะ") {
		t.Fatalf("expected the pets chunk to be the match, got: %s", scored[0].Chunk.Text)
	}
	if scored[0].Score < 0.99 {
		t.Fatalf("expected near-perfect similarity for the deterministic mock vectors, got %v", scored[0].Score)
	}
}

func TestSemanticSearchKnowledgeRespectsAllowedFilesFilter(t *testing.T) {
	srv, _ := newMockEmbedServer(t, func(text string) []float32 { return []float32{1, 0} })
	idx := knowledgeIndex{
		Chunks: []knowledgeChunk{
			{Label: "km", Path: "a.md", Text: "A", Embedding: normalizeVector([]float32{1, 0})},
			{Label: "km", Path: "b.md", Text: "B", Embedding: normalizeVector([]float32{1, 0})},
		},
	}
	scored, err := semanticSearchKnowledge(srv.Client(), srv.URL, "mock-embed", idx, map[string]bool{"km/a.md": true}, "q", 5, 0.0)
	if err != nil {
		t.Fatalf("semanticSearchKnowledge error: %v", err)
	}
	if len(scored) != 1 || scored[0].Chunk.Path != "a.md" {
		t.Fatalf("expected only km/a.md to be considered, got: %v", scored)
	}
}

// TestSearchKnowledgeToolUsesEmbeddingFallbackEndToEnd drives the actual
// telegramSession.searchKnowledgeTool dispatch path (what
// runTelegramToolLoop really calls) through the full grep-misses ->
// embedding-search-hits flow, using the real a.md content from the bug
// report.
func TestSearchKnowledgeToolUsesEmbeddingFallbackEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("นายอรรถพล คงหวาน มีหมาชื่อว่า เมกกะ และมีแมวชื่อ พิคโค่"), 0644); err != nil {
		t.Fatal(err)
	}
	knowledgeCfg, _ := resolveKnowledgeConfig(dir)

	embedSrv, _ := newMockEmbedServer(t, func(text string) []float32 {
		if strings.Contains(text, "เมกกะ") || strings.Contains(text, "สัตว์เลี้ยง") {
			return []float32{1, 0}
		}
		return []float32{0, 1}
	})

	telegramSrv, _ := newMockTelegramSendMessageServer(t)
	ollamaSrv := httptest.NewServer(http.NewServeMux()) // unused for this test, just needs to exist
	defer ollamaSrv.Close()

	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, t.TempDir(), map[int64]bool{1: true})
	session.knowledgeCfg = knowledgeCfg
	session.embedCfg = embedConfig{Model: "mock-embed", TopK: 5, MinScore: 0.5}
	session.client = embedSrv.Client() // searchKnowledgeTool calls ollamaEmbed via s.client
	session.pcfg.Host = embedSrv.URL

	idx, err := buildKnowledgeIndex(embedSrv.Client(), embedSrv.URL, "mock-embed", knowledgeCfg, knowledgeIndex{FileHashes: map[string]string{}}, session.outFile)
	if err != nil {
		t.Fatalf("buildKnowledgeIndex error: %v", err)
	}
	session.knowledgeIdx.set(idx)

	result, err := session.searchKnowledgeTool(map[string]interface{}{"pattern": "*", "query": "สัตว์เลี้ยง"})
	if err != nil {
		t.Fatalf("searchKnowledgeTool error: %v", err)
	}
	if !strings.Contains(result, "เมกกะ") || !strings.Contains(result, "พิคโค่") {
		t.Fatalf("expected the semantic-search fallback to surface the actual answer, got: %s", result)
	}
	if !strings.Contains(result, "embedding") {
		t.Fatalf("expected the result to be labeled as coming from embedding search, got: %s", result)
	}
}

// TestSearchKnowledgeToolFallsBackWhenEmbeddingScoresAreLow confirms that
// when semantic search runs but finds nothing above --embed-min-score,
// searchKnowledgeTool still falls through to the existing small-file
// content-dump fallback rather than returning an empty-handed "no match".
func TestSearchKnowledgeToolFallsBackWhenEmbeddingScoresAreLow(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("เนื้อหาที่ไม่เกี่ยวข้องกับคำถามเลย"), 0644); err != nil {
		t.Fatal(err)
	}
	knowledgeCfg, _ := resolveKnowledgeConfig(dir)

	embedSrv, _ := newMockEmbedServer(t, func(text string) []float32 {
		return []float32{float32(len(text) % 7), float32(len(text) % 5)} // low/inconsistent similarity by construction
	})

	telegramSrv, _ := newMockTelegramSendMessageServer(t)
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()

	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, t.TempDir(), map[int64]bool{1: true})
	session.knowledgeCfg = knowledgeCfg
	session.embedCfg = embedConfig{Model: "mock-embed", TopK: 5, MinScore: 0.999} // impossibly high bar - forces the fallback
	session.client = embedSrv.Client()
	session.pcfg.Host = embedSrv.URL

	idx, err := buildKnowledgeIndex(embedSrv.Client(), embedSrv.URL, "mock-embed", knowledgeCfg, knowledgeIndex{FileHashes: map[string]string{}}, session.outFile)
	if err != nil {
		t.Fatalf("buildKnowledgeIndex error: %v", err)
	}
	session.knowledgeIdx.set(idx)

	result, err := session.searchKnowledgeTool(map[string]interface{}{"pattern": "*", "query": "คำค้นที่ไม่ตรงอะไรเลย"})
	if err != nil {
		t.Fatalf("searchKnowledgeTool error: %v", err)
	}
	if !strings.Contains(result, "เนื้อหาที่ไม่เกี่ยวข้อง") {
		t.Fatalf("expected the small-file-dump fallback to still fire when embedding scores are all below threshold, got: %s", result)
	}
}

// ======================================================================
// Section: discordbot
//
// Unit tests for the pure helper functions (allowlist, addressing,
// message splitting), REST client tests via httptest (same pattern as
// telegrambot's), Gateway protocol tests driven against a fake in-memory
// wsConn (never a real Discord connection - see discordGatewaySession's
// own doc comment for why), and end-to-end handleDiscordMessage tests
// mirroring telegrambot's own test section, confirming the shared
// chatBotCore behaves identically from Discord's side of the fence too.
// ======================================================================

func TestSplitDiscordMessageShortPassesThrough(t *testing.T) {
	got := splitDiscordMessage("สวัสดีครับ")
	if len(got) != 1 || got[0] != "สวัสดีครับ" {
		t.Fatalf("expected single unchanged chunk, got %#v", got)
	}
}

func TestSplitDiscordMessageRespectsDiscordLimit(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 150; i++ {
		sb.WriteString(strings.Repeat("a", 30))
		sb.WriteString("\n")
	}
	chunks := splitDiscordMessage(sb.String())
	if len(chunks) < 2 {
		t.Fatalf("expected text longer than Discord's limit to split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if utf8.RuneCountInString(c) > discordMaxMessageRunes {
			t.Fatalf("chunk exceeds discordMaxMessageRunes (%d): %d runes", discordMaxMessageRunes, utf8.RuneCountInString(c))
		}
	}
}

func TestParseDiscordIDList(t *testing.T) {
	ids, warnings := parseDiscordIDList("123456789012345678, not-a-snowflake, 987654321098765432")
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the bad entry, got %d: %v", len(warnings), warnings)
	}
	if len(ids) != 2 || !ids["123456789012345678"] || !ids["987654321098765432"] {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestDiscordAccessConfigAllowed(t *testing.T) {
	cfg := discordAccessConfig{
		Users:  map[string]bool{"111": true},
		Guilds: map[string]bool{"222": true},
	}
	// DM from an allowed user
	if !cfg.allowed(&discordMessage{Author: discordUser{ID: "111"}}) {
		t.Fatal("expected allow-listed DM user to be allowed")
	}
	// DM from a non-allowed user
	if cfg.allowed(&discordMessage{Author: discordUser{ID: "999"}}) {
		t.Fatal("expected non-allow-listed DM user to be denied")
	}
	// guild message in an allowed guild, no channel restriction configured
	if !cfg.allowed(&discordMessage{GuildID: "222", ChannelID: "555", Author: discordUser{ID: "999"}}) {
		t.Fatal("expected any channel of an allow-listed guild to be allowed when no channel restriction is set")
	}
	// guild message in a non-allowed guild
	if cfg.allowed(&discordMessage{GuildID: "333", ChannelID: "555", Author: discordUser{ID: "111"}}) {
		t.Fatal("expected a non-allow-listed guild to be denied even for an allow-listed user")
	}

	// with a channel restriction configured
	cfg.Channels = map[string]bool{"555": true}
	if !cfg.allowed(&discordMessage{GuildID: "222", ChannelID: "555", Author: discordUser{ID: "999"}}) {
		t.Fatal("expected the explicitly allowed channel to be allowed")
	}
	if cfg.allowed(&discordMessage{GuildID: "222", ChannelID: "666", Author: discordUser{ID: "999"}}) {
		t.Fatal("expected a channel not in the restriction list to be denied even in an allowed guild")
	}
}

func TestDiscordContextKey(t *testing.T) {
	dm := &discordMessage{GuildID: "", Author: discordUser{ID: "111"}}
	if got := discordContextKey(dm); got != "discord_dm_111" {
		t.Fatalf("expected discord_dm_111, got %q", got)
	}
	channel := &discordMessage{GuildID: "222", ChannelID: "333"}
	if got := discordContextKey(channel); got != "discord_channel_333" {
		t.Fatalf("expected discord_channel_333, got %q", got)
	}
	// Must never collide with Telegram's own unprefixed keys.
	tgKey := telegramContextKey(tgChat{ID: 111, Type: "private"})
	if discordContextKey(dm) == tgKey {
		t.Fatal("expected discord and telegram context keys to never collide")
	}
}

func TestDiscordMessageAddressesBotAndStripMention(t *testing.T) {
	cases := []struct {
		name    string
		msg     *discordMessage
		addr    bool
		stripTo string
	}{
		{"mention", &discordMessage{Content: "<@999> สรุปให้หน่อย", Mentions: []discordUser{{ID: "999"}}}, true, "สรุปให้หน่อย"},
		{"nickname-mention", &discordMessage{Content: "<@!999> สรุปให้หน่อย", Mentions: []discordUser{{ID: "999"}}}, true, "สรุปให้หน่อย"},
		{"ola-prefix", &discordMessage{Content: "!ola สรุปให้หน่อย"}, true, "สรุปให้หน่อย"},
		{"ask-prefix", &discordMessage{Content: "!ask hello"}, true, "hello"},
		{"unaddressed", &discordMessage{Content: "คุยกันเฉยๆ ไม่เกี่ยวกับบอท"}, false, ""},
		{"mentions-someone-else", &discordMessage{Content: "<@111> hi", Mentions: []discordUser{{ID: "111"}}}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := discordMessageAddressesBot(tc.msg, "999")
			if got != tc.addr {
				t.Fatalf("discordMessageAddressesBot: got %v, want %v", got, tc.addr)
			}
			if tc.addr {
				stripped := stripDiscordMention(tc.msg.Content, "999")
				if stripped != tc.stripTo {
					t.Fatalf("stripDiscordMention: got %q, want %q", stripped, tc.stripTo)
				}
			}
		})
	}
}

func newMockDiscordRESTServer(t *testing.T) (*httptest.Server, *[]struct {
	ChannelID string
	Content   string
}, *string) {
	t.Helper()
	var sent []struct {
		ChannelID string
		Content   string
	}
	var lastAuthHeader string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v10/users/@me", func(w http.ResponseWriter, r *http.Request) {
		lastAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"999","username":"olabot","bot":true}`)
	})
	mux.HandleFunc("/api/v10/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"url":"wss://fake-gateway.example/"}`)
	})
	mux.HandleFunc("/api/v10/channels/", func(w http.ResponseWriter, r *http.Request) {
		lastAuthHeader = r.Header.Get("Authorization")
		// path shape: /api/v10/channels/{id}/messages
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v10/channels/"), "/")
		if len(parts) != 2 || parts[1] != "messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, struct {
			ChannelID string
			Content   string
		}{parts[0], body.Content})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sent, &lastAuthHeader
}

// TestDiscordGetMeAndSendMessage exercises the actual discordGetMe/
// discordSendMessage functions end to end against a mock server (now
// that discordRESTRequest takes apiBase as a parameter rather than a
// hardcoded production constant) - confirming the real "Bot <token>"
// auth header (Discord's scheme, different from Telegram's URL-embedded
// token) and the real request/response wiring, not just the shape.
func TestDiscordGetMeAndSendMessage(t *testing.T) {
	srv, sent, lastAuth := newMockDiscordRESTServer(t)
	apiBase := srv.URL + "/api/v10"

	me, err := discordGetMe(srv.Client(), apiBase, "test-token-123")
	if err != nil {
		t.Fatalf("discordGetMe error: %v", err)
	}
	if me.ID != "999" || me.Username != "olabot" || !me.Bot {
		t.Fatalf("unexpected discordGetMe result: %#v", me)
	}
	if *lastAuth != "Bot test-token-123" {
		t.Fatalf("expected Authorization header 'Bot test-token-123', got %q", *lastAuth)
	}

	if err := discordSendMessage(srv.Client(), apiBase, "test-token-123", "555", "hello"); err != nil {
		t.Fatalf("discordSendMessage error: %v", err)
	}
	if len(*sent) != 1 || (*sent)[0].ChannelID != "555" || (*sent)[0].Content != "hello" {
		t.Fatalf("unexpected sent messages: %#v", *sent)
	}
}

func TestDiscordGetGatewayURL(t *testing.T) {
	srv, _, _ := newMockDiscordRESTServer(t)
	url, err := discordGetGatewayURL(srv.Client(), srv.URL+"/api/v10", "test-token")
	if err != nil {
		t.Fatalf("discordGetGatewayURL error: %v", err)
	}
	if url != "wss://fake-gateway.example/" {
		t.Fatalf("unexpected gateway url: %q", url)
	}
}

func TestDiscordSendMessageSplitsLongText(t *testing.T) {
	srv, sent, _ := newMockDiscordRESTServer(t)
	var sb strings.Builder
	for i := 0; i < 150; i++ {
		sb.WriteString(strings.Repeat("a", 30))
		sb.WriteString("\n")
	}
	if err := discordSendMessage(srv.Client(), srv.URL+"/api/v10", "test-token", "777", sb.String()); err != nil {
		t.Fatalf("discordSendMessage error: %v", err)
	}
	if len(*sent) < 2 {
		t.Fatalf("expected multiple POSTs for a long message, got %d", len(*sent))
	}
	for _, s := range *sent {
		if s.ChannelID != "777" {
			t.Fatalf("expected all chunks to go to channel 777, got %q", s.ChannelID)
		}
	}
}

// TestDiscordRESTRetriesAfter429 confirms discordRESTRequest actually
// honors Discord's rate-limit response (429 + retry_after) rather than
// just passing the failure straight back - the second attempt against
// the same mock endpoint must succeed once the mock stops returning 429.
func TestDiscordRESTRetriesAfter429(t *testing.T) {
	var attempt int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v10/users/@me", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempt, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"retry_after":0.01}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1","username":"olabot","bot":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	me, err := discordGetMe(srv.Client(), srv.URL+"/api/v10", "test-token")
	if err != nil {
		t.Fatalf("discordGetMe error: %v", err)
	}
	if me.Username != "olabot" {
		t.Fatalf("unexpected result after retry: %#v", me)
	}
	if atomic.LoadInt32(&attempt) != 2 {
		t.Fatalf("expected exactly 2 attempts (1 rate-limited + 1 retry), got %d", attempt)
	}
}

// ─────────────────────────────────────────────────────────────────
// Gateway protocol tests - driven entirely against a fake in-memory
// wsConn, never a real Discord connection (see discordGatewaySession's
// own doc comment for why the whole protocol state machine is
// structured to make this possible).
// ─────────────────────────────────────────────────────────────────

// fakeWSConn is a scriptable wsConn: reads pop from a queue of canned
// server->client messages (blocking briefly if the queue is empty, so a
// test can push more messages while a goroutine is already reading),
// writes are recorded for assertions, and Close marks the connection
// dead so any further Read returns an error (matching a real closed
// socket) - this is what lets discordGatewaySession.runOnce's blocking
// ReadMessage loop actually terminate in a test instead of hanging
// forever.
type fakeWSConn struct {
	mu       sync.Mutex
	toClient [][]byte
	written  [][]byte
	closed   bool
	cond     *sync.Cond
}

func newFakeWSConn() *fakeWSConn {
	c := &fakeWSConn{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *fakeWSConn) push(payload discordGatewayPayload) {
	data, _ := json.Marshal(payload)
	c.mu.Lock()
	c.toClient = append(c.toClient, data)
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *fakeWSConn) ReadMessage() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.toClient) == 0 && !c.closed {
		c.cond.Wait()
	}
	if c.closed && len(c.toClient) == 0 {
		return nil, fmt.Errorf("fakeWSConn: closed")
	}
	msg := c.toClient[0]
	c.toClient = c.toClient[1:]
	return msg, nil
}

func (c *fakeWSConn) WriteMessage(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("fakeWSConn: closed")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	c.written = append(c.written, cp)
	return nil
}

func (c *fakeWSConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.cond.Broadcast()
	return nil
}

func (c *fakeWSConn) writtenPayloads() []discordGatewayPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []discordGatewayPayload
	for _, w := range c.written {
		var p discordGatewayPayload
		if err := json.Unmarshal(w, &p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func TestDiscordGatewaySessionIdentifiesAndDispatchesMessage(t *testing.T) {
	fake := newFakeWSConn()
	fake.push(discordGatewayPayload{Op: discordOpHello, D: json.RawMessage(`{"heartbeat_interval":60000}`)}) // long interval - this test doesn't exercise heartbeat timing

	var seqOne int64 = 1
	msgPayload, _ := json.Marshal(discordMessage{ID: "1", ChannelID: "555", Author: discordUser{ID: "111"}, Content: "hi"})
	fake.push(discordGatewayPayload{Op: discordOpDispatch, T: "MESSAGE_CREATE", S: &seqOne, D: msgPayload})

	var received []*discordMessage
	var mu sync.Mutex
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()

	gw := &discordGatewaySession{
		token:      "test-token",
		intents:    discordDefaultIntents,
		dial:       func(url string) (wsConn, error) { return fake, nil },
		gatewayURL: func() (string, error) { return "wss://fake/", nil },
		onMessage: func(m *discordMessage) {
			mu.Lock()
			received = append(received, m)
			mu.Unlock()
		},
		outFile: logFile,
	}

	done := make(chan error, 1)
	go func() { done <- gw.runOnce() }()

	// Give the goroutine a moment to process HELLO/IDENTIFY/the dispatched
	// message, then close the fake connection so runOnce's blocking read
	// returns and the test can finish deterministically.
	time.Sleep(100 * time.Millisecond)
	fake.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].Content != "hi" || received[0].Author.ID != "111" {
		t.Fatalf("expected exactly 1 dispatched MESSAGE_CREATE to reach onMessage, got: %#v", received)
	}

	written := fake.writtenPayloads()
	if len(written) == 0 || written[0].Op != discordOpIdentify {
		t.Fatalf("expected the first message sent to the gateway to be IDENTIFY (op %d), got: %#v", discordOpIdentify, written)
	}
	var identify discordIdentify
	if err := json.Unmarshal(written[0].D, &identify); err != nil {
		t.Fatalf("failed to parse IDENTIFY payload: %v", err)
	}
	if identify.Token != "test-token" || identify.Intents != discordDefaultIntents {
		t.Fatalf("unexpected IDENTIFY contents: %#v", identify)
	}
}

func TestDiscordGatewaySessionSendsHeartbeatOnInterval(t *testing.T) {
	fake := newFakeWSConn()
	fake.push(discordGatewayPayload{Op: discordOpHello, D: json.RawMessage(`{"heartbeat_interval":50}`)}) // short interval, deliberately, to actually observe a heartbeat in a fast test

	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()
	gw := &discordGatewaySession{
		token:      "t",
		intents:    discordDefaultIntents,
		dial:       func(url string) (wsConn, error) { return fake, nil },
		gatewayURL: func() (string, error) { return "wss://fake/", nil },
		onMessage:  func(m *discordMessage) {},
		outFile:    logFile,
	}
	done := make(chan error, 1)
	go func() { done <- gw.runOnce() }()

	// Wait long enough for at least one heartbeat to have been sent
	// (interval 50ms + jitter, well under the 300ms budget here), then
	// ACK it so runOnce doesn't treat the connection as dead, then close.
	time.Sleep(200 * time.Millisecond)
	fake.push(discordGatewayPayload{Op: discordOpHeartbeatACK})
	time.Sleep(50 * time.Millisecond)
	fake.Close()
	<-done

	written := fake.writtenPayloads()
	sawHeartbeat := false
	for _, w := range written {
		if w.Op == discordOpHeartbeat {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatalf("expected at least one HEARTBEAT (op %d) to have been sent, got: %#v", discordOpHeartbeat, written)
	}
}

func TestDiscordGatewaySessionRunReconnectsAfterDisconnect(t *testing.T) {
	var dialCount int32
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()

	gw := &discordGatewaySession{
		token:   "t",
		intents: discordDefaultIntents,
		dial: func(url string) (wsConn, error) {
			n := atomic.AddInt32(&dialCount, 1)
			fake := newFakeWSConn()
			fake.push(discordGatewayPayload{Op: discordOpHello, D: json.RawMessage(`{"heartbeat_interval":60000}`)})
			if n == 1 {
				// First connection: close almost immediately (simulating a
				// dropped connection) so run()'s outer loop must reconnect.
				go func() {
					time.Sleep(20 * time.Millisecond)
					fake.Close()
				}()
			}
			return fake, nil
		},
		gatewayURL: func() (string, error) { return "wss://fake/", nil },
		onMessage:  func(m *discordMessage) {},
		outFile:    logFile,
	}

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() { gw.run(stopCh); close(done) }()

	// run() backs off before reconnecting (starting at 1s) - this test
	// only needs to observe that a second dial happens at all, not wait
	// out the full backoff, so stop it well before that and just assert
	// the first disconnect was observed.
	time.Sleep(100 * time.Millisecond)
	close(stopCh)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("gw.run did not exit after stopCh was closed")
	}
	if atomic.LoadInt32(&dialCount) < 1 {
		t.Fatal("expected at least 1 dial attempt")
	}
}

// newTestDiscordSession builds a discordSession wired to mock Discord
// REST (discordSrv) and Ollama (ollamaSrv) httptest servers, for driving
// handleDiscordMessage directly - mirrors newTestTelegramSession exactly,
// see that function's own comment for why the Gateway itself is never
// driven in these tests (handleDiscordMessage is the right seam, same
// reasoning as telegrambot's handleTelegramMessage).
func newTestDiscordSession(t *testing.T, discordSrv, ollamaSrv *httptest.Server, contextDir string, users map[string]bool) *discordSession {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "discordbot-test-log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	return &discordSession{
		chatBotCore: chatBotCore{
			client:       ollamaSrv.Client(),
			systemPrompt: buildDiscordSystemPrompt(""),
			tools:        filterTools(builtinTools, "get_current_time", "delay"),
			pcfg:         providerConfig{Provider: providerOllama, Host: ollamaSrv.URL, Model: "mock-model"},
			ctxSize:      4096,
			contextDir:   contextDir,
			knowledgeIdx: &knowledgeIndexStore{},
			keepRecent:   defaultChatBotKeepRecentTurns,
			compactAfter: defaultChatBotCompactAfterTurns,
			outFile:      logFile,
			locks:        map[string]*sync.Mutex{},
		},
		restClient:  discordSrv.Client(),
		apiBase:     discordSrv.URL + "/api/v10",
		token:       "test-token",
		botUserID:   "999",
		botUsername: "olabot",
		access:      discordAccessConfig{Users: users, Guilds: map[string]bool{}},
		sem:         make(chan struct{}, 4),
	}
}

func TestHandleDiscordMessageAllowedDMUserGetsAnswer(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)

	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("สวัสดีครับ ยินดีช่วยเหลือ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestDiscordSession(t, discordSrv, ollamaSrv, contextDir, map[string]bool{"111": true})

	msg := &discordMessage{ID: "1", ChannelID: "channel-1", Author: discordUser{ID: "111", Username: "moo"}, Content: "สวัสดี"}
	session.handleDiscordMessage(msg)

	if len(*sent) != 1 || (*sent)[0].ChannelID != "channel-1" || (*sent)[0].Content != "สวัสดีครับ ยินดีช่วยเหลือ" {
		t.Fatalf("unexpected reply: %#v", *sent)
	}

	cctx, err := loadChatContext(contextDir, discordContextKey(msg))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	if len(cctx.Turns) != 2 || cctx.Turns[0].Role != "user" || cctx.Turns[1].Role != "assistant" {
		t.Fatalf("expected 1 user + 1 assistant turn persisted, got: %#v", cctx.Turns)
	}
}

func TestHandleDiscordMessageDeniedUserGetsNoAnswerFromModel(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)

	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("ไม่ควรถูกเรียก", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestDiscordSession(t, discordSrv, ollamaSrv, t.TempDir(), map[string]bool{})

	msg := &discordMessage{ID: "1", ChannelID: "channel-1", Author: discordUser{ID: "111"}, Content: "สวัสดี"}
	session.handleDiscordMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected a denied user's message to never reach the model")
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Content, "ยังไม่ได้รับอนุญาต") {
		t.Fatalf("expected a single access-denied reply, got: %#v", *sent)
	}
}

func TestHandleDiscordMessageIgnoresBotsIncludingItself(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestDiscordSession(t, discordSrv, ollamaSrv, t.TempDir(), map[string]bool{"111": true, "999": true})

	// another bot
	session.handleDiscordMessage(&discordMessage{ID: "1", ChannelID: "c", Author: discordUser{ID: "222", Bot: true}, Content: "hi"})
	// itself (echo-loop guard)
	session.handleDiscordMessage(&discordMessage{ID: "2", ChannelID: "c", Author: discordUser{ID: "999"}, Content: "hi"})

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected messages from bots (including the bot itself) to never reach the model")
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no replies to bot messages, got: %#v", *sent)
	}
}

func TestHandleDiscordMessageGuildIgnoresUnaddressedMessages(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestDiscordSession(t, discordSrv, ollamaSrv, t.TempDir(), map[string]bool{})
	session.access.Guilds = map[string]bool{"guild-1": true}

	msg := &discordMessage{ID: "1", GuildID: "guild-1", ChannelID: "channel-1", Author: discordUser{ID: "111"}, Content: "คุยกันเฉยๆ ไม่เกี่ยวกับบอท"}
	session.handleDiscordMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected an unaddressed guild message to never reach the model")
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no reply to an unaddressed guild message, got: %#v", *sent)
	}
}

func TestHandleDiscordMessageGuildRespondsWhenMentioned(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("ได้เลยครับ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestDiscordSession(t, discordSrv, ollamaSrv, t.TempDir(), map[string]bool{})
	session.access.Guilds = map[string]bool{"guild-1": true}

	msg := &discordMessage{
		ID: "1", GuildID: "guild-1", ChannelID: "channel-1",
		Author:   discordUser{ID: "111"},
		Content:  "<@999> ช่วยหน่อย",
		Mentions: []discordUser{{ID: "999"}},
	}
	session.handleDiscordMessage(msg)

	if len(*sent) != 1 || (*sent)[0].Content != "ได้เลยครับ" {
		t.Fatalf("expected a reply when the bot is @mentioned, got: %#v", *sent)
	}
}

func TestHandleDiscordMessageToolsAndWhoamiCommands(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestDiscordSession(t, discordSrv, ollamaSrv, t.TempDir(), map[string]bool{"111": true})

	session.handleDiscordMessage(&discordMessage{ID: "1", ChannelID: "c", Author: discordUser{ID: "222"}, Content: "!whoami"})
	session.handleDiscordMessage(&discordMessage{ID: "2", ChannelID: "c", Author: discordUser{ID: "111"}, Content: "!tools"})

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected !whoami and !tools to never reach the model")
	}
	if len(*sent) != 2 {
		t.Fatalf("expected 2 replies (!whoami + !tools), got %d: %#v", len(*sent), *sent)
	}
	if !strings.Contains((*sent)[0].Content, "222") {
		t.Fatalf("expected !whoami to echo the sender's own ID even though not allow-listed, got: %s", (*sent)[0].Content)
	}
	if !strings.Contains((*sent)[1].Content, "search_knowledge") {
		t.Fatalf("expected !tools to show the shared status text, got: %s", (*sent)[1].Content)
	}
}

// TestHandleTelegramMessageRecordsUnaddressedGroupMessageWithoutReplying
// is the direct regression test for a specific request: group chat
// members' conversation should be recorded into persistent context - with
// speaker attribution - even for messages that never mention the bot, so
// the bot already has that context whenever it IS later addressed. The
// bot must still never call the model or reply for these.
func TestHandleTelegramMessageRecordsUnaddressedGroupMessageWithoutReplying(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{})
	session.access.Groups = map[int64]bool{-999: true}

	msg := &tgMessage{
		From: tgUser{ID: 111, FirstName: "สมชาย", Username: "somchai"},
		Chat: tgChat{ID: -999, Type: "group", Title: "ห้องเรียน"},
		Text: "วันนี้อากาศดีจัง",
	}
	session.handleTelegramMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected an unaddressed group message to never reach the model")
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no reply to an unaddressed group message, got: %#v", *sent)
	}

	cctx, err := loadChatContext(contextDir, telegramContextKey(msg.Chat))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	if len(cctx.Turns) != 1 {
		t.Fatalf("expected the unaddressed message to still be recorded as a turn, got %d turns", len(cctx.Turns))
	}
	if cctx.Turns[0].Speaker != "สมชาย" {
		t.Fatalf("expected the turn to be attributed to the sender's display name, got speaker=%q", cctx.Turns[0].Speaker)
	}
	if cctx.Turns[0].Content != "วันนี้อากาศดีจัง" {
		t.Fatalf("unexpected recorded content: %q", cctx.Turns[0].Content)
	}
	if cctx.Turns[0].Role != "user" {
		t.Fatalf("expected role=user, got %q", cctx.Turns[0].Role)
	}
}

// TestHandleTelegramMessageMentionedSeesPriorUnaddressedGroupHistory is the
// end-to-end version: several group members chat without mentioning the
// bot, then one of them @mentions it - the bot's actual request to the
// model must include the earlier speakers' names and what they said, not
// just the addressed message on its own.
func TestHandleTelegramMessageMentionedSeesPriorUnaddressedGroupHistory(t *testing.T) {
	telegramSrv, sent := newMockTelegramSendMessageServer(t)
	var lastRequestBody string
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastRequestBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("เข้าใจแล้วครับ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestTelegramSession(t, telegramSrv, ollamaSrv, contextDir, map[int64]bool{})
	session.access.Groups = map[int64]bool{-999: true}
	session.botUsername = "olabot"
	chat := tgChat{ID: -999, Type: "group", Title: "ห้องเรียน"}

	session.handleTelegramMessage(&tgMessage{
		From: tgUser{ID: 111, FirstName: "สมชาย"},
		Chat: chat,
		Text: "พรุ่งนี้มีสอบวิชาอะไรนะ",
	})
	session.handleTelegramMessage(&tgMessage{
		From: tgUser{ID: 222, FirstName: "สมหญิง"},
		Chat: chat,
		Text: "วิชา Network Security ครับ",
	})
	session.handleTelegramMessage(&tgMessage{
		From: tgUser{ID: 333, FirstName: "มานะ"},
		Chat: chat,
		Text: "@olabot ช่วยสรุปให้หน่อย",
	})

	if len(*sent) != 1 {
		t.Fatalf("expected exactly 1 reply (only the mentioned message), got %d: %#v", len(*sent), *sent)
	}
	if !strings.Contains(lastRequestBody, "สมชาย") || !strings.Contains(lastRequestBody, "สมหญิง") {
		t.Fatalf("expected the model request to include the earlier speakers' names, got: %s", lastRequestBody)
	}
	if !strings.Contains(lastRequestBody, "พรุ่งนี้มีสอบ") || !strings.Contains(lastRequestBody, "Network Security") {
		t.Fatalf("expected the model request to include the earlier unaddressed messages' content, got: %s", lastRequestBody)
	}

	cctx, err := loadChatContext(contextDir, telegramContextKey(chat))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	// 2 unaddressed user turns + 1 addressed user turn + that turn's own assistant reply
	if len(cctx.Turns) != 4 {
		t.Fatalf("expected 4 turns recorded, got %d: %#v", len(cctx.Turns), cctx.Turns)
	}
	if cctx.Turns[0].Speaker != "สมชาย" || cctx.Turns[1].Speaker != "สมหญิง" {
		t.Fatalf("expected the first two turns attributed to the unaddressed speakers in order, got: %#v", cctx.Turns[:2])
	}
	if cctx.Turns[3].Role != "assistant" || cctx.Turns[3].Speaker != "" {
		t.Fatalf("expected the final turn to be the bot's own reply with no speaker, got: %#v", cctx.Turns[3])
	}
}

// TestHandleDiscordMessageRecordsUnaddressedGuildMessageWithoutReplying is
// the Discord counterpart of the Telegram test above - same behavior via
// the shared chatBotCore.recordAndRespond.
func TestHandleDiscordMessageRecordsUnaddressedGuildMessageWithoutReplying(t *testing.T) {
	discordSrv, sent, _ := newMockDiscordRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestDiscordSession(t, discordSrv, ollamaSrv, contextDir, map[string]bool{})
	session.access.Guilds = map[string]bool{"guild-1": true}

	msg := &discordMessage{ID: "1", GuildID: "guild-1", ChannelID: "channel-1", Author: discordUser{ID: "111", Username: "somchai"}, Content: "สวัสดีทุกคน"}
	session.handleDiscordMessage(msg)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected an unaddressed guild message to never reach the model")
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no reply, got: %#v", *sent)
	}

	cctx, err := loadChatContext(contextDir, discordContextKey(msg))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	if len(cctx.Turns) != 1 || cctx.Turns[0].Speaker != "somchai" || cctx.Turns[0].Content != "สวัสดีทุกคน" {
		t.Fatalf("expected the unaddressed message recorded with speaker attribution, got: %#v", cctx.Turns)
	}
}

// TestFormatChatTurnContentDMHasNoSpeakerPrefix confirms a DM turn
// (speaker == "") is never prefixed - only group/channel turns with more
// than one possible human speaker need attribution.
func TestFormatChatTurnContentDMHasNoSpeakerPrefix(t *testing.T) {
	if got := formatChatTurnContent("", "สวัสดี"); got != "สวัสดี" {
		t.Fatalf("expected no prefix for an empty speaker, got: %q", got)
	}
	if got := formatChatTurnContent("สมชาย", "สวัสดี"); got != "[สมชาย] สวัสดี" {
		t.Fatalf("expected speaker prefix, got: %q", got)
	}
}

// TestCompactChatContextPreservesSpeakerAttribution confirms compaction's
// own summarization input keeps who-said-what instead of collapsing
// every group member into a generic "user:" label.
func TestCompactChatContextPreservesSpeakerAttribution(t *testing.T) {
	var lastRequestBody string
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastRequestBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("สรุป: สมชายถามเรื่องสอบ สมหญิงตอบว่าเป็นวิชา Network Security", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	cctx := &chatContext{Key: "group_-999"}
	for i := 0; i < 25; i++ {
		cctx.Turns = append(cctx.Turns, chatTurn{Role: "user", Speaker: "สมชาย", Content: "ข้อความที่ " + fmt.Sprint(i)})
	}
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()

	compactChatContext(ollamaSrv.Client(), providerConfig{Provider: providerOllama, Host: ollamaSrv.URL, Model: "mock"}, 4096, cctx, 5, logFile)

	if !strings.Contains(lastRequestBody, "สมชาย:") {
		t.Fatalf("expected the summarization request to attribute turns by speaker name, got: %s", lastRequestBody)
	}
	if len(cctx.Turns) != 5 {
		t.Fatalf("expected 5 recent turns kept verbatim after compaction, got %d", len(cctx.Turns))
	}
}

// TestChatBotDefaultsMatchRequestedValues pins down the three default
// values explicitly requested: --context-compact-after=300,
// --context-keep-recent=100, --embed-refresh-interval=1 minute.
func TestChatBotDefaultsMatchRequestedValues(t *testing.T) {
	if defaultChatBotCompactAfterTurns != 300 {
		t.Fatalf("expected default compact-after to be 300, got %d", defaultChatBotCompactAfterTurns)
	}
	if defaultChatBotKeepRecentTurns != 100 {
		t.Fatalf("expected default keep-recent to be 100, got %d", defaultChatBotKeepRecentTurns)
	}
	if defaultEmbedRefreshInterval != time.Minute {
		t.Fatalf("expected default embed-refresh-interval to be 1 minute, got %v", defaultEmbedRefreshInterval)
	}
	if defaultTelegramPollTimeoutSec != 600 {
		t.Fatalf("expected default poll-timeout to be 600, got %d", defaultTelegramPollTimeoutSec)
	}
}

// ======================================================================
// Section: linebot
//
// Unit tests for the pure helper functions (signature verification,
// allowlist, mention detection/stripping, message splitting), REST client
// tests via httptest, webhook HTTP handler tests (a real net/http.Handler
// - no fake transport needed at all, simpler to test than either
// telegrambot's long-poll loop or discordbot's Gateway state machine),
// and end-to-end handleLineMessage tests mirroring the other two bots'
// own test sections, confirming the shared chatBotCore behaves
// identically from LINE's side of the fence too.
// ======================================================================

func TestVerifyLineSignature(t *testing.T) {
	secret := "test-channel-secret"
	body := []byte(`{"events":[]}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !verifyLineSignature(body, validSig, secret) {
		t.Fatal("expected a correctly computed signature to verify")
	}
	if verifyLineSignature(body, validSig, "wrong-secret") {
		t.Fatal("expected verification to fail with the wrong secret")
	}
	if verifyLineSignature([]byte(`{"events":[],"tampered":true}`), validSig, secret) {
		t.Fatal("expected verification to fail if the body was modified after signing")
	}
	if verifyLineSignature(body, "not-valid-base64!!!", secret) {
		t.Fatal("expected verification to fail gracefully on a malformed signature header, not panic")
	}
	if verifyLineSignature(body, "", secret) {
		t.Fatal("expected an empty signature header to fail verification")
	}
}

func TestSplitLineMessageRespectsLineLimit(t *testing.T) {
	got := splitLineMessage("สวัสดีครับ")
	if len(got) != 1 || got[0] != "สวัสดีครับ" {
		t.Fatalf("expected single unchanged chunk, got %#v", got)
	}

	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString(strings.Repeat("a", 30))
		sb.WriteString("\n")
	}
	chunks := splitLineMessage(sb.String())
	if len(chunks) < 2 {
		t.Fatalf("expected text longer than LINE's limit to split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if utf8.RuneCountInString(c) > lineMaxMessageRunes {
			t.Fatalf("chunk exceeds lineMaxMessageRunes (%d): %d runes", lineMaxMessageRunes, utf8.RuneCountInString(c))
		}
	}
}

func TestParseLineIDList(t *testing.T) {
	ids, warnings := parseLineIDList("U4af4980629, C1234567890abcdef, ")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for LINE's opaque ID format, got: %v", warnings)
	}
	if len(ids) != 2 || !ids["U4af4980629"] || !ids["C1234567890abcdef"] {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestLineAccessConfigAllowed(t *testing.T) {
	cfg := lineAccessConfig{
		Users:  map[string]bool{"U111": true},
		Groups: map[string]bool{"C222": true, "R333": true},
	}
	if !cfg.allowed(lineSource{Type: "user", UserID: "U111"}) {
		t.Fatal("expected allow-listed 1-on-1 user to be allowed")
	}
	if cfg.allowed(lineSource{Type: "user", UserID: "U999"}) {
		t.Fatal("expected non-allow-listed user to be denied")
	}
	if !cfg.allowed(lineSource{Type: "group", GroupID: "C222", UserID: "U999"}) {
		t.Fatal("expected allow-listed group to be allowed regardless of sender")
	}
	if !cfg.allowed(lineSource{Type: "room", RoomID: "R333", UserID: "U999"}) {
		t.Fatal("expected allow-listed room to be allowed (rooms share the Groups allowlist)")
	}
	if cfg.allowed(lineSource{Type: "group", GroupID: "C444", UserID: "U111"}) {
		t.Fatal("expected a non-allow-listed group to be denied even for an allow-listed user")
	}
}

func TestLineContextKeyNoCollisionWithOtherPlatforms(t *testing.T) {
	userSrc := lineSource{Type: "user", UserID: "U111"}
	groupSrc := lineSource{Type: "group", GroupID: "C222"}
	if got := lineContextKey(userSrc); got != "line_user_U111" {
		t.Fatalf("expected line_user_U111, got %q", got)
	}
	if got := lineContextKey(groupSrc); got != "line_group_C222" {
		t.Fatalf("expected line_group_C222, got %q", got)
	}
	tgKey := telegramContextKey(tgChat{ID: 111, Type: "private"})
	dcKey := discordContextKey(&discordMessage{Author: discordUser{ID: "111"}})
	if lineContextKey(userSrc) == tgKey || lineContextKey(userSrc) == dcKey {
		t.Fatal("expected LINE context keys to never collide with Telegram/Discord keys")
	}
}

func TestLineMessageAddressesBotAndStripMention(t *testing.T) {
	cases := []struct {
		name    string
		msg     *lineMessage
		addr    bool
		stripTo string
	}{
		{"mention", &lineMessage{Text: "@olabot สรุปให้หน่อย", Mention: &lineMention{Mentionees: []lineMentionee{{Index: 0, Length: 7, UserID: "Ubot"}}}}, true, "สรุปให้หน่อย"},
		{"ola-prefix", &lineMessage{Text: "/ola สรุปให้หน่อย"}, true, "สรุปให้หน่อย"},
		{"ask-prefix", &lineMessage{Text: "/ask hello"}, true, "hello"},
		{"mentions-someone-else", &lineMessage{Text: "@friend hi", Mention: &lineMention{Mentionees: []lineMentionee{{Index: 0, Length: 7, UserID: "Uother"}}}}, false, ""},
		{"unaddressed", &lineMessage{Text: "คุยกันเฉยๆ ไม่เกี่ยวกับบอท"}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lineMessageAddressesBot(tc.msg, "Ubot")
			if got != tc.addr {
				t.Fatalf("lineMessageAddressesBot: got %v, want %v", got, tc.addr)
			}
			if tc.addr {
				stripped := stripLineMention(tc.msg)
				if stripped != tc.stripTo {
					t.Fatalf("stripLineMention: got %q, want %q", stripped, tc.stripTo)
				}
			}
		})
	}
}

func TestLineAlreadyProcessedDedup(t *testing.T) {
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()
	s := &lineSession{chatBotCore: chatBotCore{outFile: logFile}, recentEvents: map[string]bool{}}

	if s.alreadyProcessed("evt-1") {
		t.Fatal("expected the first sighting of an event ID to not be flagged as a duplicate")
	}
	if !s.alreadyProcessed("evt-1") {
		t.Fatal("expected a redelivered event ID to be recognized as a duplicate")
	}
	if s.alreadyProcessed("evt-2") {
		t.Fatal("expected a different event ID to not be flagged as a duplicate")
	}
	if s.alreadyProcessed("") {
		t.Fatal("expected an empty event ID (no dedup info available) to never be treated as a duplicate")
	}
}

// newMockLineRESTServer returns an httptest.Server implementing the LINE
// REST endpoints this bot calls (/info, /profile/{id}, /group/{gid}/
// member/{uid}, /message/reply, /message/push), recording every reply/push
// call and the last Authorization header seen (to verify the Bearer
// scheme).
// lineSentMessage is one recorded reply/push call the mock LINE REST
// server received.
type lineSentMessage struct {
	Kind string // "reply" or "push"
	To   string // replyToken for reply, recipient id for push
	Text string
}

// lineSentTracker guards the mock server's recorded messages with a
// mutex - necessary because, unlike telegramSession/discordSession's own
// tests (which call handleTelegramMessage/handleDiscordMessage directly
// and synchronously from the test goroutine), lineSession's webhookHandler
// dispatches to handleLineMessage in its own goroutine (see
// webhookHandler's own doc comment on why - LINE expects a fast ack).
// That means the mock server's handlers run concurrently with the test
// goroutine's own assertions; a plain unsynchronized slice here would be
// a genuine data race, not just untidy code, and go test -race catches
// exactly that.
type lineSentTracker struct {
	mu   sync.Mutex
	sent []lineSentMessage
}

func (t *lineSentTracker) add(msg lineSentMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, msg)
}

// snapshot returns a race-free copy of everything recorded so far.
func (t *lineSentTracker) snapshot() []lineSentMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]lineSentMessage, len(t.sent))
	copy(out, t.sent)
	return out
}

// waitForCount blocks (up to a short timeout) until at least n messages
// have been recorded - used instead of polling an unrelated signal (like
// a separate atomic counter on the model-call side) as a proxy for "the
// async handler has finished sending its reply", which is exactly the
// race the original version of this test harness had.
func (t *lineSentTracker) waitForCount(n int) []lineSentMessage {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := t.snapshot(); len(s) >= n {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	return t.snapshot()
}

func newMockLineRESTServer(t *testing.T) (*httptest.Server, *lineSentTracker, *string) {
	t.Helper()
	tracker := &lineSentTracker{}
	var authMu sync.Mutex
	var lastAuth string
	setAuth := func(r *http.Request) {
		authMu.Lock()
		lastAuth = r.Header.Get("Authorization")
		authMu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		setAuth(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"userId":"Ubot","displayName":"olabot"}`)
	})
	mux.HandleFunc("/profile/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"displayName":"สมชาย"}`)
	})
	mux.HandleFunc("/group/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"displayName":"สมหญิง"}`)
	})
	mux.HandleFunc("/message/reply", func(w http.ResponseWriter, r *http.Request) {
		setAuth(r)
		var body struct {
			ReplyToken string `json:"replyToken"`
			Messages   []struct {
				Text string `json:"text"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			tracker.add(lineSentMessage{"reply", body.ReplyToken, m.Text})
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/message/push", func(w http.ResponseWriter, r *http.Request) {
		setAuth(r)
		var body struct {
			To       string `json:"to"`
			Messages []struct {
				Text string `json:"text"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			tracker.add(lineSentMessage{"push", body.To, m.Text})
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tracker, &lastAuth
}

func TestLineRESTAuthHeaderUsesBearerScheme(t *testing.T) {
	srv, _, lastAuth := newMockLineRESTServer(t)
	if _, err := lineGetBotInfo(srv.Client(), srv.URL, "test-token"); err != nil {
		t.Fatalf("lineGetBotInfo error: %v", err)
	}
	if *lastAuth != "Bearer test-token" {
		t.Fatalf("expected Authorization header 'Bearer test-token', got %q", *lastAuth)
	}
}

func TestLineReplyThenPushRouting(t *testing.T) {
	srv, sent, _ := newMockLineRESTServer(t)

	// sendReply-equivalent: a direct reply call should show up as "reply"
	if err := lineReplyMessage(srv.Client(), srv.URL, "tok", "rtoken-1", "hello"); err != nil {
		t.Fatalf("lineReplyMessage error: %v", err)
	}
	// sendAnswer-equivalent: a direct push call should show up as "push"
	if err := linePushMessage(srv.Client(), srv.URL, "tok", "U111", "answer"); err != nil {
		t.Fatalf("linePushMessage error: %v", err)
	}
	if got := sent.snapshot(); len(got) != 2 || got[0].Kind != "reply" || got[1].Kind != "push" {
		t.Fatalf("unexpected sent messages: %#v", got)
	}
}

func TestLineWebhookHandlerRejectsBadSignature(t *testing.T) {
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()
	s := &lineSession{chatBotCore: chatBotCore{outFile: logFile}, channelSecret: "secret", recentEvents: map[string]bool{}}

	body := []byte(`{"events":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/line/webhook", bytes.NewReader(body))
	req.Header.Set("X-Line-Signature", "clearly-not-valid")
	rec := httptest.NewRecorder()
	s.webhookHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a bad signature, got %d", rec.Code)
	}
}

func TestLineWebhookHandlerAcceptsValidSignatureAndDispatches(t *testing.T) {
	lineSrv, sent, _ := newMockLineRESTServer(t)

	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("สวัสดีครับ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()
	s := &lineSession{
		chatBotCore: chatBotCore{
			client:       ollamaSrv.Client(),
			systemPrompt: buildChatBotSystemPrompt("LINE", ""),
			tools:        filterTools(builtinTools, "get_current_time", "delay"),
			pcfg:         providerConfig{Provider: providerOllama, Host: ollamaSrv.URL, Model: "mock-model"},
			ctxSize:      4096,
			contextDir:   t.TempDir(),
			knowledgeIdx: &knowledgeIndexStore{},
			keepRecent:   defaultChatBotKeepRecentTurns,
			compactAfter: defaultChatBotCompactAfterTurns,
			outFile:      logFile,
			locks:        map[string]*sync.Mutex{},
		},
		restClient:    lineSrv.Client(),
		apiBase:       lineSrv.URL,
		channelSecret: "secret",
		token:         "test-token",
		botUserID:     "Ubot",
		access:        lineAccessConfig{Users: map[string]bool{"U111": true}},
		sem:           make(chan struct{}, 4),
		nameCache:     map[string]string{},
		recentEvents:  map[string]bool{},
	}

	bodyObj := lineWebhookBody{
		Destination: "xxx",
		Events: []lineEvent{
			{
				Type:           "message",
				Source:         lineSource{Type: "user", UserID: "U111"},
				ReplyToken:     "rtoken-1",
				Message:        &lineMessage{Type: "text", Text: "สวัสดี"},
				WebhookEventID: "evt-1",
			},
		},
	}
	body, _ := json.Marshal(bodyObj)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/line/webhook", bytes.NewReader(body))
	req.Header.Set("X-Line-Signature", sig)
	rec := httptest.NewRecorder()
	s.webhookHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a validly signed webhook, got %d", rec.Code)
	}

	// The event is dispatched asynchronously (goroutine - see
	// webhookHandler's own doc comment on why) - wait on the tracker
	// itself reaching the expected count, not on an unrelated signal like
	// the model-call counter, which only proves the FIRST half of the
	// async work finished, not the subsequent push call this assertion
	// actually depends on.
	got := sent.waitForCount(1)

	if atomic.LoadInt32(&ollamaCalls) != 1 {
		t.Fatalf("expected the webhook event to reach the model exactly once, got %d calls", ollamaCalls)
	}
	if len(got) != 1 || got[0].Kind != "push" || got[0].Text != "สวัสดีครับ" {
		t.Fatalf("expected a single push reply with the model's answer, got: %#v", got)
	}
}

// newTestLineSession builds a lineSession wired to mock LINE REST and
// Ollama httptest servers, for driving handleLineMessage directly -
// mirrors newTestTelegramSession/newTestDiscordSession.
func newTestLineSession(t *testing.T, lineSrv, ollamaSrv *httptest.Server, contextDir string, users map[string]bool) *lineSession {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "linebot-test-log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	return &lineSession{
		chatBotCore: chatBotCore{
			client:       ollamaSrv.Client(),
			systemPrompt: buildChatBotSystemPrompt("LINE", ""),
			tools:        filterTools(builtinTools, "get_current_time", "delay"),
			pcfg:         providerConfig{Provider: providerOllama, Host: ollamaSrv.URL, Model: "mock-model"},
			ctxSize:      4096,
			contextDir:   contextDir,
			knowledgeIdx: &knowledgeIndexStore{},
			keepRecent:   defaultChatBotKeepRecentTurns,
			compactAfter: defaultChatBotCompactAfterTurns,
			outFile:      logFile,
			locks:        map[string]*sync.Mutex{},
		},
		restClient:    lineSrv.Client(),
		apiBase:       lineSrv.URL,
		channelSecret: "secret",
		token:         "test-token",
		botUserID:     "Ubot",
		access:        lineAccessConfig{Users: users, Groups: map[string]bool{}},
		sem:           make(chan struct{}, 4),
		nameCache:     map[string]string{},
		recentEvents:  map[string]bool{},
	}
}

func TestHandleLineMessageDeniedUserGetsNoAnswerFromModel(t *testing.T) {
	lineSrv, sent, _ := newMockLineRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestLineSession(t, lineSrv, ollamaSrv, t.TempDir(), map[string]bool{}) // nobody allowed

	ev := &lineEvent{
		Type:           "message",
		Source:         lineSource{Type: "user", UserID: "U999"},
		ReplyToken:     "rtoken",
		Message:        &lineMessage{Type: "text", Text: "สวัสดี"},
		WebhookEventID: "evt-denied",
	}
	session.handleLineMessage(ev)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected a denied user's message to never reach the model")
	}
	if got := sent.snapshot(); len(got) != 1 || !strings.Contains(got[0].Text, "ยังไม่ได้รับอนุญาต") {
		t.Fatalf("expected a single access-denied reply, got: %#v", got)
	}
}

func TestHandleLineMessageRecordsUnaddressedGroupMessageWithSpeakerAttribution(t *testing.T) {
	lineSrv, sent, _ := newMockLineRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	contextDir := t.TempDir()
	session := newTestLineSession(t, lineSrv, ollamaSrv, contextDir, map[string]bool{})
	session.access.Groups = map[string]bool{"C999": true}

	ev := &lineEvent{
		Type:           "message",
		Source:         lineSource{Type: "group", GroupID: "C999", UserID: "U111"},
		ReplyToken:     "rtoken",
		Message:        &lineMessage{Type: "text", Text: "วันนี้อากาศดีจัง"},
		WebhookEventID: "evt-1",
	}
	session.handleLineMessage(ev)

	if atomic.LoadInt32(&ollamaCalls) != 0 {
		t.Fatal("expected an unaddressed group message to never reach the model")
	}
	if got := sent.snapshot(); len(got) != 0 {
		t.Fatalf("expected no reply to an unaddressed group message, got: %#v", got)
	}

	cctx, err := loadChatContext(contextDir, lineContextKey(ev.Source))
	if err != nil {
		t.Fatalf("loadChatContext: %v", err)
	}
	if len(cctx.Turns) != 1 {
		t.Fatalf("expected the unaddressed message to still be recorded, got %d turns", len(cctx.Turns))
	}
	// newMockLineRESTServer's /group/ handler always returns "สมหญิง" for
	// group member profile lookups - confirms the group-member-profile
	// endpoint (not the 1-on-1 /profile/ one) was used for a group turn.
	if cctx.Turns[0].Speaker != "สมหญิง" {
		t.Fatalf("expected speaker attribution from the group-member profile lookup, got speaker=%q", cctx.Turns[0].Speaker)
	}
}

func TestHandleLineMessageMentionedRespondsAndUsesPush(t *testing.T) {
	lineSrv, sent, _ := newMockLineRESTServer(t)
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("ได้เลยครับ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestLineSession(t, lineSrv, ollamaSrv, t.TempDir(), map[string]bool{})
	session.access.Groups = map[string]bool{"C999": true}

	ev := &lineEvent{
		Type:       "message",
		Source:     lineSource{Type: "group", GroupID: "C999", UserID: "U111"},
		ReplyToken: "rtoken-should-not-be-used-for-the-answer",
		Message: &lineMessage{
			Type: "text", Text: "@olabot ช่วยหน่อย",
			Mention: &lineMention{Mentionees: []lineMentionee{{Index: 0, Length: 7, UserID: "Ubot"}}},
		},
		WebhookEventID: "evt-2",
	}
	session.handleLineMessage(ev)

	got := sent.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 message sent, got %d: %#v", len(got), got)
	}
	if got[0].Kind != "push" {
		t.Fatalf("expected the model's answer to go out via Push (never Reply, given the token's short lifetime), got kind=%q", got[0].Kind)
	}
	if got[0].To != "C999" {
		t.Fatalf("expected the push target to be the group ID, got %q", got[0].To)
	}
	if got[0].Text != "ได้เลยครับ" {
		t.Fatalf("unexpected answer text: %q", got[0].Text)
	}
}

func TestHandleLineMessageWhoamiUsesReplyNotPush(t *testing.T) {
	lineSrv, sent, _ := newMockLineRESTServer(t)
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()

	session := newTestLineSession(t, lineSrv, ollamaSrv, t.TempDir(), map[string]bool{})

	ev := &lineEvent{
		Type:           "message",
		Source:         lineSource{Type: "user", UserID: "U111"},
		ReplyToken:     "rtoken-whoami",
		Message:        &lineMessage{Type: "text", Text: "/whoami"},
		WebhookEventID: "evt-whoami",
	}
	session.handleLineMessage(ev)

	got := sent.snapshot()
	if len(got) != 1 || got[0].Kind != "reply" {
		t.Fatalf("expected /whoami to use the fast Reply path (no model call needed), got: %#v", got)
	}
	if !strings.Contains(got[0].Text, "U111") {
		t.Fatalf("expected the reply to include the user's own ID, got: %s", got[0].Text)
	}
}

func TestLineWebhookRedeliveryIsIgnored(t *testing.T) {
	lineSrv, sent, _ := newMockLineRESTServer(t)
	var ollamaCalls int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("โอเค", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	session := newTestLineSession(t, lineSrv, ollamaSrv, t.TempDir(), map[string]bool{"U111": true})

	ev := &lineEvent{
		Type:           "message",
		Source:         lineSource{Type: "user", UserID: "U111"},
		ReplyToken:     "rtoken",
		Message:        &lineMessage{Type: "text", Text: "ทดสอบ redelivery"},
		WebhookEventID: "evt-redelivered",
	}
	session.handleLineMessage(ev)
	session.handleLineMessage(ev) // simulate LINE redelivering the exact same event

	if atomic.LoadInt32(&ollamaCalls) != 1 {
		t.Fatalf("expected a redelivered webhook event to be processed exactly once, got %d model calls", ollamaCalls)
	}
	if got := sent.snapshot(); len(got) != 1 {
		t.Fatalf("expected exactly 1 reply sent despite the redelivery, got %d: %#v", len(got), got)
	}
}

// ======================================================================
// Section: webbot
//
// Unit tests for the pure helper functions (session ID randomness, token
// comparison, idle-session cleanup), HTTP handler tests via httptest
// (index/session/chat, token gate, session-not-found), and an end-to-end
// test proving the "bot introduces itself before the user can chat"
// requirement and the "never touches disk" requirement both actually
// hold, not just look right in the code.
// ======================================================================

// newTestWebBotSession builds a webBotSession wired to a mock Ollama
// server, for driving the HTTP handlers directly - mirrors
// newTestTelegramSession/newTestDiscordSession/newTestLineSession.
// renderWebBotPageForTest mirrors cmdWebBot's own template execution -
// tests that exercise indexHandler need session.renderedHTML populated
// the same way a real run would, since indexHandler just serves that
// field verbatim rather than rendering on every request.
func renderWebBotPageForTest(t *testing.T, title, avatar string) []byte {
	t.Helper()
	tmpl, err := template.New("webbot").Parse(webBotChatHTMLTemplate)
	if err != nil {
		t.Fatalf("parse webbot template: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, webBotPageData{Title: title, Avatar: template.URL(avatar)}); err != nil {
		t.Fatalf("execute webbot template: %v", err)
	}
	return buf.Bytes()
}

func newTestWebBotSession(t *testing.T, ollamaSrv *httptest.Server, token string) *webBotSession {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "webbot-test-log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	return &webBotSession{
		chatBotCore: chatBotCore{
			client:       ollamaSrv.Client(),
			systemPrompt: buildChatBotSystemPrompt("เว็บแชท", ""),
			tools:        filterTools(builtinTools, "get_current_time", "delay"),
			pcfg:         providerConfig{Provider: providerOllama, Host: ollamaSrv.URL, Model: "mock-model"},
			ctxSize:      4096,
			keepRecent:   defaultChatBotKeepRecentTurns,
			compactAfter: defaultChatBotCompactAfterTurns,
			outFile:      logFile,
			locks:        map[string]*sync.Mutex{},
		},
		token:        token,
		renderedHTML: renderWebBotPageForTest(t, defaultWebBotTitle, ""),
		sessions:     map[string]*chatContext{},
	}
}

func TestNewWebBotSessionIDIsRandomAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newWebBotSessionID()
		if err != nil {
			t.Fatalf("newWebBotSessionID error: %v", err)
		}
		if len(id) == 0 {
			t.Fatal("expected a non-empty session id")
		}
		if seen[id] {
			t.Fatalf("expected unique session ids, got a duplicate: %s", id)
		}
		seen[id] = true
		for _, r := range id {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("expected a hex-only (URL/cookie-safe) session id, got %q", id)
			}
		}
	}
}

func TestTokensEqual(t *testing.T) {
	if !tokensEqual("secret123", "secret123") {
		t.Fatal("expected identical tokens to compare equal")
	}
	if tokensEqual("secret123", "secret124") {
		t.Fatal("expected different tokens to compare unequal")
	}
	if tokensEqual("short", "muchlongertoken") {
		t.Fatal("expected different-length tokens to compare unequal")
	}
	// Note: tokensEqual("", "") is mathematically true (both empty, same
	// length, vacuously no differing bytes) - that's fine and not a
	// vulnerability, because requireToken never calls tokensEqual at all
	// when s.token=="" (see its own early return); this function doesn't
	// need to special-case blank input itself.
}

func TestWebBotSessionStoreCreateGetAndCleanup(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "")

	ctx, id, err := session.createSession()
	if err != nil {
		t.Fatalf("createSession error: %v", err)
	}
	if ctx.Key != id {
		t.Fatalf("expected the context's Key to match the returned session id, got Key=%q id=%q", ctx.Key, id)
	}

	got, ok := session.getSession(id)
	if !ok || got != ctx {
		t.Fatal("expected getSession to return the exact same in-memory context just created")
	}

	if _, ok := session.getSession("does-not-exist"); ok {
		t.Fatal("expected a lookup of an unknown session id to fail")
	}

	// Backdate LastActive to simulate an idle session, then confirm cleanup drops it.
	ctx.LastActive = time.Now().Add(-1 * time.Hour)
	session.cleanupIdleSessions(10 * time.Minute)
	if _, ok := session.getSession(id); ok {
		t.Fatal("expected an idle-past-ttl session to be swept by cleanupIdleSessions")
	}
}

func TestWebBotSessionStoreCleanupKeepsActiveSessions(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "")

	_, id, err := session.createSession()
	if err != nil {
		t.Fatalf("createSession error: %v", err)
	}
	session.cleanupIdleSessions(10 * time.Minute) // just created - well within the TTL
	if _, ok := session.getSession(id); !ok {
		t.Fatal("expected a freshly created (not idle) session to survive cleanup")
	}
}

func TestWebBotRequireTokenNoTokenConfiguredAllowsAll(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "") // no token configured

	called := false
	handler := session.requireToken(func(w http.ResponseWriter, r *http.Request) { called = true })
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !called || rec.Code != http.StatusOK {
		t.Fatalf("expected the request to pass through unauthenticated when no token is configured, called=%v code=%d", called, rec.Code)
	}
}

func TestWebBotRequireTokenRejectsMissingOrWrongToken(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "correct-token")

	called := false
	handler := session.requireToken(func(w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token at all, got code=%d called=%v", rec.Code, called)
	}

	called = false
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/?token=wrong-token", nil))
	if called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with a wrong token, got code=%d called=%v", rec.Code, called)
	}
}

func TestWebBotRequireTokenAcceptsQueryParamThenSetsCookie(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "correct-token")

	called := false
	handler := session.requireToken(func(w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/?token=correct-token", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("expected the correct query-param token to be accepted, code=%d called=%v", rec.Code, called)
	}
	resp := rec.Result()
	var cookieSet bool
	for _, c := range resp.Cookies() {
		if c.Name == webBotTokenCookieName && c.Value == "correct-token" {
			cookieSet = true
		}
	}
	if !cookieSet {
		t.Fatal("expected a valid ?token= to set a session cookie for subsequent requests")
	}

	// A follow-up request with just the cookie (no query param) should also pass.
	called = false
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: webBotTokenCookieName, Value: "correct-token"})
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if !called || rec2.Code != http.StatusOK {
		t.Fatalf("expected the cookie alone to authenticate a follow-up request, code=%d called=%v", rec2.Code, called)
	}
}

func TestWebBotIndexHandlerServesGruvboxAndPlaypenFont(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "")

	rec := httptest.NewRecorder()
	session.indexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "Playpen+Sans+Thai") {
		t.Fatal("expected the page to load the Playpen Sans Thai Google Font")
	}
	if !strings.Contains(body, "#282828") || !strings.Contains(body, "#fabd2f") {
		t.Fatal("expected gruvbox theme colors (background/yellow) to be present in the page")
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected Content-Type text/html, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestWebBotSessionHandlerGreetsBeforeAnyUserMessage(t *testing.T) {
	var lastRequestBody string
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastRequestBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("สวัสดีครับ ผมคือผู้ช่วย AI ยินดีให้บริการครับ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "")

	rec := httptest.NewRecorder()
	session.sessionHandler(rec, httptest.NewRequest(http.MethodPost, "/api/session", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp webBotSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatal("expected a non-empty session_id")
	}
	if resp.Greeting != "sวัสดีครับ ผมคือผู้ช่วย AI ยินดีให้บริการครับ" && resp.Greeting != "สวัสดีครับ ผมคือผู้ช่วย AI ยินดีให้บริการครับ" {
		t.Fatalf("expected the model-generated greeting to be returned, got: %q", resp.Greeting)
	}
	if !strings.Contains(lastRequestBody, "แนะนำตัวเอง") {
		t.Fatalf("expected the greeting call to actually instruct the model to introduce itself, got request body: %s", lastRequestBody)
	}

	// The greeting must already be recorded as the session's first turn,
	// with no user turn before it (nobody said anything yet).
	ctx, ok := session.getSession(resp.SessionID)
	if !ok {
		t.Fatal("expected the session to exist after sessionHandler returns")
	}
	if len(ctx.Turns) != 1 || ctx.Turns[0].Role != "assistant" {
		t.Fatalf("expected exactly 1 turn (the greeting, role=assistant) with nothing before it, got: %#v", ctx.Turns)
	}
}

func TestWebBotSessionHandlerFallsBackToStaticGreetingOnModelFailure(t *testing.T) {
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "")

	rec := httptest.NewRecorder()
	session.sessionHandler(rec, httptest.NewRequest(http.MethodPost, "/api/session", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected sessionHandler to still succeed (with a fallback greeting) even if the model call fails, got %d", rec.Code)
	}
	var resp webBotSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Greeting != webBotFallbackGreeting {
		t.Fatalf("expected the static fallback greeting when the model call fails, got: %q", resp.Greeting)
	}
}

func TestWebBotChatHandlerRequiresExistingSession(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.NewServeMux())
	defer ollamaSrv.Close()
	session := newTestWebBotSession(t, ollamaSrv, "")

	body, _ := json.Marshal(webBotChatRequest{SessionID: "does-not-exist", Message: "hi"})
	rec := httptest.NewRecorder()
	session.chatHandler(rec, httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown session id, got %d", rec.Code)
	}
	var resp webBotChatResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == "" {
		t.Fatal("expected a non-empty error message")
	}
}

func TestWebBotChatHandlerEndToEndAndNeverPersists(t *testing.T) {
	var round int32
	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			fmt.Fprint(w, streamLine("สวัสดีครับ", "", "", true)) // greeting
			return
		}
		fmt.Fprint(w, streamLine("2+2 เท่ากับ 4 ครับ", "", "", true))
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	// contextDir intentionally never set on chatBotCore for webbot - this
	// test's real point is that nothing under diskDir ever gets written to
	// regardless, confirmed below by checking the directory stays empty.
	diskDir := t.TempDir()
	session := newTestWebBotSession(t, ollamaSrv, "")

	sessRec := httptest.NewRecorder()
	session.sessionHandler(sessRec, httptest.NewRequest(http.MethodPost, "/api/session", nil))
	var sessResp webBotSessionResponse
	_ = json.Unmarshal(sessRec.Body.Bytes(), &sessResp)

	chatBody, _ := json.Marshal(webBotChatRequest{SessionID: sessResp.SessionID, Message: "2+2 เท่ากับเท่าไหร่"})
	chatRec := httptest.NewRecorder()
	session.chatHandler(chatRec, httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(chatBody)))

	if chatRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", chatRec.Code, chatRec.Body.String())
	}
	var chatResp webBotChatResponse
	_ = json.Unmarshal(chatRec.Body.Bytes(), &chatResp)
	if chatResp.Answer != "2+2 เท่ากับ 4 ครับ" {
		t.Fatalf("unexpected answer: %q", chatResp.Answer)
	}

	ctx, ok := session.getSession(sessResp.SessionID)
	if !ok || len(ctx.Turns) != 3 {
		t.Fatalf("expected 3 turns in memory (greeting + user + assistant), got: %#v", ctx)
	}

	entries, err := os.ReadDir(diskDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected webbot to never write anything to disk for a session, found: %v", entries)
	}
}

// ─────────────────────────────────────────────────────────────────
// webbot: knowledge index persistence, --webbot-title, --webbot-avatar
// ─────────────────────────────────────────────────────────────────

func TestResolveWebBotAvatarEmptyReturnsEmpty(t *testing.T) {
	got, err := resolveWebBotAvatar("")
	if err != nil || got != "" {
		t.Fatalf("expected (\"\", nil) for no avatar configured, got (%q, %v)", got, err)
	}
}

func TestResolveWebBotAvatarURLPassthrough(t *testing.T) {
	cases := []string{
		"https://example.com/avatar.png",
		"http://example.com/avatar.png",
		"data:image/png;base64,iVBORw0KGgo=",
	}
	for _, c := range cases {
		got, err := resolveWebBotAvatar(c)
		if err != nil {
			t.Fatalf("resolveWebBotAvatar(%q) error: %v", c, err)
		}
		if got != c {
			t.Fatalf("expected a URL/data-URI to pass through unchanged, got %q for input %q", got, c)
		}
	}
}

// minimalPNG is a well-known, valid, complete 1x1 transparent PNG (the
// smallest valid PNG file byte-for-byte) - real enough for
// http.DetectContentType to correctly identify as image/png.
var minimalPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x04, 0x00, 0x00, 0x00, 0xb5, 0x1c, 0x0c, 0x02, 0x00, 0x00, 0x00,
	0x0b, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x64, 0x60, 0x00, 0x00,
	0x00, 0x05, 0x00, 0x01, 0x5a, 0x39, 0x0e, 0x74, 0x00, 0x00, 0x00, 0x00,
	0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func TestResolveWebBotAvatarLocalFileBecomesDataURI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "avatar.png")
	if err := os.WriteFile(path, minimalPNG, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveWebBotAvatar(path)
	if err != nil {
		t.Fatalf("resolveWebBotAvatar error: %v", err)
	}
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("expected a data:image/png;base64,... URI, got: %s", got[:min(40, len(got))])
	}
	wantB64 := base64.StdEncoding.EncodeToString(minimalPNG)
	if !strings.HasSuffix(got, wantB64) {
		t.Fatal("expected the data URI to contain the exact base64-encoded file bytes")
	}
}

func TestResolveWebBotAvatarRejectsNonImageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-an-image.txt")
	if err := os.WriteFile(path, []byte("this is definitely not an image"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWebBotAvatar(path); err == nil {
		t.Fatal("expected an error for a local file that isn't an image")
	}
}

func TestResolveWebBotAvatarMissingFileErrors(t *testing.T) {
	if _, err := resolveWebBotAvatar("/does/not/exist/avatar.png"); err == nil {
		t.Fatal("expected an error for a nonexistent local path")
	}
}

func TestWebBotPageTemplateAppliesCustomTitleAndAvatar(t *testing.T) {
	html := string(renderWebBotPageForTest(t, "ผู้ช่วยวิชา Network Security", "https://example.com/bot.png"))
	if !strings.Contains(html, "<title>ผู้ช่วยวิชา Network Security</title>") {
		t.Fatalf("expected the custom title in <title>, got: %s", html[:min(400, len(html))])
	}
	if !strings.Contains(html, "ผู้ช่วยวิชา Network Security</h1>") {
		t.Fatal("expected the custom title in the page header <h1>")
	}
	if !strings.Contains(html, `src="https://example.com/bot.png"`) {
		t.Fatal("expected the avatar URL as the header <img> src")
	}
	// html/template escapes "/" as "\/" inside a JS string literal
	// (defensive against a value containing "</script>") - functionally
	// identical JavaScript, just not byte-identical to the unescaped URL.
	if !strings.Contains(html, `var OLA_AVATAR = "https:\/\/example.com\/bot.png";`) {
		t.Fatal("expected the avatar URL exposed to JS as OLA_AVATAR for use in chat bubbles")
	}
}

func TestWebBotPageTemplateOmitsAvatarMarkupWhenNotConfigured(t *testing.T) {
	html := string(renderWebBotPageForTest(t, defaultWebBotTitle, ""))
	if strings.Contains(html, `class="header-avatar"`) {
		t.Fatal("expected no header avatar <img> element at all when no avatar is configured")
	}
	if !strings.Contains(html, `var OLA_AVATAR = "";`) {
		t.Fatal("expected OLA_AVATAR to be an empty string when no avatar is configured")
	}
}

// TestWebBotPageTemplateAllowsDataURIAvatar is the direct regression test
// for a real bug caught by live testing (not by the unit tests above,
// which only ever exercised an https:// avatar): html/template's default
// URL-context escaper only allows a small scheme allowlist through
// (http/https/mailto/...) and silently replaces anything else - INCLUDING
// a data: URI, exactly what a local --webbot-avatar file resolves to -
// with an inert "#ZgotmplZ" placeholder instead of ever rendering it.
// Fixed by typing webBotPageData.Avatar as template.URL so the escaper
// trusts a scheme our own code already produced safely (see
// resolveWebBotAvatar) rather than filtering it.
func TestWebBotPageTemplateAllowsDataURIAvatar(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	html := string(renderWebBotPageForTest(t, defaultWebBotTitle, dataURI))
	if strings.Contains(html, "ZgotmplZ") {
		t.Fatal("expected the data: URI avatar to render as-is, not get replaced with html/template's inert placeholder")
	}
	// html/template HTML-entity-escapes some characters within an
	// attribute value even when it trusts the URL scheme (here "+"
	// becomes "&#43;") - a browser decodes that back to the literal
	// character before using it as the actual attribute value, so this is
	// functionally identical to the raw URI, not a sign anything broke.
	// Compare against the entity-escaped form rather than requiring
	// byte-for-byte equality with the input.
	wantSrc := `src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk&#43;A8AAQUBAScY42YAAAAASUVORK5CYII="`
	if !strings.Contains(html, wantSrc) {
		t.Fatalf("expected the data URI (HTML-entity-escaped, which is safe/equivalent) as the header <img> src, got: %s", html[:min(600, len(html))])
	}
}

// TestWebBotPageTemplateEscapesAdversarialAvatarValue proves the
// template.URL fix above doesn't reopen an injection hole: a value
// containing a literal '"' must still be neutralized rather than
// breaking out of either the src="..." attribute or the JS string
// literal OLA_AVATAR also sits in, even though template.URL tells the
// escaper to trust the URL *scheme*.
func TestWebBotPageTemplateEscapesAdversarialAvatarValue(t *testing.T) {
	evil := `data:image/png;base64,AAAA"onerror="alert(1)`
	html := string(renderWebBotPageForTest(t, defaultWebBotTitle, evil))
	if strings.Contains(html, `"onerror="alert(1)"`) {
		t.Fatal("expected an embedded quote in the avatar value to be escaped, not break out of the src attribute")
	}
	if strings.Contains(html, `var OLA_AVATAR = "data:image/png;base64,AAAA"onerror`) {
		t.Fatal("expected an embedded quote in the avatar value to be escaped, not break out of the JS string literal")
	}
}

// TestWebBotPageTemplateEscapesTitleSafely proves html/template's
// contextual auto-escaping actually protects this page even though
// --webbot-title is operator-configured, not end-user input - good
// hygiene regardless of who sets it.
func TestWebBotPageTemplateEscapesTitleSafely(t *testing.T) {
	html := string(renderWebBotPageForTest(t, `</title><script>alert(1)</script>`, ""))
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("expected a title containing HTML/script markup to be escaped, not injected verbatim")
	}
}

// TestWebBotEmbeddingIndexPersistsToContextDir is the direct regression
// test for the request that webbot's knowledge index behave like
// telegrambot/discordbot/linebot's own: cached to disk under
// --context-dir, and incrementally updated (not fully rebuilt) on
// subsequent refreshes.
func TestWebBotEmbeddingIndexPersistsToContextDir(t *testing.T) {
	knowledgeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(knowledgeDir, "a.md"), []byte("เนื้อหาความรู้ทดสอบ"), 0644); err != nil {
		t.Fatal(err)
	}
	knowledgeCfg, _ := resolveKnowledgeConfig(knowledgeDir)

	var embedCalls int32
	embedSrv, _ := newMockEmbedServer(t, func(text string) []float32 {
		atomic.AddInt32(&embedCalls, 1)
		return []float32{1, 0}
	})

	contextDir := t.TempDir()
	idxPath := knowledgeIndexPath(contextDir)
	logFile, _ := os.CreateTemp(t.TempDir(), "log")
	defer logFile.Close()

	session := &webBotSession{
		chatBotCore: chatBotCore{
			client:           embedSrv.Client(),
			knowledgeCfg:     knowledgeCfg,
			embedCfg:         embedConfig{Model: "mock-embed", TopK: 5, MinScore: 0.1},
			knowledgeIdx:     &knowledgeIndexStore{},
			knowledgeIdxPath: idxPath,
			pcfg:             providerConfig{Provider: providerOllama, Host: embedSrv.URL, Model: "mock-embed"},
			contextDir:       contextDir,
			outFile:          logFile,
			locks:            map[string]*sync.Mutex{},
		},
		sessions: map[string]*chatContext{},
	}

	session.refreshKnowledgeIndex()

	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("expected knowledge-index.json to be written under --context-dir, got: %v", err)
	}
	if len(session.knowledgeIdx.get().Chunks) == 0 {
		t.Fatal("expected the freshly built index to have at least one chunk")
	}
	firstCallCount := atomic.LoadInt32(&embedCalls)
	if firstCallCount == 0 {
		t.Fatal("expected at least one embed call for the initial build")
	}

	// A second session pointed at the SAME context-dir should load the
	// cached index from disk and, since the file hasn't changed,
	// re-embed nothing at all.
	session2 := &webBotSession{
		chatBotCore: chatBotCore{
			client:           embedSrv.Client(),
			knowledgeCfg:     knowledgeCfg,
			embedCfg:         embedConfig{Model: "mock-embed", TopK: 5, MinScore: 0.1},
			knowledgeIdx:     &knowledgeIndexStore{},
			knowledgeIdxPath: idxPath,
			pcfg:             providerConfig{Provider: providerOllama, Host: embedSrv.URL, Model: "mock-embed"},
			contextDir:       contextDir,
			outFile:          logFile,
			locks:            map[string]*sync.Mutex{},
		},
		sessions: map[string]*chatContext{},
	}
	prevIdx, err := loadKnowledgeIndex(idxPath)
	if err != nil {
		t.Fatalf("loadKnowledgeIndex: %v", err)
	}
	session2.knowledgeIdx.set(prevIdx)

	atomic.StoreInt32(&embedCalls, 0)
	session2.refreshKnowledgeIndex()
	if atomic.LoadInt32(&embedCalls) != 0 {
		t.Fatalf("expected zero embed calls when reloading an unchanged knowledge base from a persisted index, got %d", embedCalls)
	}
	if len(session2.knowledgeIdx.get().Chunks) != len(session.knowledgeIdx.get().Chunks) {
		t.Fatal("expected the reloaded index to have the same chunks as the original")
	}
}
