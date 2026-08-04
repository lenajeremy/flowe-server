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

// Typeform's Create and Responses APIs, both on api.typeform.com.
//
// The shape worth handling here is a response payload. Typeform returns answers
// as a flat array whose entries reference their question by field id, so reading
// "what did they say to question 3" means joining answers against the form
// definition. get_response_text does that join and returns readable Q&A pairs,
// which is what a summarising or routing workflow actually wants.

const typeformAPI = "https://api.typeform.com"

func typeformCall(ctx context.Context, token, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, typeformAPI+path, reader)
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
		return "", fmt.Errorf("typeform request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		}
		msg := ""
		if json.Unmarshal(raw, &e) == nil && e.Description != "" {
			msg = e.Description
			if e.Code != "" {
				msg += " (" + e.Code + ")"
			}
		}
		if msg == "" {
			msg = truncateStr(string(raw), 300)
		}
		if resp.StatusCode == http.StatusForbidden {
			msg += " — the token may be missing a scope, or the form belongs to another workspace"
		}
		return "", fmt.Errorf("Typeform API error (%d): %s", resp.StatusCode, msg)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

func runTypeform(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.TypeformLimit, 25)
	form := func() string { return sub(d.TypeformFormId) }

	switch d.IntegrationOp {
	// ---- forms ----
	case "list_forms":
		q := url.Values{"page_size": {fmt.Sprint(limit)}}
		if v := sub(d.TypeformSearch); v != "" {
			q.Set("search", v)
		}
		if v := sub(d.TypeformWorkspaceId); v != "" {
			q.Set("workspace_id", v)
		}
		raw, err := typeformCall(ctx, token, http.MethodGet, "/forms?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_form":
		if form() == "" {
			return "", fmt.Errorf("get_form needs a form ID")
		}
		raw, err := typeformCall(ctx, token, http.MethodGet, "/forms/"+form(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "create_form":
		// The full form definition is passed through: Typeform's field schema is
		// large and varies per question type, so validating it here would only
		// duplicate their API and go stale.
		def := strings.TrimSpace(sub(d.TypeformDefinition))
		if def == "" {
			if sub(d.TypeformTitle) == "" {
				return "", fmt.Errorf("create_form needs a title, or a full JSON form definition")
			}
			// A title alone produces an empty form, which is a valid starting point.
			return typeformCall(ctx, token, http.MethodPost, "/forms",
				map[string]any{"title": sub(d.TypeformTitle)})
		}
		var m map[string]any
		if json.Unmarshal([]byte(def), &m) != nil {
			return "", fmt.Errorf(`the form definition must be a JSON object with at least a title, ` +
				`e.g. {"title":"Feedback","fields":[…]}`)
		}
		if t := sub(d.TypeformTitle); t != "" {
			m["title"] = t
		}
		return typeformCall(ctx, token, http.MethodPost, "/forms", m)

	case "update_form":
		if form() == "" {
			return "", fmt.Errorf("update_form needs a form ID")
		}
		def := strings.TrimSpace(sub(d.TypeformDefinition))
		if def == "" {
			return "", fmt.Errorf("update_form needs a full JSON form definition — a PUT replaces " +
				"the form, so fetch it with get_form, change what you need, and send it back")
		}
		var m map[string]any
		if json.Unmarshal([]byte(def), &m) != nil {
			return "", fmt.Errorf("the form definition must be a JSON object")
		}
		return typeformCall(ctx, token, http.MethodPut, "/forms/"+form(), m)

	case "delete_form":
		if form() == "" {
			return "", fmt.Errorf("delete_form needs a form ID")
		}
		return typeformCall(ctx, token, http.MethodDelete, "/forms/"+form(), nil)

	case "get_form_messages":
		if form() == "" {
			return "", fmt.Errorf("get_form_messages needs a form ID")
		}
		return typeformCall(ctx, token, http.MethodGet, "/forms/"+form()+"/messages", nil)

	// ---- responses ----
	case "list_responses":
		if form() == "" {
			return "", fmt.Errorf("list_responses needs a form ID")
		}
		raw, err := typeformCall(ctx, token, http.MethodGet,
			"/forms/"+form()+"/responses?"+typeformResponseQuery(d, sub, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "get_response_text":
		// Answers reference their question by field id, so on its own a response is
		// unreadable. Join it against the form definition into Q&A pairs.
		if form() == "" {
			return "", fmt.Errorf("get_response_text needs a form ID")
		}
		return typeformResponseText(ctx, token, form(), d, sub, limit)

	case "delete_responses":
		ids := splitCSV(sub(d.TypeformResponseIds))
		if form() == "" || len(ids) == 0 {
			return "", fmt.Errorf("delete_responses needs a form ID and at least one response token")
		}
		q := url.Values{}
		for _, id := range ids {
			q.Add("included_tokens", id)
		}
		return typeformCall(ctx, token, http.MethodDelete,
			"/forms/"+form()+"/responses?"+q.Encode(), nil)

	case "get_insights":
		if form() == "" {
			return "", fmt.Errorf("get_insights needs a form ID")
		}
		return typeformCall(ctx, token, http.MethodGet, "/insights/"+form()+"/summary", nil)

	// ---- workspaces ----
	case "list_workspaces":
		return typeformCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/workspaces?page_size=%d", limit), nil)

	case "get_workspace":
		if sub(d.TypeformWorkspaceId) == "" {
			return "", fmt.Errorf("get_workspace needs a workspace ID")
		}
		return typeformCall(ctx, token, http.MethodGet, "/workspaces/"+sub(d.TypeformWorkspaceId), nil)

	case "create_workspace":
		if sub(d.TypeformTitle) == "" {
			return "", fmt.Errorf("create_workspace needs a name")
		}
		return typeformCall(ctx, token, http.MethodPost, "/workspaces",
			map[string]any{"name": sub(d.TypeformTitle)})

	case "delete_workspace":
		if sub(d.TypeformWorkspaceId) == "" {
			return "", fmt.Errorf("delete_workspace needs a workspace ID")
		}
		return typeformCall(ctx, token, http.MethodDelete,
			"/workspaces/"+sub(d.TypeformWorkspaceId), nil)

	// ---- themes & images ----
	case "list_themes":
		return typeformCall(ctx, token, http.MethodGet,
			fmt.Sprintf("/themes?page_size=%d", limit), nil)

	case "get_theme":
		if sub(d.TypeformThemeId) == "" {
			return "", fmt.Errorf("get_theme needs a theme ID")
		}
		return typeformCall(ctx, token, http.MethodGet, "/themes/"+sub(d.TypeformThemeId), nil)

	case "delete_theme":
		if sub(d.TypeformThemeId) == "" {
			return "", fmt.Errorf("delete_theme needs a theme ID")
		}
		return typeformCall(ctx, token, http.MethodDelete, "/themes/"+sub(d.TypeformThemeId), nil)

	case "list_images":
		return typeformCall(ctx, token, http.MethodGet, "/images", nil)

	// ---- webhooks ----
	case "list_webhooks":
		if form() == "" {
			return "", fmt.Errorf("list_webhooks needs a form ID")
		}
		return typeformCall(ctx, token, http.MethodGet, "/forms/"+form()+"/webhooks", nil)

	case "create_webhook":
		// Typeform keys a webhook by a tag you choose, and PUT is create-or-update.
		if form() == "" || sub(d.TypeformUrl) == "" {
			return "", fmt.Errorf("create_webhook needs a form ID and a URL")
		}
		tag := firstNonEmpty(sub(d.TypeformTag), "fernary")
		payload := map[string]any{"url": sub(d.TypeformUrl), "enabled": true}
		if v := sub(d.TypeformSecret); v != "" {
			// Lets the receiving end verify the payload signature.
			payload["secret"] = v
			payload["verify_ssl"] = true
		}
		return typeformCall(ctx, token, http.MethodPut,
			"/forms/"+form()+"/webhooks/"+url.PathEscape(tag), payload)

	case "delete_webhook":
		if form() == "" {
			return "", fmt.Errorf("delete_webhook needs a form ID")
		}
		tag := firstNonEmpty(sub(d.TypeformTag), "fernary")
		return typeformCall(ctx, token, http.MethodDelete,
			"/forms/"+form()+"/webhooks/"+url.PathEscape(tag), nil)

	case "get_current_user":
		return typeformCall(ctx, token, http.MethodGet, "/me", nil)

	case "":
		return "", fmt.Errorf("no Typeform operation selected")
	}
	return "", fmt.Errorf("unsupported Typeform operation: %s", d.IntegrationOp)
}

// typeformResponseQuery builds the shared filter for the responses endpoints.
func typeformResponseQuery(d FlowNodeData, sub func(string) string, limit int) string {
	q := url.Values{"page_size": {fmt.Sprint(limit)}}
	if v := sub(d.TypeformSince); v != "" {
		q.Set("since", v)
	}
	if v := sub(d.TypeformUntil); v != "" {
		q.Set("until", v)
	}
	if v := sub(d.TypeformCompleted); v != "" {
		q.Set("completed", v)
	}
	if v := sub(d.TypeformQuery); v != "" {
		q.Set("query", v)
	}
	if v := sub(d.TypeformAfter); v != "" {
		// Cursor pagination, for walking new responses across scheduled runs.
		q.Set("after", v)
	}
	return q.Encode()
}

// typeformResponseText joins answers to their question titles.
func typeformResponseText(ctx context.Context, token, formID string, d FlowNodeData,
	sub func(string) string, limit int) (string, error) {

	formRaw, err := typeformCall(ctx, token, http.MethodGet, "/forms/"+formID, nil)
	if err != nil {
		return "", err
	}
	var def struct {
		Title  string `json:"title"`
		Fields []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"fields"`
	}
	if json.Unmarshal([]byte(formRaw), &def) != nil {
		return "", fmt.Errorf("could not read the form definition")
	}
	titles := make(map[string]string, len(def.Fields))
	for _, f := range def.Fields {
		titles[f.ID] = f.Title
	}

	respRaw, err := typeformCall(ctx, token, http.MethodGet,
		"/forms/"+formID+"/responses?"+typeformResponseQuery(d, sub, limit), nil)
	if err != nil {
		return "", err
	}
	var page struct {
		Items []struct {
			Token       string `json:"token"`
			SubmittedAt string `json:"submitted_at"`
			Answers     []struct {
				Field struct {
					ID string `json:"id"`
				} `json:"field"`
				Type    string                    `json:"type"`
				Text    string                    `json:"text"`
				Email   string                    `json:"email"`
				Number  json.Number               `json:"number"`
				Boolean *bool                     `json:"boolean"`
				Date    string                    `json:"date"`
				URL     string                    `json:"url"`
				Phone   string                    `json:"phone_number"`
				Choice  struct{ Label string }    `json:"choice"`
				Choices struct{ Labels []string } `json:"choices"`
			} `json:"answers"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(respRaw), &page) != nil {
		return "", fmt.Errorf("could not read the responses")
	}
	if len(page.Items) == 0 {
		return "", fmt.Errorf("this form has no responses matching those filters")
	}

	var b strings.Builder
	for _, item := range page.Items {
		b.WriteString("Response ")
		b.WriteString(item.Token)
		if item.SubmittedAt != "" {
			b.WriteString(" (")
			b.WriteString(item.SubmittedAt)
			b.WriteString(")")
		}
		b.WriteString("\n")
		for _, a := range item.Answers {
			question := titles[a.Field.ID]
			if question == "" {
				question = a.Field.ID
			}
			b.WriteString("  ")
			b.WriteString(question)
			b.WriteString(": ")
			b.WriteString(typeformAnswerValue(a.Type, a.Text, a.Email, a.Number.String(),
				a.Boolean, a.Date, a.URL, a.Phone, a.Choice.Label, a.Choices.Labels))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return truncateStr(b.String(), 12000), nil
}

// typeformAnswerValue renders whichever of the answer variants is populated. The
// field carrying the value is named after the answer type, so the type decides
// where to look.
func typeformAnswerValue(kind, text, email, number string, boolean *bool,
	date, link, phone, choice string, choices []string) string {

	switch kind {
	case "text", "long_text":
		return text
	case "email":
		return email
	case "number":
		return number
	case "boolean":
		if boolean != nil && *boolean {
			return "yes"
		}
		return "no"
	case "date":
		return date
	case "url", "file_url":
		return link
	case "phone_number":
		return phone
	case "choice":
		return choice
	case "choices":
		return strings.Join(choices, ", ")
	}
	// An unrecognized type is better reported than silently dropped.
	return firstNonEmpty(text, email, number, choice, strings.Join(choices, ", "),
		date, link, phone, "(no answer)")
}
