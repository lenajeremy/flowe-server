package handlers

import (
	"strings"
	"testing"
)

// Do the scopes we ask for match the APIs we call?
//
// The bug these exist for: every Confluence operation but three runs against the
// v2 API (/wiki/api/v2), v2 refuses classic scopes, and we were asking for
// classic ones. The connection succeeded, the token was valid, and then every
// call came back `401 {"code":401,"message":"Unauthorized; scope does not
// match"}` — including the space picker, which is the first thing a user touches
// after connecting.
//
// This is a table, not a network test, and deliberately so. Atlassian's
// /authorize endpoint redirects to a login page BEFORE it validates scopes — a
// deliberately invented scope name gets the same 302 as a real one — so probing
// it proves only that the host is up. What can be pinned is the mapping itself,
// taken from Atlassian's per-endpoint reference, so changes to the requested
// scope set cannot silently drift away from the operations documented here.

// atlassianScopeStyle reports whether a scope name is granular
// (verb:resource:product) or classic (verb:product-resource).
func atlassianScopeStyle(scope string) string {
	switch {
	case scope == "offline_access":
		return "special"
	case strings.Count(scope, ":") == 2:
		return "granular"
	default:
		return "classic"
	}
}

func TestConfluenceAsksOnlyForGranularScopes(t *testing.T) {
	// The endpoint references for every shipped operation specify granular
	// scopes. Keeping a classic scope here would reintroduce the mismatch that
	// made the v2 calls fail after an apparently successful connection.
	for _, scope := range atlassianScopes["confluence"] {
		if style := atlassianScopeStyle(scope); style == "classic" {
			t.Errorf("%q is classic, but the shipped endpoints document granular scopes", scope)
		}
	}
}

func TestEveryConfluenceOperationHasItsScope(t *testing.T) {
	// Straight from Atlassian's endpoint reference. Left side is the op in
	// executor/confluence.go, right side is what that endpoint documents.
	need := map[string][]string{
		"list_spaces (v2 GET /spaces)":                   {"read:space:confluence"},
		"get_space (v2 GET /spaces/{id})":                {"read:space:confluence"},
		"resource picker (v2 GET /spaces)":               {"read:space:confluence"},
		"list_pages (v2 GET /pages)":                     {"read:page:confluence"},
		"get_page (v2 GET /pages/{id})":                  {"read:page:confluence"},
		"find_page_by_title (v2 GET /pages)":             {"read:page:confluence"},
		"list_child_pages (v2 GET /pages/{id}/children)": {"read:page:confluence"},
		"create_page (v2 POST /pages)":                   {"write:page:confluence"},
		"update_page (v2 PUT /pages/{id})":               {"write:page:confluence"},
		"delete_page (v2 DELETE /pages/{id})":            {"delete:page:confluence"},
		// Despite their names, the v2 blogpost endpoints document the page
		// scopes, not read/write:blogpost:confluence.
		"list_blog_posts (v2 GET /blogposts)":               {"read:page:confluence"},
		"create_blog_post (v2 POST /blogposts)":             {"write:page:confluence"},
		"list_comments (v2 GET /footer-comments)":           {"read:comment:confluence"},
		"add_comment (v2 POST /footer-comments)":            {"write:comment:confluence"},
		"list_labels (v2 GET /pages/{id}/labels)":           {"read:label:confluence"},
		"add_label (v1 POST /content/{id}/label)":           {"read:label:confluence", "write:label:confluence"},
		"list_attachments (v2 GET /pages/{id}/attachments)": {"read:attachment:confluence"},
		"upload_attachment (v1 PUT /child/attachment)":      {"write:attachment:confluence", "read:content-details:confluence"},
		"search_pages (v1 GET /search?cql=)":                {"read:content-details:confluence"},
		"get_current_user (v1 GET /user/current)":           {"read:content-details:confluence"},
	}

	granted := map[string]bool{}
	for _, s := range atlassianScopes["confluence"] {
		granted[s] = true
	}
	for op, scopes := range need {
		for _, s := range scopes {
			if !granted[s] {
				t.Errorf("%s needs %q, which is not requested — the call will 401 with "+
					"\"scope does not match\"", op, s)
			}
		}
	}
}

func TestConfluenceAsksForNothingItDoesNotUse(t *testing.T) {
	// The other direction: an unused scope is a consent screen asking for more
	// than the product does, which is the thing that makes an admin say no.
	used := map[string]bool{
		"offline_access": true, "read:space:confluence": true,
		"read:page:confluence": true, "write:page:confluence": true,
		"delete:page:confluence": true, "read:comment:confluence": true,
		"write:comment:confluence": true, "read:label:confluence": true,
		"write:label:confluence": true, "read:attachment:confluence": true,
		"write:attachment:confluence": true, "read:content-details:confluence": true,
	}
	for _, s := range atlassianScopes["confluence"] {
		if !used[s] {
			t.Errorf("%q is requested but no operation needs it", s)
		}
	}
}

func TestTheScopeMismatchErrorTellsYouWhatToDo(t *testing.T) {
	// A user reading "Unauthorized; scope does not match" has no way to know the
	// fix is to reconnect. Everything else must pass through untouched.
	raw := errString(`provider returned 401: {"code":401,"message":"Unauthorized; scope does not match"}`)
	got := translateAtlassianScopeError(raw)
	if got == nil || strings.Contains(got.Error(), "scope does not match") {
		t.Errorf("the raw Atlassian wording survived: %v", got)
	}
	if !strings.Contains(got.Error(), "reconnect") {
		t.Errorf("the message does not say to reconnect: %v", got)
	}

	other := errString("provider returned 500: upstream exploded")
	if translateAtlassianScopeError(other).Error() != other.Error() {
		t.Error("an unrelated error was rewritten")
	}
	if translateAtlassianScopeError(nil) != nil {
		t.Error("nil became an error")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
