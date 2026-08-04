package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Calendly API v2.
//
// Everything is addressed by URI rather than id: a user is
// https://api.calendly.com/users/ABC123, and most list endpoints require the
// user or organization URI as a query parameter. Those two URIs come from
// GET /users/me, so calendlyIdentity resolves them once and the ops that need
// them fall back to it — a workflow should not have to paste a URI it cannot
// know in advance.
//
// The API used to be read-mostly. It now has a Scheduling API: POST /invitees
// books a meeting outright, without a redirect or an embedded Calendly UI. That
// endpoint needs a paid Calendly plan, which is the one limit worth stating up
// front because it fails at runtime rather than at connect time.

const calendlyAPI = "https://api.calendly.com"

func calendlyCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, calendlyAPI+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("calendly request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Title   string `json:"title"`
			Message string `json:"message"`
			Details []struct {
				Parameter string `json:"parameter"`
				Message   string `json:"message"`
			} `json:"details"`
		}
		var parts []string
		if json.Unmarshal(raw, &e) == nil {
			if e.Message != "" {
				parts = append(parts, e.Message)
			} else if e.Title != "" {
				parts = append(parts, e.Title)
			}
			for _, x := range e.Details {
				parts = append(parts, x.Parameter+": "+x.Message)
			}
		}
		msg := strings.Join(parts, "; ")
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		switch resp.StatusCode {
		case http.StatusForbidden:
			msg += " — booking through the API needs a paid Calendly plan, and some " +
				"endpoints are limited to organization admins"
		case http.StatusNotFound:
			msg += " — Calendly identifies things by full URI, so check the value looks " +
				"like https://api.calendly.com/…"
		}
		return "", fmt.Errorf("Calendly API error (%d): %s", resp.StatusCode, msg)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// calendlyIdentity returns the current user's URI and their organization's URI,
// which most list endpoints require as a filter.
func calendlyIdentity(ctx context.Context, token string) (userURI, orgURI string, err error) {
	raw, err := calendlyCall(ctx, token, http.MethodGet, "/users/me", nil)
	if err != nil {
		return "", "", err
	}
	var me struct {
		Resource struct {
			URI          string `json:"uri"`
			Organization string `json:"current_organization"`
		} `json:"resource"`
	}
	if json.Unmarshal([]byte(raw), &me) != nil || me.Resource.URI == "" {
		return "", "", fmt.Errorf("could not read the connected Calendly account")
	}
	return me.Resource.URI, me.Resource.Organization, nil
}

