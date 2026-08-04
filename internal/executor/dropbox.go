package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Dropbox API v2, which is not a conventional REST API and will not behave like
// one if treated as such:
//
//   - Everything is POST, including reads. There are no path parameters; the
//     target is named in the request body.
//   - Requests split across two hosts. Metadata calls go to api.dropboxapi.com
//     with a JSON body. Content calls (upload, download) go to
//     content.dropboxapi.com with the *arguments in a Dropbox-API-Arg header*
//     and the raw bytes as the body.
//   - That header must be ASCII, so a path containing an accent or an emoji has
//     to be \u-escaped or Dropbox rejects the request. dropboxArgHeader does it.
//   - Paths are absolute from the account root and must start with a slash. The
//     root itself is the empty string, not "/", which is the single most common
//     mistake — dropboxPath normalizes both.
//
// This node is text-oriented, so download returns decoded text capped in size.
// A binary file will come back as mojibake rather than something useful, and the
// op says so instead of pretending otherwise.

const (
	dropboxRPCHost     = "https://api.dropboxapi.com/2"
	dropboxContentHost = "https://content.dropboxapi.com/2"
)

// dropboxPath normalizes a user-supplied path. Dropbox wants "" for the root and
// a leading slash for everything else.
func dropboxPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// dropboxArgHeader JSON-encodes the arguments and escapes every non-ASCII rune,
// because an HTTP header cannot carry raw UTF-8 and Dropbox rejects it outright.
func dropboxArgHeader(arg any) (string, error) {
	b, err := json.Marshal(arg)
	if err != nil {
		return "", fmt.Errorf("could not encode the request arguments")
	}
	var out strings.Builder
	for _, r := range string(b) {
		if r < utf8.RuneSelf && r >= 0x20 && r != 0x7f {
			out.WriteRune(r)
			continue
		}
		fmt.Fprintf(&out, "\\u%04x", r)
	}
	return out.String(), nil
}

// dropboxRPC calls a metadata endpoint. A nil body means "no arguments", which
// Dropbox requires be sent with no Content-Type at all.
func dropboxRPC(ctx context.Context, token, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dropboxRPCHost+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return dropboxDo(req, 0)
}

func dropboxDo(req *http.Request, textCap int) (string, error) {
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("dropbox request failed: %w", err)
	}
	defer resp.Body.Close()

	limit := int64(1 << 20)
	if textCap > 0 {
		// Read a little past the cap so truncation is detectable.
		limit = int64(textCap) * 4
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, limit))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", dropboxError(resp.StatusCode, raw)
	}
	// A content-download response carries its metadata in a header and the file
	// itself in the body, so return the text rather than the envelope.
	if textCap > 0 {
		return truncateStr(string(raw), textCap), nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Sprintf(`{"ok":true,"status":%d}`, resp.StatusCode), nil
	}
	return string(raw), nil
}

// dropboxError turns Dropbox's tagged-union errors into a sentence. The useful
// detail is in error_summary, which reads like "path/not_found/..".
func dropboxError(status int, raw []byte) error {
	var e struct {
		Summary string `json:"error_summary"`
		Message string `json:"error_message"`
	}
	msg := ""
	if json.Unmarshal(raw, &e) == nil {
		msg = firstNonEmpty(e.Summary, e.Message)
	}
	if msg == "" {
		msg = truncateStr(string(raw), 300)
	}
	switch {
	case strings.Contains(msg, "path/not_found"):
		msg += " — paths are absolute from your Dropbox root and start with a slash, e.g. /Reports/q3.txt"
	case strings.Contains(msg, "path/conflict"):
		msg += " — something already exists there; set overwrite mode or choose another path"
	case strings.Contains(msg, "missing_scope"):
		msg += " — the connection lacks the scope for this call; reconnect Dropbox to grant it"
	case status == http.StatusTooManyRequests:
		msg += " — Dropbox is rate-limiting; retry after the delay it asked for"
	}
	return fmt.Errorf("Dropbox API error (%d): %s", status, msg)
}

