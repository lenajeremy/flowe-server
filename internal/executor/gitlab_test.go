package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitLabCreateBranchUsesRequestedBranchAndBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/85184925/repository/branches" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("branch") != "docs/readme" || r.URL.Query().Get("ref") != "master" {
			t.Fatalf("unexpected branch query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"docs/readme","commit":{"id":"abc123"}}`))
	}))
	defer server.Close()
	previousURL, previousClient := gitlabAPIURL, integrationHTTP
	gitlabAPIURL, integrationHTTP = server.URL, server.Client()
	defer func() { gitlabAPIURL, integrationHTTP = previousURL, previousClient }()

	result, err := runGitlab(context.Background(), "token", FlowNodeData{
		IntegrationOp: "create_branch", GitlabProjectId: "85184925",
		GitlabSourceBranch: "docs/readme", GitlabTargetBranch: "master",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(result), &decoded) != nil || decoded["status"] != "created" || decoded["sha"] != "abc123" {
		t.Fatalf("unexpected result: %s", result)
	}
}
