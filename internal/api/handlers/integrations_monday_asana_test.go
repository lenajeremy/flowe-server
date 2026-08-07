package handlers

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/triggers"
)

func TestMondayAndAsanaOAuthConfiguration(t *testing.T) {
	for _, provider := range []string{"monday", "asana"} {
		if _, ok := oauthProviders[provider]; !ok {
			t.Fatalf("%s missing from OAuth providers", provider)
		}
		if !pkceProviders[provider] {
			t.Fatalf("%s must use PKCE", provider)
		}
		if _, ok := refreshTokenEndpoints[provider]; !ok {
			t.Fatalf("%s missing refresh-token endpoint", provider)
		}
	}
	if got := oauthProviders["monday"].extraAuthQ.Get("scope"); !strings.Contains(got, "webhooks:write") || !strings.Contains(got, "boards:write") {
		t.Fatalf("monday scopes = %q", got)
	}
	if oauthProviders["monday"].extraAuthQ.Get("force_install_if_needed") != "true" {
		t.Fatal("monday OAuth must install the app before authorization when needed")
	}
	if got := oauthProviders["asana"].extraAuthQ.Get("scope"); !strings.Contains(got, "webhooks:write") || !strings.Contains(got, "webhooks:delete") || !strings.Contains(got, "tasks:write") {
		t.Fatalf("Asana scopes = %q", got)
	}
	if !refreshTokenEndpoints["monday"].jsonBody {
		t.Fatal("monday refresh grant must be JSON")
	}
	if refreshTokenEndpoints["asana"].jsonBody {
		t.Fatal("Asana refresh grant must be form encoded")
	}
}

func TestAIBuilderCatalogIncludesMondayAndAsanaActionsAndTriggers(t *testing.T) {
	for _, provider := range []string{"monday", "asana"} {
		entry := catalogEntry(provider)
		if entry == nil {
			t.Fatalf("AI node catalog is missing %s", provider)
		}
		fields, ok := entry["dataFields"].(map[string]any)
		if !ok || !strings.Contains(fields["integrationOp"].(string), "create") {
			t.Fatalf("AI node catalog has no usable %s operation schema: %#v", provider, fields)
		}
		if len(triggers.Catalog()[provider]) == 0 {
			t.Fatalf("AI trigger catalog is missing %s events", provider)
		}
	}
}

func TestMondayExpiryFallsBackToJWTExp(t *testing.T) {
	expected := time.Now().Add(time.Hour).Truncate(time.Second)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, _ := json.Marshal(map[string]any{"exp": expected.Unix()})
	token := header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	connection := &models.IntegrationConnection{}
	setOAuthExpiry(connection, token, 0)
	if connection.ExpiresAt == nil || !connection.ExpiresAt.Equal(expected) {
		t.Fatalf("ExpiresAt = %v, want %v", connection.ExpiresAt, expected)
	}
}
