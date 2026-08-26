package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type githubStubFunc func(*http.Request) (*http.Response, error)

func (f githubStubFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func githubStub(t *testing.T, handler func(method, path string, body map[string]any) (int, string)) func() {
	t.Helper()
	previous := integrationHTTP
	integrationHTTP = &http.Client{Transport: githubStubFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &body)
		}
		status, payload := handler(req.Method, req.URL.Path, body)
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(payload)),
		}, nil
	})}
	return func() { integrationHTTP = previous }
}

func TestCreateBranchForksFromBase(t *testing.T) {
	var postedRef, postedSHA string
	defer githubStub(t, func(method, path string, body map[string]any) (int, string) {
		switch {
		case method == http.MethodGet && path == "/repos/acme/widget/git/ref/heads/main":
			return 200, `{"object":{"sha":"basesha"}}`
		case method == http.MethodPost && path == "/repos/acme/widget/git/refs":
			postedRef, _ = body["ref"].(string)
			postedSHA, _ = body["sha"].(string)
			return 201, `{"ref":"refs/heads/fix/thing"}`
		}
		t.Errorf("unexpected call %s %s", method, path)
		return 500, `{}`
	})()

	raw, err := runGithub(context.Background(), "tok", FlowNodeData{
		IntegrationOp: "create_branch",
		GithubRepo:    "acme/widget",
		GithubBranch:  "fix/thing",
	}, nil)
	if err != nil {
		t.Fatalf("create_branch: %v", err)
	}
	if postedRef != "refs/heads/fix/thing" || postedSHA != "basesha" {
		t.Fatalf("posted ref %q at %q, want the base commit", postedRef, postedSHA)
	}
	if !strings.Contains(raw, `"created":true`) {
		t.Fatalf("result = %s", raw)
	}
}

// An agent that retries a step must not wedge on the branch its previous
// attempt already created.
func TestCreateBranchIsIdempotent(t *testing.T) {
	defer githubStub(t, func(method, path string, _ map[string]any) (int, string) {
		switch {
		case method == http.MethodGet && path == "/repos/acme/widget/git/ref/heads/main":
			return 200, `{"object":{"sha":"basesha"}}`
		case method == http.MethodGet && path == "/repos/acme/widget/git/ref/heads/fix/thing":
			return 200, `{"object":{"sha":"existingsha"}}`
		case method == http.MethodPost:
			return 422, `{"message":"Reference already exists"}`
		}
		return 500, `{}`
	})()

	raw, err := runGithub(context.Background(), "tok", FlowNodeData{
		IntegrationOp: "create_branch",
		GithubRepo:    "acme/widget",
		GithubBranch:  "fix/thing",
	}, nil)
	if err != nil {
		t.Fatalf("an existing branch should satisfy create_branch, got: %v", err)
	}
	if !strings.Contains(raw, `"created":false`) || !strings.Contains(raw, "existingsha") {
		t.Fatalf("result = %s", raw)
	}
}

