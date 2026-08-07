package handlers

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type confluenceResourcesRoundTripFunc func(*http.Request) (*http.Response, error)

func (f confluenceResourcesRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestConfluenceResourcesIncludesPagesAndPaginates(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()

	http.DefaultTransport = confluenceResourcesRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer confluence-token" {
			t.Errorf("authorization = %q", req.Header.Get("Authorization"))
		}
		body := ""
		switch {
		case strings.HasSuffix(req.URL.Path, "/spaces"):
			body = `{"results":[{"id":"space-1","key":"ENG","name":"Engineering"}]}`
		case strings.HasSuffix(req.URL.Path, "/pages") && req.URL.Query().Get("cursor") == "":
			body = `{"results":[{"id":"101","title":"Runbook","spaceId":"space-1"},{"id":"102","title":"Company"}],"_links":{"next":"/wiki/api/v2/pages?cursor=next-page"}}`
		case strings.HasSuffix(req.URL.Path, "/pages") && req.URL.Query().Get("cursor") == "next-page":
			body = `{"results":[{"id":"103","title":"Architecture","spaceId":"space-1"}],"_links":{}}`
		default:
			t.Fatalf("unexpected Atlassian request: %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	resources, err := confluenceResources("confluence-token", "cloud-123")
	if err != nil {
		t.Fatalf("confluenceResources: %v", err)
	}
	if len(resources) != 4 {
		t.Fatalf("resources = %+v, want one space and three pages", resources)
	}
	want := []integrationResource{
		{ID: "ENG", Name: "Engineering", Type: "space"},
		{ID: "101", Name: "Runbook — Engineering", Type: "page"},
		{ID: "102", Name: "Company", Type: "page"},
		{ID: "103", Name: "Architecture — Engineering", Type: "page"},
	}
	for i := range want {
		if resources[i] != want[i] {
			t.Errorf("resource %d = %+v, want %+v", i, resources[i], want[i])
		}
	}
}
