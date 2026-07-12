// Package cryptobox provides transparent authenticated encryption for secrets
// at rest (e.g. stored OAuth tokens). It is keyed by TOKEN_ENC_KEY (a
// base64-encoded 32-byte key → AES-256-GCM). When the key is absent or invalid,
// Encrypt/Decrypt are no-ops, so local dev and pre-existing plaintext rows keep
// working; set the key in production to encrypt tokens in the database. The
// scheme is a lazy migration: legacy plaintext decrypts to itself and is
// re-encrypted on the next write.
package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"sync"
)

const prefix = "enc:v1:" // marks ciphertext so Decrypt passes legacy plaintext through

var (
	keyOnce sync.Once
	key     []byte
)

func loadKey() {
	raw := os.Getenv("TOKEN_ENC_KEY")
	if raw == "" {
		return
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(b) != 32 {
		return // invalid key → stay in passthrough mode
	}
	key = b
}

func active() []byte {
	keyOnce.Do(loadKey)
	return key
}

// Encrypt returns an authenticated ciphertext string. It is a no-op when no key
// is configured, the input is empty, or the input is already encrypted.
func Encrypt(plaintext string) string {
	k := active()
	if k == nil || plaintext == "" || strings.HasPrefix(plaintext, prefix) {
		return plaintext
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return plaintext
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return plaintext
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct)
}

// Decrypt reverses Encrypt. Values without the ciphertext prefix (legacy
// plaintext) are returned unchanged.
func Decrypt(value string) string {
	if !strings.HasPrefix(value, prefix) {
		return value
	}
	k := active()
	if k == nil {
		return value // no key → cannot decrypt; return as stored
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return value
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return value
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return value
	}
	if len(raw) < gcm.NonceSize() {
		return value
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return value
	}
	return string(pt)
}