func runCalendly(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.CalendlyLimit, 25)

	// Resolve whichever URI the node did not supply, lazily — only the ops that
	// need one pay for the extra call.
	scope := func() (userURI, orgURI string, err error) {
		userURI, orgURI = sub(d.CalendlyUser), sub(d.CalendlyOrganization)
		if userURI != "" && orgURI != "" {
			return userURI, orgURI, nil
		}
		u, o, err := calendlyIdentity(ctx, token)
		if err != nil {
			return "", "", err
		}
		return firstNonEmpty(userURI, u), firstNonEmpty(orgURI, o), nil
	}

	switch d.IntegrationOp {
	case "get_current_user":
		return calendlyCall(ctx, token, http.MethodGet, "/users/me", nil)

	case "get_user":
		if sub(d.CalendlyUser) == "" {
			return "", fmt.Errorf("get_user needs a user URI")
		}
		return calendlyCall(ctx, token, http.MethodGet, "/users/"+calendlyUUID(sub(d.CalendlyUser)), nil)

	// ---- event types ----
	case "list_event_types":
		userURI, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		q := url.Values{"count": {fmt.Sprint(limit)}}
		// Organization scope lists every member's event types; user scope is the
		// narrower and more common case.
		if strings.EqualFold(sub(d.CalendlyScope), "organization") {
			q.Set("organization", orgURI)
		} else {
			q.Set("user", userURI)
		}
		raw, err := calendlyCall(ctx, token, http.MethodGet, "/event_types?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_event_type":
		if sub(d.CalendlyEventType) == "" {
			return "", fmt.Errorf("get_event_type needs an event type URI")
		}
		return calendlyCall(ctx, token, http.MethodGet,
			"/event_types/"+calendlyUUID(sub(d.CalendlyEventType)), nil)

	case "list_available_times":
		// The prerequisite for booking: Calendly only accepts a start time that
		// appears here, and the window may not exceed 7 days.
		if sub(d.CalendlyEventType) == "" {
			return "", fmt.Errorf("list_available_times needs an event type URI")
		}
		if sub(d.CalendlyStartTime) == "" || sub(d.CalendlyEndTime) == "" {
			return "", fmt.Errorf("list_available_times needs a start and end time (at most 7 days apart)")
		}
		q := url.Values{
			"event_type": {sub(d.CalendlyEventType)},
			"start_time": {sub(d.CalendlyStartTime)},
			"end_time":   {sub(d.CalendlyEndTime)},
		}
		return calendlyCall(ctx, token, http.MethodGet, "/event_type_available_times?"+q.Encode(), nil)

	// ---- booking ----
	case "create_booking":
		return calendlyBook(ctx, token, d, sub)

	case "create_scheduling_link":
		// A single-use link, for when a human should pick the slot instead.
		if sub(d.CalendlyEventType) == "" {
			return "", fmt.Errorf("create_scheduling_link needs an event type URI")
		}
		return calendlyCall(ctx, token, http.MethodPost, "/scheduling_links", map[string]any{
			"max_event_count": 1,
			"owner":           sub(d.CalendlyEventType),
			"owner_type":      "EventType",
		})

	// ---- scheduled events ----
	case "list_scheduled_events":
		userURI, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		q := url.Values{"count": {fmt.Sprint(limit)}, "sort": {"start_time:asc"}}
		if strings.EqualFold(sub(d.CalendlyScope), "organization") {
			q.Set("organization", orgURI)
		} else {
			q.Set("user", userURI)
		}
		if v := sub(d.CalendlyStatus); v != "" {
			q.Set("status", v)
		}
		if v := sub(d.CalendlyStartTime); v != "" {
			q.Set("min_start_time", v)
		}
		if v := sub(d.CalendlyEndTime); v != "" {
			q.Set("max_start_time", v)
		}
		raw, err := calendlyCall(ctx, token, http.MethodGet, "/scheduled_events?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_scheduled_event":
		if sub(d.CalendlyEvent) == "" {
			return "", fmt.Errorf("get_scheduled_event needs an event URI")
		}
		return calendlyCall(ctx, token, http.MethodGet,
			"/scheduled_events/"+calendlyUUID(sub(d.CalendlyEvent)), nil)

	case "cancel_event":
		if sub(d.CalendlyEvent) == "" {
			return "", fmt.Errorf("cancel_event needs an event URI")
		}
		payload := map[string]any{}
		if v := sub(d.CalendlyReason); v != "" {
			payload["reason"] = v
		}
		return calendlyCall(ctx, token, http.MethodPost,
			"/scheduled_events/"+calendlyUUID(sub(d.CalendlyEvent))+"/cancellation", payload)

	// ---- invitees ----
	case "list_invitees":
		if sub(d.CalendlyEvent) == "" {
			return "", fmt.Errorf("list_invitees needs an event URI")
		}
		q := url.Values{"count": {fmt.Sprint(limit)}}
		if v := sub(d.CalendlyEmail); v != "" {
			q.Set("email", v)
		}
		if v := sub(d.CalendlyStatus); v != "" {
			q.Set("status", v)
		}
		return calendlyCall(ctx, token, http.MethodGet,
			"/scheduled_events/"+calendlyUUID(sub(d.CalendlyEvent))+"/invitees?"+q.Encode(), nil)

	case "get_invitee":
		if sub(d.CalendlyEvent) == "" || sub(d.CalendlyInvitee) == "" {
			return "", fmt.Errorf("get_invitee needs an event URI and an invitee URI")
		}
		return calendlyCall(ctx, token, http.MethodGet, fmt.Sprintf("/scheduled_events/%s/invitees/%s",
			calendlyUUID(sub(d.CalendlyEvent)), calendlyUUID(sub(d.CalendlyInvitee))), nil)

	case "mark_no_show":
		if sub(d.CalendlyInvitee) == "" {
			return "", fmt.Errorf("mark_no_show needs an invitee URI")
		}
		return calendlyCall(ctx, token, http.MethodPost, "/invitee_no_shows",
			map[string]any{"invitee": sub(d.CalendlyInvitee)})

	case "undo_no_show":
		if sub(d.CalendlyNoShow) == "" {
			return "", fmt.Errorf("undo_no_show needs the no-show URI returned by mark_no_show")
		}
		return calendlyCall(ctx, token, http.MethodDelete,
			"/invitee_no_shows/"+calendlyUUID(sub(d.CalendlyNoShow)), nil)

	// ---- availability ----
	case "list_availability_schedules":
		userURI, _, err := scope()
		if err != nil {
			return "", err
		}
		return calendlyCall(ctx, token, http.MethodGet,
			"/user_availability_schedules?user="+url.QueryEscape(userURI), nil)

	case "list_busy_times":
		userURI, _, err := scope()
		if err != nil {
			return "", err
		}
		if sub(d.CalendlyStartTime) == "" || sub(d.CalendlyEndTime) == "" {
			return "", fmt.Errorf("list_busy_times needs a start and end time (at most 7 days apart)")
		}
		q := url.Values{
			"user":       {userURI},
			"start_time": {sub(d.CalendlyStartTime)},
			"end_time":   {sub(d.CalendlyEndTime)},
		}
		return calendlyCall(ctx, token, http.MethodGet, "/user_busy_times?"+q.Encode(), nil)

	// ---- organization ----
	case "list_memberships":
		_, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		return calendlyCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"/organization_memberships?organization=%s&count=%d", url.QueryEscape(orgURI), limit), nil)

	case "remove_member":
		if sub(d.CalendlyMembership) == "" {
			return "", fmt.Errorf("remove_member needs a membership URI from list_memberships")
		}
		return calendlyCall(ctx, token, http.MethodDelete,
			"/organization_memberships/"+calendlyUUID(sub(d.CalendlyMembership)), nil)

	case "invite_to_organization":
		_, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		if sub(d.CalendlyEmail) == "" {
			return "", fmt.Errorf("invite_to_organization needs an email address")
		}
		return calendlyCall(ctx, token, http.MethodPost,
			"/organizations/"+calendlyUUID(orgURI)+"/invitations",
			map[string]any{"email": sub(d.CalendlyEmail)})

	case "list_invitations":
		_, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		return calendlyCall(ctx, token, http.MethodGet,
			"/organizations/"+calendlyUUID(orgURI)+"/invitations", nil)

	// ---- routing forms ----
	case "list_routing_forms":
		_, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		return calendlyCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"/routing_forms?organization=%s&count=%d", url.QueryEscape(orgURI), limit), nil)

	case "list_routing_form_submissions":
		if sub(d.CalendlyRoutingForm) == "" {
			return "", fmt.Errorf("list_routing_form_submissions needs a routing form URI")
		}
		return calendlyCall(ctx, token, http.MethodGet, fmt.Sprintf(
			"/routing_form_submissions?form=%s&count=%d",
			url.QueryEscape(sub(d.CalendlyRoutingForm)), limit), nil)

	// ---- webhooks ----
	case "list_webhooks":
		userURI, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		q := url.Values{"organization": {orgURI}, "count": {fmt.Sprint(limit)}}
		if strings.EqualFold(sub(d.CalendlyScope), "organization") {
			q.Set("scope", "organization")
		} else {
			q.Set("scope", "user")
			q.Set("user", userURI)
		}
		return calendlyCall(ctx, token, http.MethodGet, "/webhook_subscriptions?"+q.Encode(), nil)

	case "create_webhook":
		userURI, orgURI, err := scope()
		if err != nil {
			return "", err
		}
		if sub(d.CalendlyUrl) == "" {
			return "", fmt.Errorf("create_webhook needs a callback URL")
		}
		events := splitCSV(sub(d.CalendlyEvents))
		if len(events) == 0 {
			events = []string{"invitee.created", "invitee.canceled"}
		}
		payload := map[string]any{
			"url":          sub(d.CalendlyUrl),
			"events":       events,
			"organization": orgURI,
			"scope":        "user",
			"user":         userURI,
		}
		if strings.EqualFold(sub(d.CalendlyScope), "organization") {
			payload["scope"] = "organization"
			delete(payload, "user")
		}
		return calendlyCall(ctx, token, http.MethodPost, "/webhook_subscriptions", payload)

	case "delete_webhook":
		if sub(d.CalendlyWebhookId) == "" {
			return "", fmt.Errorf("delete_webhook needs a webhook URI")
		}
		return calendlyCall(ctx, token, http.MethodDelete,
			"/webhook_subscriptions/"+calendlyUUID(sub(d.CalendlyWebhookId)), nil)

	case "delete_invitee_data":
		// GDPR erasure; irreversible, and Calendly applies it asynchronously.
		emails := splitCSV(sub(d.CalendlyEmail))
		if len(emails) == 0 {
			return "", fmt.Errorf("delete_invitee_data needs at least one email address")
		}
		return calendlyCall(ctx, token, http.MethodPost, "/data_compliance/deletion/invitees",
			map[string]any{"emails": emails})

	case "":
		return "", fmt.Errorf("no Calendly operation selected")
	}
	return "", fmt.Errorf("unsupported Calendly operation: %s", d.IntegrationOp)
}

