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

	default:
		return "", fmt.Errorf("unknown Google Calendar operation: %s", d.IntegrationOp)
	}
}
