package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

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
