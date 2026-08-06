package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient("github-user-token", server.Client())
	client.BaseURL = server.URL
	return client
}

func TestInstallationForRepositoryChecksExactSelectedRepository(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer github-user-token" {
			t.Errorf("missing user-token authorization header")
		}
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("missing pinned GitHub API version")
		}
		switch r.URL.Path {
		case "/user/installations":
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"installations": []any{map[string]any{
					"id": 17, "repository_selection": "selected",
					"account": map[string]any{"login": "acme", "type": "Organization"},
				}},
			})
		case "/user/installations/17/repositories":
			writeJSON(t, w, map[string]any{
				"total_count": 1,
				"repositories": []any{map[string]any{
					"id": 91, "full_name": "acme/website", "private": true,
				}},
			})
		default:
			http.NotFound(w, r)
		}
	})

	installation, err := client.InstallationForRepository(context.Background(), "ACME/Website")
	if err != nil {
		t.Fatalf("installed repository was rejected: %v", err)
	}
	if installation.ID != 17 {
		t.Fatalf("installation id = %d, want 17", installation.ID)
	}

	if _, err := client.InstallationForRepository(context.Background(), "acme/payments"); err == nil {
		t.Fatal("same-owner repository outside the selected installation was accepted")
	}
}

func TestInstallationForRepositoryPaginatesInstallationsAndRepositories(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch r.URL.Path {
		case "/user/installations":
			installations := []any{}
			if page == 1 {
				for id := 1; id <= 100; id++ {
					installations = append(installations, map[string]any{
						"id": id, "account": map[string]any{"login": fmt.Sprintf("owner-%d", id)},
					})
				}
			} else if page == 2 {
				installations = append(installations, map[string]any{
					"id": 101, "account": map[string]any{"login": "acme"},
				})
			}
			writeJSON(t, w, map[string]any{"total_count": 101, "installations": installations})
		case "/user/installations/101/repositories":
			repositories := []any{}
			if page == 1 {
				for id := 1; id <= 100; id++ {
					repositories = append(repositories, map[string]any{
						"id": id, "full_name": fmt.Sprintf("acme/repo-%d", id),
					})
				}
			} else if page == 2 {
				repositories = append(repositories, map[string]any{"id": 101, "full_name": "acme/target"})
			}
			writeJSON(t, w, map[string]any{"total_count": 101, "repositories": repositories})
		default:
			// The exact-repository scan checks earlier installations first. They
			// have no repositories, which keeps this test focused on both page-2s.
			if strings.HasPrefix(r.URL.Path, "/user/installations/") {
				writeJSON(t, w, map[string]any{"total_count": 0, "repositories": []any{}})
				return
			}
			http.NotFound(w, r)
		}
	})

	installation, err := client.InstallationForRepository(context.Background(), "acme/target")
	if err != nil {
		t.Fatalf("page-two repository was not found: %v", err)
	}
	if installation.ID != 101 {
		t.Fatalf("installation id = %d, want 101", installation.ID)
	}
}

func TestListInstallationsPreservesGitHubAuthorizationFailure(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})

	_, err := client.ListInstallations(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden || apiErr.Message != "Resource not accessible by integration" {
		t.Fatalf("unexpected API error: %#v", apiErr)
	}
}

func TestSuspendedInstallationCannotCoverRepository(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/installations" {
			t.Fatalf("suspended installation should not have its repositories queried: %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"total_count": 1,
			"installations": []any{map[string]any{
				"id": 17, "account": map[string]any{"login": "acme"},
				"suspended_at": "2026-08-06T10:00:00Z",
			}},
		})
	})

	if _, err := client.InstallationForRepository(context.Background(), "acme/website"); err == nil {
		t.Fatal("suspended installation covered a repository")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode test response: %v", err)
	}
}
