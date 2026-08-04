package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// Confluence Cloud, via the same Atlassian gateway as Jira:
// https://api.atlassian.com/ex/confluence/{cloudId}/wiki/...
//
// This provider straddles two API versions on purpose. v2 is the current API and
// covers pages, spaces, blog posts and comments. But v2 has no CQL search, no
// label-add, and no attachment upload, so those three ops use v1 (/wiki/rest/api),
// which is still supported. Paths below always spell out which they mean.

const (
	confluenceV2 = "/wiki/api/v2"
	confluenceV1 = "/wiki/rest/api"
)

func confluenceCall(ctx context.Context, token, cloudID, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		atlassianGateway+"/ex/confluence/"+cloudID+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return jiraDo(req, "Confluence")
}

// storageBody wraps content for Confluence's storage format. Text that already
// looks like markup is passed through; anything else becomes paragraphs, with
// XML-significant characters escaped so an ampersand in prose can't break the
// document.
func storageBody(text string) map[string]any {
	value := text
	if !strings.Contains(text, "<") {
		var b strings.Builder
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			b.WriteString("<p>")
			b.WriteString(escapeXML(line))
			b.WriteString("</p>")
		}
		value = b.String()
	}
	return map[string]any{"representation": "storage", "value": value}
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// confluenceSpaceID turns a space key (what users know, e.g. "ENG") into the
// numeric id that v2 endpoints require. Already-numeric input passes through.
func confluenceSpaceID(ctx context.Context, token, cloudID, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("this operation needs a space key")
	}
	if isAllDigits(key) {
		return key, nil
	}
	raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet,
		confluenceV2+"/spaces?keys="+url.QueryEscape(key), nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Results []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(raw), &out) != nil || len(out.Results) == 0 {
		return "", fmt.Errorf("no Confluence space found with key %q", key)
	}
	return out.Results[0].ID, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// confluencePageVersion reads a page's current version. Updates must send
// version.number + 1 or Confluence rejects them as a conflict.
func confluencePageVersion(ctx context.Context, token, cloudID, pageID string) (int, string, error) {
	raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet,
		confluenceV2+"/pages/"+pageID, nil)
	if err != nil {
		return 0, "", err
	}
	var page struct {
		Title   string `json:"title"`
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	if json.Unmarshal([]byte(raw), &page) != nil || page.Version.Number == 0 {
		return 0, "", fmt.Errorf("could not read the current version of page %s", pageID)
	}
	return page.Version.Number, page.Title, nil
}

