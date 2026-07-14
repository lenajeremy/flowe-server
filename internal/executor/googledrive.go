package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

	case "upload_file":
		mime := firstNonEmpty(sub(d.GDriveMimeType), "text/plain")
		meta := map[string]any{"name": firstNonEmpty(sub(d.GDriveName), "upload.txt"), "mimeType": mime}
		if parent := sub(d.GDriveParentId); parent != "" {
			meta["parents"] = []string{parent}
		}
		return gdriveMultipartUpload(ctx, token, meta, mime, sub(d.GDriveContent))

	case "read_file":
		id := url.PathEscape(sub(d.GDriveFileId))
		metaRaw, err := googleCall(ctx, token, http.MethodGet, gdriveBase+"/files/"+id+"?fields=id,name,mimeType", nil)
		if err != nil {
			return "", err
		}
		var meta struct {
			Name     string `json:"name"`
			MimeType string `json:"mimeType"`
		}
		_ = json.Unmarshal([]byte(metaRaw), &meta)
		var content string
		if strings.HasPrefix(meta.MimeType, "application/vnd.google-apps.") {
			// Google-native files (Docs/Sheets/Slides) must be exported.
			content, err = googleCall(ctx, token, http.MethodGet,
				gdriveBase+"/files/"+id+"/export?mimeType="+url.QueryEscape("text/plain"), nil)
		} else {
			content, err = googleCall(ctx, token, http.MethodGet, gdriveBase+"/files/"+id+"?alt=media", nil)
		}
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"name": meta.Name, "mimeType": meta.MimeType, "content": truncateStr(content, 1<<20)})
		return string(b), nil

	case "copy_file":
		body := map[string]any{}
		if name := sub(d.GDriveName); name != "" {
			body["name"] = name
		}
		if parent := sub(d.GDriveParentId); parent != "" {
			body["parents"] = []string{parent}
		}
		raw, err := googleCall(ctx, token, http.MethodPost,
			gdriveBase+"/files/"+url.PathEscape(sub(d.GDriveFileId))+"/copy?fields=id,name,webViewLink", body)
		if err != nil {
			return "", err
		}
		return raw, nil

	case "move_file":
		id := url.PathEscape(sub(d.GDriveFileId))
		// Fetch current parents so the move removes them all.
		metaRaw, err := googleCall(ctx, token, http.MethodGet, gdriveBase+"/files/"+id+"?fields=parents", nil)
		if err != nil {
			return "", err
		}
		var meta struct {
			Parents []string `json:"parents"`
		}
		_ = json.Unmarshal([]byte(metaRaw), &meta)
		q := url.Values{}
		q.Set("addParents", sub(d.GDriveParentId))
		if len(meta.Parents) > 0 {
			q.Set("removeParents", strings.Join(meta.Parents, ","))
		}
		q.Set("fields", "id,name,parents,webViewLink")
		raw, err := googleCall(ctx, token, http.MethodPatch, gdriveBase+"/files/"+id+"?"+q.Encode(), map[string]any{})
		if err != nil {
			return "", err
		}
		return raw, nil

	case "rename_file":
		raw, err := googleCall(ctx, token, http.MethodPatch,
			gdriveBase+"/files/"+url.PathEscape(sub(d.GDriveFileId))+"?fields=id,name,webViewLink",
			map[string]any{"name": sub(d.GDriveName)})
		if err != nil {
			return "", err
		}
		return raw, nil

	case "share_file":
		id := url.PathEscape(sub(d.GDriveFileId))
		role := firstNonEmpty(sub(d.GDriveRole), "reader")
		perm := map[string]any{"role": role}
		if email := sub(d.GDriveEmail); email != "" {
			perm["type"] = "user"
			perm["emailAddress"] = email
		} else {
			perm["type"] = "anyone"
		}
		if _, err := googleCall(ctx, token, http.MethodPost, gdriveBase+"/files/"+id+"/permissions", perm); err != nil {
			return "", err
		}
		linkRaw, err := googleCall(ctx, token, http.MethodGet, gdriveBase+"/files/"+id+"?fields=webViewLink", nil)
		if err != nil {
			return "", err
		}
		var link struct {
			WebViewLink string `json:"webViewLink"`
		}
		_ = json.Unmarshal([]byte(linkRaw), &link)
		b, _ := json.Marshal(map[string]any{"status": "shared", "role": role, "link": link.WebViewLink})
		return string(b), nil

	case "list_permissions":
		raw, err := googleCall(ctx, token, http.MethodGet,
			gdriveBase+"/files/"+url.PathEscape(sub(d.GDriveFileId))+"/permissions?fields=permissions(id,type,role,emailAddress)", nil)
		if err != nil {
			return "", err
		}
		return raw, nil

	case "trash_file":
		if _, err := googleCall(ctx, token, http.MethodPatch,
			gdriveBase+"/files/"+url.PathEscape(sub(d.GDriveFileId)),
			map[string]any{"trashed": true}); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":"trashed","id":%s}`, jsonString(sub(d.GDriveFileId))), nil

	default:
		return "", fmt.Errorf("unknown Google Drive operation: %s", d.IntegrationOp)
	}
}

// gdriveMultipartUpload creates a file with content in one multipart request.
func gdriveMultipartUpload(ctx context.Context, token string, meta map[string]any, mime, content string) (string, error) {
	metaJSON, _ := json.Marshal(meta)
	boundary := "flowe-upload-boundary"
	var body strings.Builder
	body.WriteString("--" + boundary + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n")
	body.Write(metaJSON)
	body.WriteString("\r\n--" + boundary + "\r\nContent-Type: " + mime + "\r\n\r\n")
	body.WriteString(content)
	body.WriteString("\r\n--" + boundary + "--")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id,name,webViewLink",
		strings.NewReader(body.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("drive upload failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Drive upload returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}
