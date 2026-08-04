package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Meet REST API v2. Two things shape what's possible here:
//
//   - The meetings.space.created scope is principal-scoped: the token can only
//     reach spaces it created itself. So create_space is the entry point for
//     every other space operation, and a space made by hand in the Meet UI is
//     invisible to this node.
//   - Conference records, recordings and transcripts only exist after a meeting
//     has actually happened, and recording/transcription are Workspace features.
//     A meeting that was never recorded returns an empty list, not an error.

const meetAPI = "https://meet.googleapis.com/v2"

// meetSpaceName accepts either a full resource name or a bare meeting code and
// returns the path segment the API expects.
func meetSpaceName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "spaces/") {
		return v
	}
	// A meeting code (abc-mnop-xyz) is a valid space identifier on its own.
	return "spaces/" + v
}

func runGoogleMeet(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.MeetLimit, 25)
	space := func() string { return meetSpaceName(sub(d.MeetSpace)) }

	// spaceConfig is shared by create and update.
	config := func() map[string]any {
		cfg := map[string]any{}
		if v := strings.ToUpper(sub(d.MeetAccessType)); v != "" {
			cfg["accessType"] = v
		}
		if v := strings.ToUpper(sub(d.MeetModeration)); v != "" {
			cfg["moderation"] = v
		}
		return cfg
	}

	switch d.IntegrationOp {
	case "create_space":
		body := map[string]any{}
		if cfg := config(); len(cfg) > 0 {
			body["config"] = cfg
		}
		// The response carries meetingUri and meetingCode — the two things a
		// workflow actually wants to pass on to an email or a calendar event.
		return googleCall(ctx, token, http.MethodPost, meetAPI+"/spaces", body)

	case "get_space":
		if space() == "" {
			return "", fmt.Errorf("get_space needs a space name or meeting code")
		}
		return googleCall(ctx, token, http.MethodGet, meetAPI+"/"+space(), nil)

	case "update_space":
		cfg := config()
		if len(cfg) == 0 {
			return "", fmt.Errorf("update_space needs an access type or moderation setting to change")
		}
		mask := make([]string, 0, 2)
		if _, ok := cfg["accessType"]; ok {
			mask = append(mask, "config.accessType")
		}
		if _, ok := cfg["moderation"]; ok {
			mask = append(mask, "config.moderation")
		}
		return googleCall(ctx, token, http.MethodPatch,
			fmt.Sprintf("%s/%s?updateMask=%s", meetAPI, space(), url.QueryEscape(strings.Join(mask, ","))),
			map[string]any{"config": cfg})

	case "end_active_conference":
		if space() == "" {
			return "", fmt.Errorf("end_active_conference needs a space name or meeting code")
		}
		if _, err := googleCall(ctx, token, http.MethodPost,
			meetAPI+"/"+space()+":endActiveConference", map[string]any{}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"space":%q,"ended":true}`, space()), nil

	case "list_conference_records":
		q := url.Values{"pageSize": {fmt.Sprint(limit)}}
		// Without a filter this returns every conference the user can see, so
		// scoping to one space is the common case.
		if f := sub(d.MeetFilter); f != "" {
			q.Set("filter", f)
		} else if s := space(); s != "" {
			q.Set("filter", fmt.Sprintf("space.name=%q", s))
		}
		raw, err := googleCall(ctx, token, http.MethodGet, meetAPI+"/conferenceRecords?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_conference_record":
		return googleCall(ctx, token, http.MethodGet, meetAPI+"/"+sub(d.MeetConferenceRecord), nil)

	case "list_participants":
		if sub(d.MeetConferenceRecord) == "" {
			return "", fmt.Errorf("list_participants needs a conference record")
		}
		raw, err := googleCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/%s/participants?pageSize=%d", meetAPI, sub(d.MeetConferenceRecord), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_recordings":
		if sub(d.MeetConferenceRecord) == "" {
			return "", fmt.Errorf("list_recordings needs a conference record")
		}
		return googleCall(ctx, token, http.MethodGet,
			meetAPI+"/"+sub(d.MeetConferenceRecord)+"/recordings", nil)

	case "list_transcripts":
		if sub(d.MeetConferenceRecord) == "" {
			return "", fmt.Errorf("list_transcripts needs a conference record")
		}
		return googleCall(ctx, token, http.MethodGet,
			meetAPI+"/"+sub(d.MeetConferenceRecord)+"/transcripts", nil)

	case "get_transcript_text":
		// The transcript resource is only a handle; the spoken words live in its
		// entries, which is what a workflow wants to summarise. Stitch them into
		// one readable block rather than making the user loop.
		if sub(d.MeetTranscript) == "" {
			return "", fmt.Errorf("get_transcript_text needs a transcript name")
		}
		raw, err := googleCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/%s/entries?pageSize=%d", meetAPI, sub(d.MeetTranscript), intOr(d.MeetLimit, 200)), nil)
		if err != nil {
			return "", err
		}
		return meetTranscriptText(raw)

	case "list_transcript_entries":
		if sub(d.MeetTranscript) == "" {
			return "", fmt.Errorf("list_transcript_entries needs a transcript name")
		}
		raw, err := googleCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"%s/%s/entries?pageSize=%d", meetAPI, sub(d.MeetTranscript), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "":
		return "", fmt.Errorf("no Google Meet operation selected")
	}
	return "", fmt.Errorf("unsupported Google Meet operation: %s", d.IntegrationOp)
}

// meetTranscriptText flattens transcript entries into "Speaker: words" lines.
// Participant is a resource name rather than a display name, so entries from the
// same speaker are grouped under a short stable label instead.
func meetTranscriptText(raw string) (string, error) {
	var out struct {
		TranscriptEntries []struct {
			Participant string `json:"participant"`
			Text        string `json:"text"`
			StartTime   string `json:"startTime"`
		} `json:"transcriptEntries"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("could not read the transcript entries")
	}
	if len(out.TranscriptEntries) == 0 {
		return "", fmt.Errorf("this transcript has no entries yet — transcription may still be " +
			"processing, or the meeting was not transcribed")
	}
	speakers := map[string]string{}
	var b strings.Builder
	for _, e := range out.TranscriptEntries {
		label, ok := speakers[e.Participant]
		if !ok {
			label = fmt.Sprintf("Speaker %d", len(speakers)+1)
			speakers[e.Participant] = label
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	return truncateStr(b.String(), 12000), nil
}
