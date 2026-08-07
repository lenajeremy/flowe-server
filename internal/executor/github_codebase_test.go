package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type githubCodebaseRoundTripFunc func(*http.Request) (*http.Response, error)

func (f githubCodebaseRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func githubCodebaseResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGithubListRepoTreeFiltersAndBoundsResponse(t *testing.T) {
	previous := integrationHTTP
	defer func() { integrationHTTP = previous }()

	integrationHTTP = &http.Client{Transport: githubCodebaseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", req.Method)
		}
		if req.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("authorization = %q", req.Header.Get("Authorization"))
		}
		if req.URL.EscapedPath() != "/repos/acme/widget/git/trees/feature%2Fsearch" {
			t.Errorf("path = %q", req.URL.EscapedPath())
		}
		if req.URL.Query().Get("recursive") != "1" {
			t.Errorf("recursive = %q, want 1", req.URL.Query().Get("recursive"))
		}
		return githubCodebaseResponse(http.StatusOK, `{
			"sha":"tree-sha","truncated":false,"tree":[
				{"path":"docs","mode":"040000","type":"tree"},
				{"path":"docs/run.sh","mode":"100755","type":"blob","size":42},
				{"path":"docs/guide.md","mode":"100644","type":"blob","size":1200},
				{"path":"src/main.go","mode":"100644","type":"blob","size":800}
			]
		}`), nil
	})}

	raw, err := runGithub(context.Background(), "installation-token", FlowNodeData{
		IntegrationOp:   "list_repo_tree",
		GithubRepo:      "acme/widget",
		GithubRef:       "feature/search",
		GithubPath:      "/docs/",
		GithubTreeLimit: 2,
	}, nil)
	if err != nil {
		t.Fatalf("runGithub: %v", err)
	}
	var result struct {
		Repository        string `json:"repository"`
		Ref               string `json:"ref"`
		PathPrefix        string `json:"path_prefix"`
		MatchingEntries   int    `json:"matching_entries"`
		ReturnedEntries   int    `json:"returned_entries"`
		Truncated         bool   `json:"truncated"`
		ProviderTruncated bool   `json:"provider_truncated"`
		Note              string `json:"note"`
		Entries           []struct {
			Path       string `json:"path"`
			Type       string `json:"type"`
			SizeBytes  int64  `json:"size_bytes"`
			Executable bool   `json:"executable"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, raw)
	}
	if result.Repository != "acme/widget" || result.Ref != "feature/search" || result.PathPrefix != "docs" {
		t.Errorf("unexpected identity fields: %+v", result)
	}
	if result.MatchingEntries != 3 || result.ReturnedEntries != 2 || !result.Truncated || result.ProviderTruncated {
		t.Errorf("unexpected bounds: %+v", result)
	}
	if result.Note == "" {
		t.Error("bounded response should tell the agent how to narrow it")
	}
	if len(result.Entries) != 2 || result.Entries[1].Path != "docs/run.sh" || !result.Entries[1].Executable || result.Entries[1].SizeBytes != 42 {
		t.Errorf("unexpected entries: %+v", result.Entries)
	}
}

func TestGithubListRepoTreeResolvesRepositoryDefaultBranch(t *testing.T) {
	previous := integrationHTTP
	defer func() { integrationHTTP = previous }()

	requests := 0
	integrationHTTP = &http.Client{Transport: githubCodebaseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch req.URL.Path {
		case "/repos/acme/widget":
			return githubCodebaseResponse(http.StatusOK, `{"default_branch":"develop"}`), nil
		case "/repos/acme/widget/git/trees/develop":
			return githubCodebaseResponse(http.StatusOK, `{"sha":"tree-sha","tree":[]}`), nil
		default:
			t.Fatalf("unexpected request path %s", req.URL.Path)
			return nil, nil
		}
	})}

	raw, err := runGithub(context.Background(), "installation-token", FlowNodeData{
		IntegrationOp: "list_repo_tree",
		GithubRepo:    "acme/widget",
	}, nil)
	if err != nil {
		t.Fatalf("runGithub: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want repository metadata + tree", requests)
	}
	var result struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Ref != "develop" {
		t.Errorf("ref = %q, want repository default branch", result.Ref)
	}
}

func TestGithubGetRepoDetailsCombinesMetadataAndLanguages(t *testing.T) {
	previous := integrationHTTP
	defer func() { integrationHTTP = previous }()

	requests := 0
	integrationHTTP = &http.Client{Transport: githubCodebaseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("authorization = %q", req.Header.Get("Authorization"))
		}
		switch req.URL.Path {
		case "/repos/acme/widget":
			return githubCodebaseResponse(http.StatusOK, `{
				"full_name":"acme/widget","description":"A useful widget","homepage":"https://widget.test",
				"html_url":"https://github.com/acme/widget","visibility":"private","private":true,
				"default_branch":"develop","language":"Go","topics":["automation","agents"],"size":512,
				"stargazers_count":7,"forks_count":2,"open_issues_count":3,"subscribers_count":4,
				"has_issues":true,"has_wiki":false,"has_pages":true,"has_discussions":true,
				"owner":{"login":"acme"},"license":{"key":"mit","name":"MIT License","spdx_id":"MIT"}
			}`), nil
		case "/repos/acme/widget/languages":
			return githubCodebaseResponse(http.StatusOK, `{"Go":9000,"TypeScript":1200}`), nil
		default:
			t.Fatalf("unexpected request path %s", req.URL.Path)
			return nil, nil
		}
	})}

	raw, err := runGithub(context.Background(), "installation-token", FlowNodeData{
		IntegrationOp: "get_repo_details",
		GithubRepo:    "acme/widget",
	}, nil)
	if err != nil {
		t.Fatalf("runGithub: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want repository + languages", requests)
	}
	var result struct {
		Repository      string           `json:"repository"`
		Owner           string           `json:"owner"`
		DefaultBranch   string           `json:"default_branch"`
		PrimaryLanguage string           `json:"primary_language"`
		Languages       map[string]int64 `json:"languages_bytes"`
		Features        map[string]bool  `json:"features"`
		License         struct {
			SPDX string `json:"spdx_id"`
		} `json:"license"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, raw)
	}
	if result.Repository != "acme/widget" || result.Owner != "acme" || result.DefaultBranch != "develop" {
		t.Errorf("unexpected repository details: %+v", result)
	}
	if result.PrimaryLanguage != "Go" || result.Languages["Go"] != 9000 || !result.Features["discussions"] || result.License.SPDX != "MIT" {
		t.Errorf("unexpected repository enrichment: %+v", result)
	}
}
