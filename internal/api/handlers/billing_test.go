package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A redirect built from a stale FRONTEND_URL entry lands the customer on "cannot
// connect" AFTER they have paid, which is the worst possible moment for it. These
// pin the fallback order.

func withOrigin(origin string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	if origin != "" {
		c.Request.Header.Set("Origin", origin)
	}
	return c
}

func TestAllowedOriginBeatsAStaleConfiguredURL(t *testing.T) {
	// FRONTEND_URL's first entry is a port nothing is serving; the browser's own
	// origin demonstrably works, because the request just arrived from it.
	t.Setenv("FRONTEND_URL", "http://127.0.0.1:4180,http://localhost:5173")
	if got := clientBaseURL(withOrigin("http://localhost:5173")); got != "http://localhost:5173" {
		t.Fatalf("base = %q, want the request's origin", got)
	}
}

func TestDisallowedOriginIsIgnored(t *testing.T) {
	// An attacker-supplied Origin must never become a redirect target handed to
	// Stripe, which would turn our checkout into an open redirect.
	t.Setenv("FRONTEND_URL", "http://localhost:5173")
	if got := clientBaseURL(withOrigin("https://evil.example.com")); got != "http://localhost:5173" {
		t.Fatalf("base = %q — an unvetted origin was accepted", got)
	}
}

func TestNoOriginFallsBackToTheConfiguredURL(t *testing.T) {
	// Server-to-server callers send no Origin at all.
	t.Setenv("FRONTEND_URL", "http://localhost:4180,http://localhost:5173")
	if got := clientBaseURL(withOrigin("")); got != "http://localhost:4180" {
		t.Fatalf("base = %q, want the first configured entry", got)
	}
}

func TestTrailingSlashIsStripped(t *testing.T) {
	// Otherwise the success URL becomes "…//settings/billing".
	t.Setenv("FRONTEND_URL", "http://localhost:5173")
	if got := clientBaseURL(withOrigin("http://localhost:5173/")); got != "http://localhost:5173" {
		t.Fatalf("base = %q, want no trailing slash", got)
	}
}
