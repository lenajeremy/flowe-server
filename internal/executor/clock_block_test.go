package executor

import (
	"strings"
	"testing"
)

// The clock, split out so it can be kept apart from a cached prompt.
//
// The split has to be lossless: the model must receive the same words in the
// same order whether a call site concatenates (WithClockAndTool) or places the
// two in separate blocks (ClockBlock). If they ever drift, one of the two call
// sites is quietly running on different instructions from the other.

func TestSplitClockSaysExactlyWhatTheConcatenatedOneSaid(t *testing.T) {
	withFixedClock(t)
	const sys = "You are a workflow builder."

	for _, hasTool := range []bool{true, false} {
		joined := appendClock(sys, hasTool)
		want := sys + "\n\n" + ClockBlock(hasTool)
		if joined != want {
			t.Errorf("hasTool=%v: the split form is not the concatenated form\n concat: %q\n split:  %q",
				hasTool, joined, want)
		}
	}
}

func TestClockBlockCarriesTheClockAndNothingElse(t *testing.T) {
	withFixedClock(t)
	block := ClockBlock(true)
	if !strings.Contains(block, "2026-03-10") {
		t.Errorf("no clock in the clock block: %q", block)
	}
	if !strings.Contains(block, ClockToolName) {
		t.Errorf("hasTool=true must advertise the tool: %q", block)
	}
	// The whole point of the split is that the prompt is NOT in here — if it
	// were, a caller placing this after the cache breakpoint would be paying to
	// re-read the prompt every turn.
	if strings.Contains(ClockBlock(false), ClockToolName) {
		t.Error("hasTool=false advertised a tool that is not registered")
	}
}
