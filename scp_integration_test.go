package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

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
		"--scp-hosts", "backup=moo@testhost=/",
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