func TestCommitFilesWritesOneCommitForEveryFile(t *testing.T) {
	var tree map[string]any
	var commit map[string]any
	var movedTo string

	defer githubStub(t, func(method, path string, body map[string]any) (int, string) {
		switch {
		case method == http.MethodGet && path == "/repos/acme/widget/git/ref/heads/fix/thing":
			return 200, `{"object":{"sha":"headsha"}}`
		case method == http.MethodGet && path == "/repos/acme/widget/git/commits/headsha":
			return 200, `{"tree":{"sha":"basetree"}}`
		case method == http.MethodPost && path == "/repos/acme/widget/git/trees":
			tree = body
			return 201, `{"sha":"newtree"}`
		case method == http.MethodPost && path == "/repos/acme/widget/git/commits":
			commit = body
			return 201, `{"sha":"newcommit","html_url":"https://github.com/acme/widget/commit/newcommit"}`
		case method == http.MethodPatch && path == "/repos/acme/widget/git/refs/heads/fix/thing":
			movedTo, _ = body["sha"].(string)
			return 200, `{"object":{"sha":"newcommit"}}`
		}
		t.Errorf("unexpected call %s %s", method, path)
		return 500, `{}`
	})()

	raw, err := runGithub(context.Background(), "tok", FlowNodeData{
		IntegrationOp:   "commit_files",
		GithubRepo:      "acme/widget",
		GithubBranch:    "fix/thing",
		GithubCommitMsg: "Fix the thing",
		GithubFiles: `[
			{"path":"a.go","content":"package a"},
			{"path":"scripts/run.sh","content":"#!/bin/sh","executable":true},
			{"path":"old.go","deleted":true}
		]`,
	}, nil)
	if err != nil {
		t.Fatalf("commit_files: %v", err)
	}

	if tree["base_tree"] != "basetree" {
		t.Fatalf("tree not based on the branch head: %#v", tree["base_tree"])
	}
	entries, _ := tree["tree"].([]any)
	if len(entries) != 3 {
		t.Fatalf("sent %d tree entries, want one per file", len(entries))
	}
	first, _ := entries[0].(map[string]any)
	if first["content"] != "package a" || first["mode"] != "100644" {
		t.Errorf("plain file entry = %#v", first)
	}
	second, _ := entries[1].(map[string]any)
	if second["mode"] != "100755" {
		t.Errorf("executable should be mode 100755, got %#v", second["mode"])
	}
	third, _ := entries[2].(map[string]any)
	sha, present := third["sha"]
	if !present || sha != nil {
		t.Errorf("a deletion must send a null sha, got %#v (present=%v)", sha, present)
	}
	if _, hasContent := third["content"]; hasContent {
		t.Error("a deletion must not carry content")
	}

	// One commit, one parent — not one commit per file.
	parents, _ := commit["parents"].([]any)
	if len(parents) != 1 || parents[0] != "headsha" {
		t.Errorf("parents = %#v, want the previous head", parents)
	}
	if commit["message"] != "Fix the thing" || commit["tree"] != "newtree" {
		t.Errorf("commit = %#v", commit)
	}
	if movedTo != "newcommit" {
		t.Errorf("branch moved to %q, want the new commit", movedTo)
	}
	if !strings.Contains(raw, `"files":3`) || !strings.Contains(raw, "newcommit") {
		t.Errorf("result = %s", raw)
	}
}

// The branch must not move if any earlier step fails: everything before the
// ref update creates unreferenced objects, so a failure leaves the branch as
// it was rather than half-updated.
func TestCommitFilesLeavesBranchAloneWhenTheCommitFails(t *testing.T) {
	moved := false
	defer githubStub(t, func(method, path string, _ map[string]any) (int, string) {
		switch {
		case method == http.MethodGet && path == "/repos/acme/widget/git/ref/heads/main":
			return 200, `{"object":{"sha":"headsha"}}`
		case method == http.MethodGet && path == "/repos/acme/widget/git/commits/headsha":
			return 200, `{"tree":{"sha":"basetree"}}`
		case method == http.MethodPost && path == "/repos/acme/widget/git/trees":
			return 201, `{"sha":"newtree"}`
		case method == http.MethodPost && path == "/repos/acme/widget/git/commits":
			return 500, `{"message":"boom"}`
		case method == http.MethodPatch:
			moved = true
			return 200, `{}`
		}
		return 500, `{}`
	})()

	if _, err := runGithub(context.Background(), "tok", FlowNodeData{
		IntegrationOp: "commit_files",
		GithubRepo:    "acme/widget",
		GithubBranch:  "main",
		GithubFiles:   `[{"path":"a.go","content":"x"}]`,
	}, nil); err == nil {
		t.Fatal("a failed commit should fail the operation")
	}
	if moved {
		t.Fatal("the branch was moved despite the commit failing")
	}
}

func TestParseGithubFilesRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"empty", "", "JSON array"},
		{"not an array", `{"path":"a"}`, "JSON array"},
		{"no files", `[]`, "nothing to commit"},
		{"missing path", `[{"content":"x"}]`, "no path"},
		{"absolute path", `[{"path":"/etc/passwd","content":"x"}]`, "relative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseGithubFiles(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}

	// Fenced JSON is what a model emits when told not to fence it.
	files, err := parseGithubFiles("```json\n[{\"path\":\"a.go\",\"content\":\"x\"}]\n```")
	if err != nil || len(files) != 1 || files[0].Path != "a.go" {
		t.Fatalf("fenced input: files=%#v err=%v", files, err)
	}
}
