package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Google Calendar API v3.

const gcalBase = "https://www.googleapis.com/calendar/v3"

func runGoogleCalendar(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	cal := firstNonEmpty(sub(d.GCalCalendarId), "primary")

	switch d.IntegrationOp {
	case "list_events":
		q := url.Values{}
		q.Set("maxResults", fmt.Sprint(intOr(d.GCalLimit, 10)))
		q.Set("singleEvents", "true")
		q.Set("orderBy", "startTime")
		q.Set("timeMin", time.Now().Format(time.RFC3339))
		raw, err := googleCall(ctx, token, http.MethodGet,
			gcalBase+"/calendars/"+url.PathEscape(cal)+"/events?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Items []struct {
				ID       string `json:"id"`
				Summary  string `json:"summary"`
				HTMLLink string `json:"htmlLink"`
				Start    struct {
					DateTime string `json:"dateTime"`
					Date     string `json:"date"`
				} `json:"start"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Items))
		for _, e := range res.Items {
			out = append(out, map[string]any{
				"id":      e.ID,
				"summary": e.Summary,
				"start":   firstNonEmpty(e.Start.DateTime, e.Start.Date),
				"link":    e.HTMLLink,
			})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "create_event":
		event := map[string]any{
			"summary":     sub(d.GCalSummary),
			"description": sub(d.GCalDescription),
			"start":       map[string]string{"dateTime": sub(d.GCalStart)},
			"end":         map[string]string{"dateTime": sub(d.GCalEnd)},
		}
		if att := splitCSV(sub(d.GCalAttendees)); len(att) > 0 {
			arr := make([]map[string]string, 0, len(att))
			for _, a := range att {
				arr = append(arr, map[string]string{"email": a})
			}
			event["attendees"] = arr
		}
		raw, err := googleCall(ctx, token, http.MethodPost,
			gcalBase+"/calendars/"+url.PathEscape(cal)+"/events", event)
		if err != nil {
			return "", err
		}
		var created struct {
			ID       string `json:"id"`
			HTMLLink string `json:"htmlLink"`
		}
		_ = json.Unmarshal([]byte(raw), &created)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": created.ID, "link": created.HTMLLink})
		return string(b), nil

	case "delete_event":
		if _, err := googleCall(ctx, token, http.MethodDelete,
			gcalBase+"/calendars/"+url.PathEscape(cal)+"/events/"+url.PathEscape(sub(d.GCalEventId)), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":"deleted","id":%s}`, jsonString(sub(d.GCalEventId))), nil

	case "get_event":
		raw, err := googleCall(ctx, token, http.MethodGet,
			gcalBase+"/calendars/"+url.PathEscape(cal)+"/events/"+url.PathEscape(sub(d.GCalEventId)), nil)
		if err != nil {
			return "", err
		}
		var e struct {
			ID          string `json:"id"`
			Summary     string `json:"summary"`
			Description string `json:"description"`
			HTMLLink    string `json:"htmlLink"`
			Start       struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"start"`
			End struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"end"`
			Attendees []struct {
				Email          string `json:"email"`
				ResponseStatus string `json:"responseStatus"`
			} `json:"attendees"`
		}
		_ = json.Unmarshal([]byte(raw), &e)
		att := make([]map[string]any, 0, len(e.Attendees))
		for _, a := range e.Attendees {
			att = append(att, map[string]any{"email": a.Email, "response": a.ResponseStatus})
		}
		b, _ := json.Marshal(map[string]any{
			"id": e.ID, "summary": e.Summary, "description": e.Description,
			"start": firstNonEmpty(e.Start.DateTime, e.Start.Date),
			"end":   firstNonEmpty(e.End.DateTime, e.End.Date),
			"link":  e.HTMLLink, "attendees": att,
		})
		return string(b), nil

	case "update_event":
		patch := map[string]any{}
		if v := sub(d.GCalSummary); v != "" {
			patch["summary"] = v
		}
		if v := sub(d.GCalDescription); v != "" {
			patch["description"] = v
		}
		if v := sub(d.GCalStart); v != "" {
			patch["start"] = map[string]string{"dateTime": v}
		}
		if v := sub(d.GCalEnd); v != "" {
			patch["end"] = map[string]string{"dateTime": v}
		}
		if att := splitCSV(sub(d.GCalAttendees)); len(att) > 0 {
			arr := make([]map[string]string, 0, len(att))
			for _, a := range att {
				arr = append(arr, map[string]string{"email": a})
			}
			patch["attendees"] = arr
		}
		if len(patch) == 0 {
			return "", fmt.Errorf("Google Calendar: nothing to update — set a title, description, time, or attendees")
		}
		raw, err := googleCall(ctx, token, http.MethodPatch,
			gcalBase+"/calendars/"+url.PathEscape(cal)+"/events/"+url.PathEscape(sub(d.GCalEventId)), patch)
		if err != nil {
			return "", err
		}
		var updated struct {
			ID       string `json:"id"`
			HTMLLink string `json:"htmlLink"`
		}
		_ = json.Unmarshal([]byte(raw), &updated)
		b, _ := json.Marshal(map[string]any{"status": "updated", "id": updated.ID, "link": updated.HTMLLink})
		return string(b), nil

	case "quick_add":
		q := url.Values{"text": {sub(d.GCalText)}}
		raw, err := googleCall(ctx, token, http.MethodPost,
			gcalBase+"/calendars/"+url.PathEscape(cal)+"/events/quickAdd?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var created struct {
			ID       string `json:"id"`
			Summary  string `json:"summary"`
			HTMLLink string `json:"htmlLink"`
		}
		_ = json.Unmarshal([]byte(raw), &created)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": created.ID, "summary": created.Summary, "link": created.HTMLLink})
		return string(b), nil

	case "list_calendars":
		raw, err := googleCall(ctx, token, http.MethodGet, gcalBase+"/users/me/calendarList?maxResults=50", nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Items []struct {
				ID      string `json:"id"`
				Summary string `json:"summary"`
				Primary bool   `json:"primary"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Items))
		for _, c := range res.Items {
			out = append(out, map[string]any{"id": c.ID, "name": c.Summary, "primary": c.Primary})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "find_free_time":
		start, end := sub(d.GCalStart), sub(d.GCalEnd)
		if start == "" {
			start = time.Now().Format(time.RFC3339)
		}
		if end == "" {
			end = time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339)
		}
		raw, err := googleCall(ctx, token, http.MethodPost, gcalBase+"/freeBusy", map[string]any{
			"timeMin": start, "timeMax": end,
			"items": []map[string]string{{"id": cal}},
		})
		if err != nil {
			return "", err
		}
		var res struct {
			Calendars map[string]struct {
				Busy []struct {
					Start string `json:"start"`
					End   string `json:"end"`
				} `json:"busy"`
			} `json:"calendars"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		busy := []map[string]string{}
		for _, c := range res.Calendars {
			for _, w := range c.Busy {
				busy = append(busy, map[string]string{"start": w.Start, "end": w.End})
			}
		}
		b, _ := json.Marshal(map[string]any{"window": map[string]string{"start": start, "end": end}, "busy": busy})
		return string(b), nil

	case "respond_to_event":
		resp := sub(d.GCalResponse)
		switch resp {
		case "accepted", "declined", "tentative":
		default:
			return "", fmt.Errorf("Google Calendar: response must be accepted, declined, or tentative")
		}
		// Find the authenticated user's attendee entry and patch its status.
		evPath := gcalBase + "/calendars/" + url.PathEscape(cal) + "/events/" + url.PathEscape(sub(d.GCalEventId))
		raw, err := googleCall(ctx, token, http.MethodGet, evPath, nil)
		if err != nil {
			return "", err
		}
		var ev struct {
			Attendees []map[string]any `json:"attendees"`
		}
		_ = json.Unmarshal([]byte(raw), &ev)
		found := false
		for _, a := range ev.Attendees {
			if self, _ := a["self"].(bool); self {
				a["responseStatus"] = resp
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("Google Calendar: you are not an attendee of this event")
		}
		if _, err := googleCall(ctx, token, http.MethodPatch, evPath, map[string]any{"attendees": ev.Attendees}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": resp})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Google Calendar operation: %s", d.IntegrationOp)
	}
}
