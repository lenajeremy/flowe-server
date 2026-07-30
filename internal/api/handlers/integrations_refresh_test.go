package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"
)

// GitHub App user tokens expire after 8 hours. Before refresh support, the
// connection silently died overnight and every scheduled run 401'd — so this
// covers the exchange end to end against a stub GitHub: JSON is requested,
// grant_type/refresh_token are sent, and the rotated credentials come back.
func TestRefreshConnectionGithub(t *testing.T) {
	var gotForm url.Values
	var gotAccept string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":             "ghu_new_access",
			"expires_in":               28800, // 8h, as GitHub Apps issue
			"refresh_token":            "ghr_rotated",
			"refresh_token_expires_in": 15897600,
			"token_type":               "bearer",
		})
	}))
	defer srv.Close()

	// Point the github provider at the stub for this test only.
	orig := refreshTokenEndpoints["github"]
	ep := orig
	ep.tokenURL = srv.URL
	refreshTokenEndpoints["github"] = ep
	t.Cleanup(func() { refreshTokenEndpoints["github"] = orig })

	expired := time.Now().Add(-time.Minute)
	conn := &models.IntegrationConnection{
		Provider:     "github",
		AccessToken:  "ghu_old_access",
		RefreshToken: "ghr_old",
		ExpiresAt:    &expired,
	}

	tok, supported, err := exchangeRefreshToken(conn)
	if err != nil {
		t.Fatalf("exchangeRefreshToken: %v", err)
	}
	if !supported {
		t.Fatal("github should support the refresh grant")
	}

	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q, want application/json — GitHub replies form-encoded without it", gotAccept)
	}
	if g := gotForm.Get("grant_type"); g != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", g)
	}
	if g := gotForm.Get("refresh_token"); g != "ghr_old" {
		t.Errorf("sent refresh_token = %q, want the stored one", g)
	}
	if tok.AccessToken != "ghu_new_access" {
		t.Errorf("access token = %q, want the refreshed one", tok.AccessToken)
	}
	if tok.RefreshToken != "ghr_rotated" {
		t.Errorf("refresh token = %q, want the rotated one (GitHub rotates on every use)", tok.RefreshToken)
	}
	if tok.ExpiresIn != 28800 {
		t.Errorf("expires_in = %d, want 28800 (8h, the GitHub App user-token lifetime)", tok.ExpiresIn)
	}
}

// GitHub reports refresh failures with HTTP 200 and an error body, so a spent
// refresh token must surface as an error rather than a silent empty token.
func TestRefreshConnectionGithubRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "bad_refresh_token",
			"error_description": "The refresh token passed is incorrect or expired.",
		})
	}))
	defer srv.Close()

	orig := refreshTokenEndpoints["github"]
	ep := orig
	ep.tokenURL = srv.URL
	refreshTokenEndpoints["github"] = ep
	t.Cleanup(func() { refreshTokenEndpoints["github"] = orig })

	expired := time.Now().Add(-time.Minute)
	conn := &models.IntegrationConnection{
		Provider: "github", AccessToken: "ghu_old", RefreshToken: "ghr_spent", ExpiresAt: &expired,
	}
	if _, _, err := exchangeRefreshToken(conn); err == nil {
		t.Fatal("expected an error when GitHub rejects the refresh token")
	}
}

// Providers with no refresh flow must pass through untouched rather than
// attempting an exchange.
func TestRefreshConnectionUnsupportedProvider(t *testing.T) {
	conn := &models.IntegrationConnection{Provider: "notion", AccessToken: "secret_abc"}
	_, supported, err := exchangeRefreshToken(conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if supported {
		t.Error("notion has no refresh flow and must not attempt an exchange")
	}
}
