package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Keep API v1.
//
// This is the most restricted service of the set, and the restrictions are worth
// stating plainly because they decide whether the node can work at all:
//
//   - It is Google Workspace only. A personal @gmail.com account cannot authorize
//     it, no matter what the consent screen says.
//   - Google positions it as an enterprise administration API. A Workspace admin
//     has to enable it, and organisations commonly reach it through domain-wide
//     delegation rather than per-user consent.
//   - A note created through the API belongs to the caller. Notes made in the
//     Keep app are not necessarily visible to it.
//
// All of those land as a 403, so keepError says so rather than reporting an
// unexplained permission failure.

const keepAPI = "https://keep.googleapis.com/v1"

func keepError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "PERMISSION_DENIED") {
		return fmt.Errorf("%w — the Keep API is Google Workspace only (not personal "+
			"@gmail.com accounts) and a Workspace admin has to enable it for the domain", err)
	}
	return err
}

func runGoogleKeep(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.KeepLimit, 25)

	// A note name is notes/{id}; accept a bare id too.
	note := func() string {
		v := strings.TrimSpace(sub(d.KeepNoteName))
		if v == "" || strings.HasPrefix(v, "notes/") {
			return v
		}
		return "notes/" + v
	}
	call := func(method, path string, body any) (string, error) {
		out, err := googleCall(ctx, token, method, keepAPI+path, body)
		return out, keepError(err)
	}

	switch d.IntegrationOp {
	case "create_note":
		if sub(d.KeepTitle) == "" && sub(d.KeepText) == "" && sub(d.KeepListItems) == "" {
			return "", fmt.Errorf("create_note needs a title, body text, or checklist items")
		}
		body := map[string]any{"title": sub(d.KeepTitle)}
		// A note's body is either prose or a checklist, never both.
		if items := sub(d.KeepListItems); strings.TrimSpace(items) != "" {
			listItems := []any{}
			for _, line := range strings.Split(strings.ReplaceAll(items, "\r\n", "\n"), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					listItems = append(listItems, map[string]any{
						"text":    map[string]any{"text": line},
						"checked": false,
					})
				}
			}
			body["body"] = map[string]any{"list": map[string]any{"listItems": listItems}}
		} else {
			body["body"] = map[string]any{"text": map[string]any{"text": sub(d.KeepText)}}
		}
		return call(http.MethodPost, "/notes", body)

	case "get_note":
		if note() == "" {
			return "", fmt.Errorf("get_note needs a note name")
		}
		return call(http.MethodGet, "/"+note(), nil)

	case "list_notes":
		q := url.Values{"pageSize": {fmt.Sprint(limit)}}
		if f := sub(d.KeepFilter); f != "" {
			q.Set("filter", f)
		}
		raw, err := call(http.MethodGet, "/notes?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "delete_note":
		if note() == "" {
			return "", fmt.Errorf("delete_note needs a note name")
		}
		if _, err := call(http.MethodDelete, "/"+note(), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deletedNote":%q}`, note()), nil

	case "share_note":
		emails := splitCSV(sub(d.KeepEmail))
		if note() == "" || len(emails) == 0 {
			return "", fmt.Errorf("share_note needs a note name and at least one email")
		}
		requests := make([]any, 0, len(emails))
		for _, e := range emails {
			requests = append(requests, map[string]any{
				"parent": note(),
				"permission": map[string]any{
					"role":  "WRITER",
					"email": e,
				},
			})
		}
		return call(http.MethodPost, "/"+note()+"/permissions:batchCreate",
			map[string]any{"requests": requests})

	case "unshare_note":
		// batchDelete takes permission names, which list them via get_note.
		names := splitCSV(sub(d.KeepEmail))
		if note() == "" || len(names) == 0 {
			return "", fmt.Errorf("unshare_note needs a note name and permission names from get_note")
		}
		for i, n := range names {
			if !strings.Contains(n, "/permissions/") {
				names[i] = note() + "/permissions/" + n
			}
		}
		if _, err := call(http.MethodPost, "/"+note()+"/permissions:batchDelete",
			map[string]any{"names": names}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"note":%q,"removed":%d}`, note(), len(names)), nil

	case "":
		return "", fmt.Errorf("no Google Keep operation selected")
	}
	return "", fmt.Errorf("unsupported Google Keep operation: %s", d.IntegrationOp)
}
