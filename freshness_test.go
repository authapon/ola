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
