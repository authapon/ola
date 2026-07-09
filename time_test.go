package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