func runConfluence(ctx context.Context, token, cloudID string, d FlowNodeData, outputs map[string]string) (string, error) {
	if cloudID == "" {
		return "", fmt.Errorf("no Confluence site is linked to this connection — reconnect Confluence to select a site")
	}
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.ConfluenceLimit, 25)
	page := func() string { return sub(d.ConfluencePageId) }

	switch d.IntegrationOp {
	// ---- spaces ----
	case "list_spaces":
		raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet,
			fmt.Sprintf("%s/spaces?limit=%d", confluenceV2, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_space":
		id, err := confluenceSpaceID(ctx, token, cloudID, sub(d.ConfluenceSpaceKey))
		if err != nil {
			return "", err
		}
		return confluenceCall(ctx, token, cloudID, http.MethodGet, confluenceV2+"/spaces/"+id, nil)

	// ---- pages ----
	case "list_pages":
		path := fmt.Sprintf("%s/pages?limit=%d&sort=-modified-date", confluenceV2, limit)
		if key := sub(d.ConfluenceSpaceKey); key != "" {
			id, err := confluenceSpaceID(ctx, token, cloudID, key)
			if err != nil {
				return "", err
			}
			path = fmt.Sprintf("%s/spaces/%s/pages?limit=%d&sort=-modified-date", confluenceV2, id, limit)
		}
		raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_page":
		if page() == "" {
			return "", fmt.Errorf("get_page needs a page id")
		}
		raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet,
			confluenceV2+"/pages/"+page()+"?body-format=storage", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "find_page_by_title":
		if sub(d.ConfluenceTitle) == "" {
			return "", fmt.Errorf("find_page_by_title needs a title")
		}
		q := url.Values{"title": {sub(d.ConfluenceTitle)}, "limit": {fmt.Sprint(limit)}}
		if key := sub(d.ConfluenceSpaceKey); key != "" {
			id, err := confluenceSpaceID(ctx, token, cloudID, key)
			if err != nil {
				return "", err
			}
			q.Set("space-id", id)
		}
		return confluenceCall(ctx, token, cloudID, http.MethodGet,
			confluenceV2+"/pages?"+q.Encode(), nil)

	case "list_child_pages":
		if page() == "" {
			return "", fmt.Errorf("list_child_pages needs a parent page id")
		}
		return confluenceCall(ctx, token, cloudID, http.MethodGet,
			fmt.Sprintf("%s/pages/%s/children?limit=%d", confluenceV2, page(), limit), nil)

	case "create_page":
		spaceID, err := confluenceSpaceID(ctx, token, cloudID, sub(d.ConfluenceSpaceKey))
		if err != nil {
			return "", err
		}
		payload := map[string]any{
			"spaceId": spaceID,
			"status":  firstNonEmpty(sub(d.ConfluenceStatus), "current"),
			"title":   sub(d.ConfluenceTitle),
			"body":    storageBody(sub(d.ConfluenceBody)),
		}
		if p := sub(d.ConfluenceParentId); p != "" {
			payload["parentId"] = p
		}
		return confluenceCall(ctx, token, cloudID, http.MethodPost, confluenceV2+"/pages", payload)

	case "update_page":
		if page() == "" {
			return "", fmt.Errorf("update_page needs a page id")
		}
		version, currentTitle, err := confluencePageVersion(ctx, token, cloudID, page())
		if err != nil {
			return "", err
		}
		// Title and body are both required on update, so an unset title has to
		// fall back to the existing one rather than be omitted.
		payload := map[string]any{
			"id":      page(),
			"status":  firstNonEmpty(sub(d.ConfluenceStatus), "current"),
			"title":   firstNonEmpty(sub(d.ConfluenceTitle), currentTitle),
			"body":    storageBody(sub(d.ConfluenceBody)),
			"version": map[string]any{"number": version + 1},
		}
		return confluenceCall(ctx, token, cloudID, http.MethodPut, confluenceV2+"/pages/"+page(), payload)

	case "delete_page":
		if page() == "" {
			return "", fmt.Errorf("delete_page needs a page id")
		}
		return confluenceCall(ctx, token, cloudID, http.MethodDelete, confluenceV2+"/pages/"+page(), nil)

	case "search_pages":
		// CQL search only exists on v1.
		cql := sub(d.ConfluenceCql)
		if cql == "" {
			return "", fmt.Errorf("search_pages needs a CQL query, e.g. text ~ \"onboarding\"")
		}
		raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet, fmt.Sprintf(
			"%s/search?cql=%s&limit=%d", confluenceV1, url.QueryEscape(cql), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- blog posts ----
	case "list_blog_posts":
		raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet,
			fmt.Sprintf("%s/blogposts?limit=%d&sort=-created-date", confluenceV2, limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_blog_post":
		spaceID, err := confluenceSpaceID(ctx, token, cloudID, sub(d.ConfluenceSpaceKey))
		if err != nil {
			return "", err
		}
		return confluenceCall(ctx, token, cloudID, http.MethodPost, confluenceV2+"/blogposts", map[string]any{
			"spaceId": spaceID,
			"status":  firstNonEmpty(sub(d.ConfluenceStatus), "current"),
			"title":   sub(d.ConfluenceTitle),
			"body":    storageBody(sub(d.ConfluenceBody)),
		})

	// ---- comments ----
	case "add_comment":
		if page() == "" || sub(d.ConfluenceComment) == "" {
			return "", fmt.Errorf("add_comment needs a page id and comment text")
		}
		return confluenceCall(ctx, token, cloudID, http.MethodPost, confluenceV2+"/footer-comments",
			map[string]any{"pageId": page(), "body": storageBody(sub(d.ConfluenceComment))})

	case "list_comments":
		if page() == "" {
			return "", fmt.Errorf("list_comments needs a page id")
		}
		raw, err := confluenceCall(ctx, token, cloudID, http.MethodGet, fmt.Sprintf(
			"%s/pages/%s/footer-comments?limit=%d&body-format=storage", confluenceV2, page(), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- labels ----
	case "list_labels":
		if page() == "" {
			return "", fmt.Errorf("list_labels needs a page id")
		}
		return confluenceCall(ctx, token, cloudID, http.MethodGet,
			confluenceV2+"/pages/"+page()+"/labels", nil)

	case "add_label":
		if page() == "" {
			return "", fmt.Errorf("add_label needs a page id")
		}
		names := splitCSV(sub(d.ConfluenceLabel))
		if len(names) == 0 {
			return "", fmt.Errorf("add_label needs at least one label")
		}
		// v2 can read labels but not write them; v1 takes an array.
		labels := make([]map[string]string, 0, len(names))
		for _, n := range names {
			labels = append(labels, map[string]string{"prefix": "global", "name": n})
		}
		return confluenceCall(ctx, token, cloudID, http.MethodPost,
			confluenceV1+"/content/"+page()+"/label", labels)

	// ---- attachments ----
	case "list_attachments":
		if page() == "" {
			return "", fmt.Errorf("list_attachments needs a page id")
		}
		return confluenceCall(ctx, token, cloudID, http.MethodGet,
			fmt.Sprintf("%s/pages/%s/attachments?limit=%d", confluenceV2, page(), limit), nil)

	case "upload_attachment":
		return confluenceAttach(ctx, token, cloudID, page(),
			firstNonEmpty(sub(d.ConfluenceAttachName), "attachment.txt"), sub(d.ConfluenceAttachBody))

	case "get_current_user":
		return confluenceCall(ctx, token, cloudID, http.MethodGet, confluenceV1+"/user/current", nil)

	case "":
		return "", fmt.Errorf("no Confluence operation selected")
	}
	return "", fmt.Errorf("unsupported Confluence operation: %s", d.IntegrationOp)
}

// confluenceAttach uploads text content as a page attachment. v1 only, and like
// Jira it needs X-Atlassian-Token to pass the XSRF check.
func confluenceAttach(ctx context.Context, token, cloudID, pageID, name, content string) (string, error) {
	if pageID == "" {
		return "", fmt.Errorf("upload_attachment needs a page id")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(part, content); err != nil {
		return "", err
	}
	_ = w.WriteField("minorEdit", "true")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		atlassianGateway+"/ex/confluence/"+cloudID+confluenceV1+"/content/"+pageID+"/child/attachment",
		&buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check")
	return jiraDo(req, "Confluence")
}
