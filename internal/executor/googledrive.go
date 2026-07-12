package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Google Drive API v3.

const gdriveBase = "https://www.googleapis.com/drive/v3"

func runGoogleDrive(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	switch d.IntegrationOp {
	case "list_files", "search":
		q := url.Values{}
		if query := sub(d.GDriveQuery); query != "" {
			q.Set("q", query)
		} else {
			q.Set("q", "trashed=false")
		}
		q.Set("pageSize", fmt.Sprint(intOr(d.GDriveLimit, 20)))
		q.Set("fields", "files(id,name,mimeType,webViewLink)")
		raw, err := googleCall(ctx, token, http.MethodGet, gdriveBase+"/files?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Files []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				MimeType string `json:"mimeType"`
				Link     string `json:"webViewLink"`
			} `json:"files"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Files))
		for _, f := range res.Files {
			out = append(out, map[string]any{"id": f.ID, "name": f.Name, "mimeType": f.MimeType, "link": f.Link})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "get_file":
		raw, err := googleCall(ctx, token, http.MethodGet,
			gdriveBase+"/files/"+url.PathEscape(sub(d.GDriveFileId))+"?fields=id,name,mimeType,size,webViewLink,modifiedTime", nil)
		if err != nil {
			return "", err
		}
		return raw, nil

	case "create_folder":
		meta := map[string]any{
			"name":     sub(d.GDriveName),
			"mimeType": "application/vnd.google-apps.folder",
		}
		if parent := sub(d.GDriveParentId); parent != "" {
			meta["parents"] = []string{parent}
		}
		raw, err := googleCall(ctx, token, http.MethodPost, gdriveBase+"/files?fields=id,name,webViewLink", meta)
		if err != nil {
			return "", err
		}
		return raw, nil

	case "delete_file":
		if _, err := googleCall(ctx, token, http.MethodDelete,
			gdriveBase+"/files/"+url.PathEscape(sub(d.GDriveFileId)), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":"deleted","id":%s}`, jsonString(sub(d.GDriveFileId))), nil

	default:
		return "", fmt.Errorf("unknown Google Drive operation: %s", d.IntegrationOp)
	}
}
