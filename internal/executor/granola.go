package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Granola's public API, which is small on purpose: notes in, notes out. There is
// no write side, no folders, no people — three operations exhaust it.
//
// Two behaviours worth knowing:
//   - Only notes that already have an AI summary and transcript are returned. One
//     still processing is a 404, not an empty result.
//   - Transcripts come back as structured segments whose speaker field differs by
//     platform (macOS uses speaker.source, iOS uses diarization_label), so
//     get_transcript flattens them rather than making a workflow deal with both.

const granolaAPI = "https://public-api.granola.ai/v1"

func granolaCall(ctx context.Context, key, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, granolaAPI+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("granola request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("Granola has no such note, or its summary is still being " +
			"generated — the API only returns notes that already have a summary and transcript")
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", fmt.Errorf("Granola rate limit reached (5 requests/second) — space these calls out")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("Granola rejected the API key — it may have been revoked, or the " +
			"workspace plan may not include API access (Business or Enterprise only)")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", fmt.Errorf("Granola API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func runGranola(ctx context.Context, key string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "list_notes":
		q := url.Values{}
		// created_after is how you scope a digest to "since the last run".
		if v := sub(d.GranolaCreatedAfter); v != "" {
			q.Set("created_after", v)
		}
		if v := sub(d.GranolaCursor); v != "" {
			q.Set("cursor", v)
		}
		if n := intOr(d.GranolaLimit, 0); n > 0 {
			q.Set("limit", fmt.Sprint(n))
		}
		path := "/notes"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		raw, err := granolaCall(ctx, key, path)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_note":
		if sub(d.GranolaNoteId) == "" {
			return "", fmt.Errorf("get_note needs a note ID")
		}
		raw, err := granolaCall(ctx, key, "/notes/"+url.PathEscape(sub(d.GranolaNoteId)))
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "get_transcript":
		if sub(d.GranolaNoteId) == "" {
			return "", fmt.Errorf("get_transcript needs a note ID")
		}
		raw, err := granolaCall(ctx, key,
			"/notes/"+url.PathEscape(sub(d.GranolaNoteId))+"?include=transcript")
		if err != nil {
			return "", err
		}
		return granolaTranscriptText(raw)

	case "":
		return "", fmt.Errorf("no Granola operation selected")
	}
	return "", fmt.Errorf("unsupported Granola operation: %s", d.IntegrationOp)
}

// granolaTranscriptText flattens a note's transcript into "Speaker: words" lines.
// The speaker label lives in a different field depending on which platform
// recorded the meeting, so both are checked before falling back to a placeholder.
func granolaTranscriptText(raw string) (string, error) {
	var note struct {
		Title      string `json:"title"`
		Transcript []struct {
			Text    string `json:"text"`
			Speaker struct {
				Source string `json:"source"`
				Name   string `json:"name"`
			} `json:"speaker"`
			DiarizationLabel string `json:"diarization_label"`
		} `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(raw), &note); err != nil {
		return "", fmt.Errorf("could not read the Granola transcript")
	}
	if len(note.Transcript) == 0 {
		return "", fmt.Errorf("this note has no transcript — it may still be processing")
	}
	var b strings.Builder
	if note.Title != "" {
		b.WriteString(note.Title)
		b.WriteString("\n\n")
	}
	for _, seg := range note.Transcript {
		speaker := firstNonEmpty(seg.Speaker.Name, seg.Speaker.Source, seg.DiarizationLabel, "Speaker")
		b.WriteString(speaker)
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(seg.Text))
		b.WriteString("\n")
	}
	return truncateStr(b.String(), 14000), nil
}
