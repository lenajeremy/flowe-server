package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fixed instant: Tuesday 2026-03-10 14:30:00 UTC, ISO week 11
func withFixedClock(t *testing.T) time.Time {
	t.Helper()
	fixed := time.Date(2026, 3, 10, 14, 30, 0, 0, time.UTC)
	old := clockNow
	clockNow = func() time.Time { return fixed }
	t.Cleanup(func() { clockNow = old })
	return fixed
}

func TestClockLine(t *testing.T) {
	fixed := withFixedClock(t)
	got := ClockLine(fixed)
	for _, want := range []string{"2026-03-10 14:30:00 UTC", "Tuesday", "week 11 of 2026"} {
		if !strings.Contains(got, want) {
			t.Errorf("ClockLine() = %q, missing %q", got, want)
		}
	}
}

func TestClockLineConvertsToUTC(t *testing.T) {
	// A non-UTC instant must be reported in UTC, not as its wall-clock reading.
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	tokyoMidnight := time.Date(2026, 3, 11, 0, 30, 0, 0, loc) // = 2026-03-10 15:30 UTC
	got := ClockLine(tokyoMidnight)
	if !strings.Contains(got, "2026-03-10 15:30:00 UTC") {
		t.Errorf("expected the UTC instant, got %q", got)
	}
	if strings.Contains(got, "2026-03-11") {
		t.Errorf("leaked the local date into a UTC field: %q", got)
	}
}

func TestWithClockPreservesSystemPrompt(t *testing.T) {
	withFixedClock(t)
	const sys = "You are a boolean condition evaluator."
	got := WithClock(sys)
	if !strings.HasPrefix(got, sys) {
		t.Errorf("original prompt must survive verbatim at the front, got %q", got)
	}
	if !strings.Contains(got, "2026-03-10") {
		t.Error("clock not appended")
	}
}

// A single-shot call has no tools registered, so it must not be told to call
// one — that is an invitation to hallucinate a tool_use block.
func TestWithClockOnlyMentionsToolWhenToolExists(t *testing.T) {
	withFixedClock(t)
	if got := WithClock("sys"); strings.Contains(got, ClockToolName) {
		t.Errorf("WithClock must not advertise the tool: %q", got)
	}
	if got := WithClockAndTool("sys"); !strings.Contains(got, ClockToolName) {
		t.Errorf("WithClockAndTool should advertise the tool: %q", got)
	}
	// Both must still state the time.
	for name, got := range map[string]string{"WithClock": WithClock("sys"), "WithClockAndTool": WithClockAndTool("sys")} {
		if !strings.Contains(got, "2026-03-10") {
			t.Errorf("%s dropped the clock: %q", name, got)
		}
	}
}

func TestWithClockOnEmptyPrompt(t *testing.T) {
	withFixedClock(t)
	got := WithClock("   ")
	if strings.HasPrefix(got, " ") || strings.Contains(got, "\n\n\n") {
		t.Errorf("blank prompt should not leave padding: %q", got)
	}
	if !strings.Contains(got, "2026-03-10") {
		t.Error("clock missing")
	}
}

func TestCurrentTimeDefaultsToUTC(t *testing.T) {
	withFixedClock(t)
	var out map[string]any
	if err := json.Unmarshal([]byte(CurrentTime("")), &out); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if out["utc"] != "2026-03-10T14:30:00Z" {
		t.Errorf("utc = %v", out["utc"])
	}
	if out["weekday"] != "Tuesday" {
		t.Errorf("weekday = %v", out["weekday"])
	}
	if _, ok := out["local"]; ok {
		t.Error("no timezone was requested, so no local reading should appear")
	}
}

func TestCurrentTimeConvertsTimezone(t *testing.T) {
	withFixedClock(t)
	var out map[string]any
	if err := json.Unmarshal([]byte(CurrentTime("Asia/Tokyo")), &out); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, bad := out["error"]; bad {
		t.Skipf("tzdata unavailable: %v", out["error"])
	}
	// Tokyo is UTC+9 → 23:30 the same day
	if got, _ := out["local"].(string); !strings.HasPrefix(got, "2026-03-10T23:30:00") {
		t.Errorf("local = %v, want 2026-03-10T23:30:00+09:00", out["local"])
	}
	if out["utc_offset_hours"] != float64(9) {
		t.Errorf("utc_offset_hours = %v, want 9", out["utc_offset_hours"])
	}
	// The UTC field must stay correct regardless of the conversion.
	if out["utc"] != "2026-03-10T14:30:00Z" {
		t.Errorf("utc corrupted by conversion: %v", out["utc"])
	}
}

func TestCurrentTimeRejectsUnknownTimezone(t *testing.T) {
	withFixedClock(t)
	var out map[string]any
	if err := json.Unmarshal([]byte(CurrentTime("Mars/Olympus_Mons")), &out); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, ok := out["error"]; !ok {
		t.Error("an unknown timezone must be reported, not silently ignored")
	}
	if _, ok := out["local"]; ok {
		t.Error("must not invent a local time for an unknown zone")
	}
	// Still useful: the caller gets a correct UTC reading alongside the error.
	if out["utc"] != "2026-03-10T14:30:00Z" {
		t.Errorf("utc = %v", out["utc"])
	}
}

func TestCurrentTimeUTCAliasSkipsConversion(t *testing.T) {
	withFixedClock(t)
	var out map[string]any
	_ = json.Unmarshal([]byte(CurrentTime("utc")), &out)
	if _, ok := out["error"]; ok {
		t.Errorf(`"utc" should be accepted, got %v`, out["error"])
	}
}