func runDropbox(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	path := func() string { return dropboxPath(sub(d.DropboxPath)) }
	limit := intOr(d.DropboxLimit, 100)

	switch d.IntegrationOp {
	// ---- browsing ----
	case "list_folder":
		return dropboxRPC(ctx, token, "/files/list_folder", map[string]any{
			"path":      path(),
			"limit":     limit,
			"recursive": strings.EqualFold(sub(d.DropboxRecursive), "true"),
		})

	case "list_folder_continue":
		// Dropbox pages with an opaque cursor rather than an offset.
		if sub(d.DropboxCursor) == "" {
			return "", fmt.Errorf("list_folder_continue needs the cursor from a previous list_folder")
		}
		raw, err := dropboxRPC(ctx, token, "/files/list_folder/continue",
			map[string]any{"cursor": sub(d.DropboxCursor)})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_metadata":
		if path() == "" {
			return "", fmt.Errorf("get_metadata needs a file or folder path")
		}
		return dropboxRPC(ctx, token, "/files/get_metadata", map[string]any{"path": path()})

	case "search":
		if sub(d.DropboxQuery) == "" {
			return "", fmt.Errorf("search needs a query")
		}
		opts := map[string]any{"max_results": limit}
		if p := path(); p != "" {
			opts["path"] = p
		}
		raw, err := dropboxRPC(ctx, token, "/files/search_v2", map[string]any{
			"query":   sub(d.DropboxQuery),
			"options": opts,
		})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	// ---- content ----
	case "download":
		// Text only by design: this returns the file's characters, so a PDF or an
		// image will be unreadable bytes rather than content.
		if path() == "" {
			return "", fmt.Errorf("download needs a file path")
		}
		arg, err := dropboxArgHeader(map[string]any{"path": path()})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			dropboxContentHost+"/files/download", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Dropbox-API-Arg", arg)
		return dropboxDo(req, 12000)

	case "upload":
		if path() == "" {
			return "", fmt.Errorf("upload needs a destination path, e.g. /Reports/summary.txt")
		}
		mode := "add"
		if strings.EqualFold(sub(d.DropboxOverwrite), "true") {
			mode = "overwrite"
		}
		arg, err := dropboxArgHeader(map[string]any{
			"path": path(),
			"mode": mode,
			// autorename avoids a hard failure when adding over an existing name.
			"autorename": mode == "add",
			"mute":       true,
		})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			dropboxContentHost+"/files/upload", strings.NewReader(sub(d.DropboxContent)))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Dropbox-API-Arg", arg)
		req.Header.Set("Content-Type", "application/octet-stream")
		return dropboxDo(req, 0)

	case "get_temporary_link":
		// A short-lived direct URL, which is how a later node can fetch the bytes.
		if path() == "" {
			return "", fmt.Errorf("get_temporary_link needs a file path")
		}
		return dropboxRPC(ctx, token, "/files/get_temporary_link", map[string]any{"path": path()})

	// ---- organising ----
	case "create_folder":
		if path() == "" {
			return "", fmt.Errorf("create_folder needs a path")
		}
		return dropboxRPC(ctx, token, "/files/create_folder_v2", map[string]any{
			"path": path(), "autorename": false,
		})

	case "delete":
		if path() == "" {
			return "", fmt.Errorf("delete needs a path — refusing to act on the account root")
		}
		return dropboxRPC(ctx, token, "/files/delete_v2", map[string]any{"path": path()})

	case "move":
		if path() == "" || sub(d.DropboxToPath) == "" {
			return "", fmt.Errorf("move needs a source and a destination path")
		}
		return dropboxRPC(ctx, token, "/files/move_v2", map[string]any{
			"from_path": path(), "to_path": dropboxPath(sub(d.DropboxToPath)), "autorename": false,
		})

	case "copy":
		if path() == "" || sub(d.DropboxToPath) == "" {
			return "", fmt.Errorf("copy needs a source and a destination path")
		}
		return dropboxRPC(ctx, token, "/files/copy_v2", map[string]any{
			"from_path": path(), "to_path": dropboxPath(sub(d.DropboxToPath)), "autorename": false,
		})

	case "list_revisions":
		if path() == "" {
			return "", fmt.Errorf("list_revisions needs a file path")
		}
		return dropboxRPC(ctx, token, "/files/list_revisions",
			map[string]any{"path": path(), "limit": limit})

	case "restore":
		if path() == "" || sub(d.DropboxRev) == "" {
			return "", fmt.Errorf("restore needs a file path and a revision from list_revisions")
		}
		return dropboxRPC(ctx, token, "/files/restore",
			map[string]any{"path": path(), "rev": sub(d.DropboxRev)})

	// ---- sharing ----
	case "create_shared_link":
		if path() == "" {
			return "", fmt.Errorf("create_shared_link needs a path")
		}
		settings := map[string]any{}
		if v := sub(d.DropboxVisibility); v != "" {
			// Password and team-only visibility need a paid plan; Dropbox says so.
			settings["requested_visibility"] = v
		}
		body := map[string]any{"path": path()}
		if len(settings) > 0 {
			body["settings"] = settings
		}
		return dropboxRPC(ctx, token, "/sharing/create_shared_link_with_settings", body)

	case "list_shared_links":
		body := map[string]any{}
		if p := path(); p != "" {
			body["path"] = p
		}
		return dropboxRPC(ctx, token, "/sharing/list_shared_links", body)

	case "revoke_shared_link":
		if sub(d.DropboxUrl) == "" {
			return "", fmt.Errorf("revoke_shared_link needs the shared link URL")
		}
		return dropboxRPC(ctx, token, "/sharing/revoke_shared_link",
			map[string]any{"url": sub(d.DropboxUrl)})

	case "add_file_member":
		if path() == "" || sub(d.DropboxEmail) == "" {
			return "", fmt.Errorf("add_file_member needs a file path and at least one email")
		}
		// The file must be identified by id, not path, so resolve it first.
		fileID, err := dropboxFileID(ctx, token, path())
		if err != nil {
			return "", err
		}
		members := []any{}
		for _, e := range splitCSV(sub(d.DropboxEmail)) {
			members = append(members, map[string]any{".tag": "email", "email": e})
		}
		return dropboxRPC(ctx, token, "/sharing/add_file_member", map[string]any{
			"file":           fileID,
			"members":        members,
			"access_level":   firstNonEmpty(sub(d.DropboxAccessLevel), "viewer"),
			"quiet":          false,
			"custom_message": sub(d.DropboxMessage),
		})

	case "list_file_members":
		if path() == "" {
			return "", fmt.Errorf("list_file_members needs a file path")
		}
		fileID, err := dropboxFileID(ctx, token, path())
		if err != nil {
			return "", err
		}
		return dropboxRPC(ctx, token, "/sharing/list_file_members",
			map[string]any{"file": fileID, "limit": limit})

	case "share_folder":
		if path() == "" {
			return "", fmt.Errorf("share_folder needs a folder path")
		}
		return dropboxRPC(ctx, token, "/sharing/share_folder", map[string]any{
			"path": path(), "force_async": false,
		})

	// ---- file requests ----
	case "list_file_requests":
		return dropboxRPC(ctx, token, "/file_requests/list_v2", map[string]any{"limit": limit})

	case "create_file_request":
		if sub(d.DropboxTitle) == "" || path() == "" {
			return "", fmt.Errorf("create_file_request needs a title and a destination folder path")
		}
		return dropboxRPC(ctx, token, "/file_requests/create", map[string]any{
			"title":       sub(d.DropboxTitle),
			"destination": path(),
			"open":        true,
		})

	// ---- account ----
	case "get_current_account":
		// One of the few endpoints that takes no arguments at all.
		return dropboxRPC(ctx, token, "/users/get_current_account", nil)

	case "get_space_usage":
		return dropboxRPC(ctx, token, "/users/get_space_usage", nil)

	case "":
		return "", fmt.Errorf("no Dropbox operation selected")
	}
	return "", fmt.Errorf("unsupported Dropbox operation: %s", d.IntegrationOp)
}

// dropboxFileID resolves a path to the file id that the sharing endpoints
// require. They reject a path, so this lookup is not optional.
func dropboxFileID(ctx context.Context, token, path string) (string, error) {
	raw, err := dropboxRPC(ctx, token, "/files/get_metadata", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	id := jsonField(raw, "id")
	if id == "" {
		return "", fmt.Errorf("could not resolve %s to a Dropbox file id", path)
	}
	return id, nil
}
