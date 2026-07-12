package cryptobox

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestEncryptRoundTrip(t *testing.T) {
	os.Setenv("TOKEN_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if active() == nil {
		t.Fatal("key not loaded")
	}
	plain := "ya29.a0-secret-oauth-token"

	ct := Encrypt(plain)
	if ct == plain {
		t.Fatal("ciphertext must differ from plaintext")
	}
	if !strings.HasPrefix(ct, prefix) {
		t.Fatalf("ciphertext missing prefix: %q", ct)
	}
	if got := Decrypt(ct); got != plain {
		t.Fatalf("round trip failed: got %q want %q", got, plain)
	}
	// Encrypt is idempotent — encrypting ciphertext returns it unchanged.
	if again := Encrypt(ct); again != ct {
		t.Fatal("double-encrypt changed the value")
	}
	// Legacy plaintext (no prefix) decrypts to itself.
	if got := Decrypt("legacy-plaintext"); got != "legacy-plaintext" {
		t.Fatalf("plaintext passthrough failed: %q", got)
	}
	// Empty stays empty.
	if Encrypt("") != "" {
		t.Fatal("empty should stay empty")
	}
	// Two encryptions of the same input differ (random nonce).
	a, b := Encrypt(plain), Encrypt(plain)
	if a == b {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
}
