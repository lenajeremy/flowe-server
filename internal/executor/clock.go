package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ── Clock ─────────────────────────────────────────────────────────
//
// Models have no clock. Left to guess, they fall back on their training
// cutoff — so "yesterday", "this quarter" or "is this order 3 days old?"
// silently resolve against the wrong year. Two mechanisms cover it:
//
//   1. ClockLine is stitched into the system prompt of every LLM call, so
//      even a single-shot completion (the llm node, a branch condition)
//      knows the date without a round-trip.
//   2. ClockTool* implements a get_current_time tool for the call sites that
//      run a tool loop, so a long session can re-read the clock and convert
//      into a named timezone instead of doing offset arithmetic in its head.
//
// Nothing here is a source of truth for scheduling — the scheduler owns that.
// This is only about telling the model what "now" is.

// clockNow is a seam for tests; production always reads the real clock.
var clockNow = time.Now

// ClockLine states the current instant unambiguously: RFC3339 in UTC (the
// canonical form every downstream API wants) plus the weekday, which models
// otherwise miscompute from the date.
func ClockLine(now time.Time) string {
	u := now.UTC()
	return fmt.Sprintf("Current date and time: %s (%s, week %d of %d). Unix: %d.",
		u.Format("2006-01-02 15:04:05 UTC"), u.Weekday(), isoWeek(u), u.Year(), u.Unix())
}

func isoWeek(t time.Time) int { _, w := t.ISOWeek(); return w }

// WithClock stitches the clock into a system prompt. The clock goes last so
// it is the freshest thing in context, and it is labelled as authoritative
// because a model that half-trusts it will still reach for its cutoff.
//
// Use this for single-shot completions, which have no tools to call.
func WithClock(system string) string { return appendClock(system, false) }

// WithClockAndTool is WithClock for call sites that also register the
// get_current_time tool. Only these mention the tool — promising a tool that
// isn't in the request just invites the model to hallucinate a call.
//
// It has no production callers left: every tool-loop call site now sends
// ClockBlock in its own block instead, because concatenating here makes the
// prefix unique per request and no prompt cache can hit. Reach for ClockBlock,
// not this, when wiring up a new loop.
func WithClockAndTool(system string) string { return appendClock(system, true) }

// ClockBlock is the clock on its own, for call sites that keep it in a
// separate block instead of concatenating it onto the prompt.
//
// The clock carries a Unix second, so it is different on every request. Glued
// to the end of a system prompt it makes the whole prefix unique each time,
// and a prompt cache — which matches on that prefix — can never hit. Held
// apart and placed last, the static instructions in front of it cache and only
// the clock is re-read per turn.
func ClockBlock(hasTool bool) string {
	clock := ClockLine(clockNow()) +
		"\nTreat this as the authoritative current time — never rely on your training cutoff for the date." +
		" Times without a stated zone are UTC."
	if hasTool {
		clock += " Call " + ClockToolName + " if you need another timezone or a fresher reading."
	}
	return clock
}

func appendClock(system string, hasTool bool) string {
	clock := ClockBlock(hasTool)
	if strings.TrimSpace(system) == "" {
		return clock
	}
	return system + "\n\n" + clock
}

// ── get_current_time tool ─────────────────────────────────────────

const (
	ClockToolName = "get_current_time"
	ClockToolDesc = "Get the current date and time. Call this before any reasoning that depends on " +
		"today's date (relative dates like \"yesterday\" or \"in 3 days\", age checks, scheduling). " +
		"Optionally pass an IANA timezone to convert into."
)

// ClockToolSchema is the JSON Schema for the tool's input, shared by every
// provider (Anthropic's input_schema and OpenAI's parameters take the same shape).
func ClockToolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timezone": map[string]any{
				"type": "string",
				"description": "Optional IANA timezone name, e.g. \"Europe/Dublin\" or " +
					"\"America/New_York\". Defaults to UTC.",
			},
		},
	}
}

// CurrentTime answers the tool call. An unknown timezone is reported rather
// than silently falling back to UTC — a wrong-but-plausible time is worse
// than a model that knows it has to ask.
func CurrentTime(tz string) string {
	now := clockNow()
	out := map[string]any{
		"utc":     now.UTC().Format(time.RFC3339),
		"weekday": now.UTC().Weekday().String(),
		"unix":    now.Unix(),
	}
	if tz = strings.TrimSpace(tz); tz != "" && !strings.EqualFold(tz, "UTC") {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			out["error"] = fmt.Sprintf("unknown timezone %q — use an IANA name like Europe/Dublin. "+
				"The utc field above is still correct.", tz)
		} else {
			local := now.In(loc)
			_, offset := local.Zone()
			out["timezone"] = tz
			out["local"] = local.Format(time.RFC3339)
			out["utc_offset_hours"] = float64(offset) / 3600
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
