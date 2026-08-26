package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func redirectGitLabAPITo(url string) func() {
	previous := gitlabAPIBase
	gitlabAPIBase = url
	return func() { gitlabAPIBase = previous }
}

func TestGitLabProjectFiltersCanPickBranchesAndMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer oauth-token" {
			t.Errorf("missing OAuth bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/123/repository/branches":
			_, _ = w.Write([]byte(`[{"name":"main"},{"name":"release/1.x"}]`))
		case "/projects/123/members/all":
			_, _ = w.Write([]byte(`[{"name":"Alex Garcia","username":"agarcia"},{"name":"robot","username":"robot"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	restore := redirectGitLabAPITo(srv.URL)
	defer restore()

	branches, err := gitlabBranchResources("oauth-token", "123")
	if err != nil || len(branches) != 2 {
		t.Fatalf("branches: %#v / %v", branches, err)
	}
	if branches[1].ID != "release/1.x" || branches[1].Type != "branch" {
		t.Errorf("branch resource = %#v", branches[1])
	}

	members, err := gitlabMemberResources("oauth-token", "123")
	if err != nil || len(members) != 2 {
		t.Fatalf("members: %#v / %v", members, err)
	}
	if members[0].ID != "agarcia" || members[0].Name != "Alex Garcia (@agarcia)" || members[0].Type != "user" {
		t.Errorf("member resource = %#v", members[0])
	}
}

func TestGitLabChildResourcesRejectNonNumericProjectIDs(t *testing.T) {
	if _, err := gitlabBranchResources("token", "acme/widgets"); err == nil {
		t.Fatal("a project path was accepted even though webhook payloads route by numeric project id")
	}
	if _, err := gitlabMemberResources("token", "0"); err == nil {
		t.Fatal("project id zero was accepted")
	}
}

func TestGitLabRepositoryResourcesIncludeEveryPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			projects := make([]map[string]any, 100)
			for index := range projects {
				projects[index] = map[string]any{"id": index + 1, "path_with_namespace": fmt.Sprintf("acme/project-%d", index+1)}
			}
			_ = json.NewEncoder(w).Encode(projects)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "path_with_namespace": "acme/project-101"}})
	}))
	defer srv.Close()
	restore := redirectGitLabAPITo(srv.URL)
	defer restore()

	repositories, err := gitlabResources("oauth-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 101 || repositories[100].ID != "101" || repositories[100].Name != "acme/project-101" {
		t.Fatalf("paginated repositories = %#v", repositories)
	}
}