// calendlyUUID takes the last path segment of a URI, since path-style endpoints
// want the bare uuid while query filters want the whole URI. Accepting either
// spares the user from knowing which is which.
func calendlyUUID(uri string) string {
	uri = strings.TrimRight(strings.TrimSpace(uri), "/")
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

// calendlyBook creates a scheduled event directly. Calendly rejects a start time
// that is not an available slot, so list_available_times comes first.
func calendlyBook(ctx context.Context, token string, d FlowNodeData, sub func(string) string) (string, error) {
	switch {
	case sub(d.CalendlyEventType) == "":
		return "", fmt.Errorf("create_booking needs an event type URI")
	case sub(d.CalendlyStartTime) == "":
		return "", fmt.Errorf("create_booking needs a start time that appears in list_available_times")
	case sub(d.CalendlyInviteeEmail) == "":
		return "", fmt.Errorf("create_booking needs the invitee's email address")
	}
	invitee := map[string]any{
		"name":  firstNonEmpty(sub(d.CalendlyInviteeName), sub(d.CalendlyInviteeEmail)),
		"email": sub(d.CalendlyInviteeEmail),
		// An IANA zone is required, and Calendly uses it for the confirmation
		// email's rendered time rather than to shift the start time.
		"timezone": firstNonEmpty(sub(d.CalendlyTimezone), "UTC"),
	}
	payload := map[string]any{
		"event_type": sub(d.CalendlyEventType),
		"start_time": sub(d.CalendlyStartTime),
		"invitee":    invitee,
	}
	if guests := splitCSV(sub(d.CalendlyGuests)); len(guests) > 0 {
		payload["event_guests"] = guests
	}
	if qa := strings.TrimSpace(sub(d.CalendlyAnswers)); qa != "" {
		var answers []any
		if json.Unmarshal([]byte(qa), &answers) != nil {
			return "", fmt.Errorf(`answers must be a JSON array, e.g. ` +
				`[{"question":"Company","answer":"Acme","position":0}]`)
		}
		payload["questions_and_answers"] = answers
	}
	return calendlyCall(ctx, token, http.MethodPost, "/invitees", payload)
}
