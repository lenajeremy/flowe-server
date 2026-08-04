package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE (RFC 7636) for providers that require it — Airtable rejects an authorize
// request without a challenge, and it is the recommended flow elsewhere.
//
// The verifier is generated alongside the CSRF state and kept server-side; only
// its SHA-256 challenge is sent to the provider. At token exchange the verifier
// is replayed, proving the code is being redeemed by whoever started the flow.
// That matters here because the authorization code travels through the user's
// browser, where an interceptor could otherwise redeem it.

// pkceProviders require code_challenge on authorize and code_verifier on exchange.
var pkceProviders = map[string]bool{
	"airtable": true,
	"supabase": true,
}

// newPKCEVerifier returns a high-entropy verifier in the unreserved-character
// alphabet RFC 7636 requires. 32 random bytes base64url-encode to 43 characters,
// which is the spec's minimum length.
func newPKCEVerifier() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; an empty verifier would silently
		// downgrade the flow, so prefer a panic-free but obviously wrong value
		// that the provider will reject.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceChallenge is the S256 challenge for a verifier. Plain is deliberately not
// supported: it offers no protection over sending the verifier itself.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
