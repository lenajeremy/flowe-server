package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Chat API v1.
//
// Chat is a Google Workspace service: a personal @gmail.com account can use Chat
// in the UI but cannot authorize this API, and the Cloud project needs the Chat
// API both enabled and *configured* (app name, avatar, description) before user
// credentials work. Both failures come back as 403, so chatError names them
// rather than leaving a bare "permission denied".
//
// Also note that user-credential calls only reach spaces the authenticated user
// is a member of — there is no way to post into a space they haven't joined.

const chatAPI = "https://chat.googleapis.com/v1"

// chatSpaceName normalizes a space id to its resource name.
func chatSpaceName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "spaces/") {
		return v
	}
	return "spaces/" + v
}

// chatError annotates the two setup problems that both surface as a 403.
func chatError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "PERMISSION_DENIED") {
		return fmt.Errorf("%w — Google Chat needs a Workspace account (not a personal "+
			"@gmail.com one) and a configured Chat app in the Cloud project", err)
	}
	return err
}

func runGoogleChat(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := intOr(d.ChatLimit, 25)
	space := func() string { return chatSpaceName(sub(d.ChatSpace)) }

	call := func(method, path string, body any) (string, error) {
		out, err := googleCall(ctx, token, method, chatAPI+path, body)
		return out, chatError(err)
	}

	switch d.IntegrationOp {
	// ---- spaces ----
	case "list_spaces":
		q := url.Values{"pageSize": {fmt.Sprint(limit)}}
		if f := sub(d.ChatFilter); f != "" {
			q.Set("filter", f)
		}
		raw, err := call(http.MethodGet, "/spaces?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_space":
		if space() == "" {
			return "", fmt.Errorf("get_space needs a space")
		}
		return call(http.MethodGet, "/"+space(), nil)

	case "create_space":
		if sub(d.ChatDisplayName) == "" {
			return "", fmt.Errorf("create_space needs a name")
		}
		return call(http.MethodPost, "/spaces", map[string]any{
			"displayName": sub(d.ChatDisplayName),
			"spaceType":   firstNonEmpty(strings.ToUpper(sub(d.ChatSpaceType)), "SPACE"),
		})

	case "setup_space":
		// spaces.setup creates the space and adds members in one call, which is
		// what a workflow wants — spaces.create leaves an empty room.
		if sub(d.ChatDisplayName) == "" {
			return "", fmt.Errorf("setup_space needs a name")
		}
		memberships := []any{}
		for _, email := range splitCSV(sub(d.ChatMemberEmail)) {
			memberships = append(memberships, map[string]any{
				"member": map[string]any{"name": "users/" + email, "type": "HUMAN"},
			})
		}
		return call(http.MethodPost, "/spaces:setup", map[string]any{
			"space": map[string]any{
				"displayName": sub(d.ChatDisplayName),
				"spaceType":   firstNonEmpty(strings.ToUpper(sub(d.ChatSpaceType)), "SPACE"),
			},
			"memberships": memberships,
		})

	case "update_space":
		if sub(d.ChatDisplayName) == "" {
			return "", fmt.Errorf("update_space needs a new name")
		}
		return call(http.MethodPatch, "/"+space()+"?updateMask=displayName",
			map[string]any{"displayName": sub(d.ChatDisplayName)})

	case "delete_space":
		if space() == "" {
			return "", fmt.Errorf("delete_space needs a space")
		}
		if _, err := call(http.MethodDelete, "/"+space(), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deletedSpace":%q}`, space()), nil

	case "find_direct_message":
		// The DM space with one person, which has no stable id to look up.
		if sub(d.ChatMemberEmail) == "" {
			return "", fmt.Errorf("find_direct_message needs a user's email")
		}
		return call(http.MethodGet,
			"/spaces:findDirectMessage?name="+url.QueryEscape("users/"+sub(d.ChatMemberEmail)), nil)

	// ---- messages ----
	case "send_message":
		if space() == "" || sub(d.ChatText) == "" {
			return "", fmt.Errorf("send_message needs a space and message text")
		}
		path := "/" + space() + "/messages"
		body := map[string]any{"text": sub(d.ChatText)}
		if t := sub(d.ChatThread); t != "" {
			// A thread key groups messages without knowing the thread's name.
			body["thread"] = map[string]any{"threadKey": t}
			path += "?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
		}
		return call(http.MethodPost, path, body)

	case "reply_in_thread":
		if space() == "" || sub(d.ChatThread) == "" {
			return "", fmt.Errorf("reply_in_thread needs a space and a thread")
		}
		thread := sub(d.ChatThread)
		body := map[string]any{"text": sub(d.ChatText)}
		// A full resource name is a known thread; anything else is a key.
		if strings.Contains(thread, "/threads/") {
			body["thread"] = map[string]any{"name": thread}
		} else {
			body["thread"] = map[string]any{"threadKey": thread}
		}
		return call(http.MethodPost,
			"/"+space()+"/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD", body)

	case "get_message":
		if sub(d.ChatMessageId) == "" {
			return "", fmt.Errorf("get_message needs a message name")
		}
		return call(http.MethodGet, "/"+sub(d.ChatMessageId), nil)

	case "update_message":
		if sub(d.ChatMessageId) == "" || sub(d.ChatText) == "" {
			return "", fmt.Errorf("update_message needs a message name and new text")
		}
		return call(http.MethodPatch, "/"+sub(d.ChatMessageId)+"?updateMask=text",
			map[string]any{"text": sub(d.ChatText)})

	case "delete_message":
		if sub(d.ChatMessageId) == "" {
			return "", fmt.Errorf("delete_message needs a message name")
		}
		if _, err := call(http.MethodDelete, "/"+sub(d.ChatMessageId), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"deletedMessage":%q}`, sub(d.ChatMessageId)), nil

	case "list_messages":
		if space() == "" {
			return "", fmt.Errorf("list_messages needs a space")
		}
		q := url.Values{"pageSize": {fmt.Sprint(limit)}, "orderBy": {"createTime desc"}}
		if f := sub(d.ChatFilter); f != "" {
			q.Set("filter", f)
		}
		raw, err := call(http.MethodGet, "/"+space()+"/messages?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "add_reaction":
		if sub(d.ChatMessageId) == "" || sub(d.ChatEmoji) == "" {
			return "", fmt.Errorf("add_reaction needs a message name and an emoji")
		}
		// Chat wants the emoji character itself, not a :shortcode:.
		return call(http.MethodPost, "/"+sub(d.ChatMessageId)+"/reactions",
			map[string]any{"emoji": map[string]any{"unicode": strings.Trim(sub(d.ChatEmoji), ":")}})

	// ---- membership ----
	case "list_members":
		if space() == "" {
			return "", fmt.Errorf("list_members needs a space")
		}
		raw, err := call(http.MethodGet, fmt.Sprintf("/%s/members?pageSize=%d", space(), limit), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "add_member":
		if space() == "" || sub(d.ChatMemberEmail) == "" {
			return "", fmt.Errorf("add_member needs a space and a user's email")
		}
		return call(http.MethodPost, "/"+space()+"/members", map[string]any{
			"member": map[string]any{"name": "users/" + sub(d.ChatMemberEmail), "type": "HUMAN"},
		})

	case "remove_member":
		if sub(d.ChatMembership) == "" {
			return "", fmt.Errorf("remove_member needs a membership name from list_members")
		}
		if _, err := call(http.MethodDelete, "/"+sub(d.ChatMembership), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"removed":%q}`, sub(d.ChatMembership)), nil

	case "":
		return "", fmt.Errorf("no Google Chat operation selected")
	}
	return "", fmt.Errorf("unsupported Google Chat operation: %s", d.IntegrationOp)
}
