package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Docs API v1.

const gdocsBase = "https://docs.googleapis.com/v1/documents"

func runGoogleDocs(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "create_document":
		raw, err := googleCall(ctx, token, http.MethodPost, gdocsBase, map[string]any{"title": sub(d.GDocsTitle)})
		if err != nil {
			return "", err
		}
		var res struct {
			DocumentID string `json:"documentId"`
			Title      string `json:"title"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{
			"status":     "created",
			"documentId": res.DocumentID,
			"title":      res.Title,
			"link":       "https://docs.google.com/document/d/" + res.DocumentID + "/edit",
		})
		return string(b), nil

	case "get_document":
		raw, err := googleCall(ctx, token, http.MethodGet,
			gdocsBase+"/"+url.PathEscape(sub(d.GDocsDocumentId)), nil)
		if err != nil {
			return "", err
		}
		text := gdocsPlainText(raw)
		b, _ := json.Marshal(map[string]any{"documentId": sub(d.GDocsDocumentId), "text": text})
		return string(b), nil

	case "append_text":
		body := map[string]any{
			"requests": []map[string]any{
				{"insertText": map[string]any{
					"text":                 sub(d.GDocsText),
					"endOfSegmentLocation": map[string]any{},
				}},
			},
		}
		if _, err := googleCall(ctx, token, http.MethodPost,
			gdocsBase+"/"+url.PathEscape(sub(d.GDocsDocumentId))+":batchUpdate", body); err != nil {
			return "", err
		}
		return `{"status":"appended"}`, nil

	case "replace_text":
		body := map[string]any{
			"requests": []map[string]any{
				{"replaceAllText": map[string]any{
					"containsText": map[string]any{"text": sub(d.GDocsFindText), "matchCase": true},
					"replaceText":  sub(d.GDocsReplaceText),
				}},
			},
		}
		raw, err := googleCall(ctx, token, http.MethodPost,
			gdocsBase+"/"+url.PathEscape(sub(d.GDocsDocumentId))+":batchUpdate", body)
		if err != nil {
			return "", err
		}
		var res struct {
			Replies []struct {
				ReplaceAllText struct {
					OccurrencesChanged int `json:"occurrencesChanged"`
				} `json:"replaceAllText"`
			} `json:"replies"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		changed := 0
		if len(res.Replies) > 0 {
			changed = res.Replies[0].ReplaceAllText.OccurrencesChanged
		}
		b, _ := json.Marshal(map[string]any{"status": "replaced", "occurrences": changed})
		return string(b), nil

	case "insert_text_at_start":
		body := map[string]any{
			"requests": []map[string]any{
				{"insertText": map[string]any{
					"text":     sub(d.GDocsText),
					"location": map[string]any{"index": 1},
				}},
			},
		}
		if _, err := googleCall(ctx, token, http.MethodPost,
			gdocsBase+"/"+url.PathEscape(sub(d.GDocsDocumentId))+":batchUpdate", body); err != nil {
			return "", err
		}
		return `{"status":"inserted"}`, nil

	case "create_from_template":
		// 1. Drive-copy the template, 2. replaceAllText for each mapping pair.
		copyBody := map[string]any{}
		if title := sub(d.GDocsTitle); title != "" {
			copyBody["name"] = title
		}
		copyRaw, err := googleCall(ctx, token, http.MethodPost,
			"https://www.googleapis.com/drive/v3/files/"+url.PathEscape(sub(d.GDocsTemplateId))+"/copy?fields=id,name", copyBody)
		if err != nil {
			return "", err
		}
		var copied struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(copyRaw), &copied)
		if copied.ID == "" {
			return "", fmt.Errorf("Google Docs: template copy failed")
		}

		if repl := sub(d.GDocsReplacements); repl != "" {
			var pairs map[string]string
			if err := json.Unmarshal([]byte(repl), &pairs); err != nil {
				return "", fmt.Errorf("Google Docs: replacements must be a JSON object of find→replace pairs: %w", err)
			}
			reqs := make([]map[string]any, 0, len(pairs))
			for find, replace := range pairs {
				reqs = append(reqs, map[string]any{
					"replaceAllText": map[string]any{
						"containsText": map[string]any{"text": find, "matchCase": true},
						"replaceText":  replace,
					},
				})
			}
			if len(reqs) > 0 {
				if _, err := googleCall(ctx, token, http.MethodPost,
					gdocsBase+"/"+copied.ID+":batchUpdate", map[string]any{"requests": reqs}); err != nil {
					return "", err
				}
			}
		}
		b, _ := json.Marshal(map[string]any{
			"status": "created", "documentId": copied.ID, "title": copied.Name,
			"link": "https://docs.google.com/document/d/" + copied.ID + "/edit",
		})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Google Docs operation: %s", d.IntegrationOp)
	}
}

// gdocsPlainText flattens a document's body into text by concatenating every
// paragraph's text-run content.
func gdocsPlainText(raw string) string {
	var doc struct {
		Body struct {
			Content []struct {
				Paragraph struct {
					Elements []struct {
						TextRun struct {
							Content string `json:"content"`
						} `json:"textRun"`
					} `json:"elements"`
				} `json:"paragraph"`
			} `json:"content"`
		} `json:"body"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range doc.Body.Content {
		for _, el := range c.Paragraph.Elements {
			b.WriteString(el.TextRun.Content)
		}
	}
	return b.String()
}
