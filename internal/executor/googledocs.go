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
