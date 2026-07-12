package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Sessions live only in Redis: the client holds a random bearer token, Redis
// maps its hash to the user with a sliding 30-day TTL. The token is delivered
// in the login response (or via the OAuth popup postMessage) and sent back on
// every request as `Authorization: Bearer <token>` — no cookies, so it works
// across sites (Vercel frontend ↔ Railway API) and in browsers that block
// third-party cookies. Losing Redis just means users sign in again.

const sessionTTL = 30 * 24 * time.Hour

type sessionData struct {
	UserID    string `json:"uid"`
	CreatedAt int64  `json:"ca"`
}

func sessionKey(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return "sess:" + hex.EncodeToString(sum[:])
}

// CreateSession stores a new session and returns the raw bearer token.
func CreateSession(ctx context.Context, rdb *redis.Client, userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	payload, _ := json.Marshal(sessionData{UserID: userID, CreatedAt: time.Now().Unix()})
	if err := rdb.Set(ctx, sessionKey(token), payload, sessionTTL).Err(); err != nil {
		return "", err
	}
	return token, nil
}

// GetSession resolves a raw bearer token to a user ID, refreshing the TTL.
func GetSession(ctx context.Context, rdb *redis.Client, rawToken string) (string, bool) {
	key := sessionKey(rawToken)
	raw, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		return "", false
	}
	var data sessionData
	if json.Unmarshal(raw, &data) != nil || data.UserID == "" {
		return "", false
	}
	rdb.Expire(ctx, key, sessionTTL)
	return data.UserID, true
}

// DestroySession removes the session behind a raw bearer token.
func DestroySession(ctx context.Context, rdb *redis.Client, rawToken string) {
	rdb.Del(ctx, sessionKey(rawToken))
}
