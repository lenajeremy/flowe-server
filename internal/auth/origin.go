package auth

import (
	"net/url"
	"os"
	"strings"
)

// OriginAllowed reports whether a browser Origin may make credentialed
// requests. Two ways in: an exact match against the comma-separated
// FRONTEND_URL env var (production), or any http(s) loopback origin —
// localhost, 127.0.0.1, ::1, *.localhost — on any port, so local dev keeps
// working when the dev server's port changes between runs.
func OriginAllowed(origin string) bool {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return false
	}

	for _, o := range strings.Split(os.Getenv("FRONTEND_URL"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" && o == origin {
			return true
		}
	}

	// Loopback convenience is for local dev only. In production
	// (APP_ENV=production) the allowlist is FRONTEND_URL exact matches only.
	if os.Getenv("APP_ENV") == "production" {
		return false
	}

	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasSuffix(host, ".localhost")
}
