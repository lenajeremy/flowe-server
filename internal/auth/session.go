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
	UserID string `json:"uid"`
	// OrgID is resolved once at login and cached here, so no request pays a
	// database round-trip to learn which tenant it acts within. Safe to cache
	// because a personal org id is derived from the user id and never changes.
	// The PLAN deliberately is not cached — it changes on a Stripe webhook, and a
	// stale plan is an entitlement bug.
	OrgID     string `json:"oid,omitempty"`
	CreatedAt int64  `json:"ca"`
}

func sessionKey(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return "sess:" + hex.EncodeToString(sum[:])
}

// CreateSession stores a new session and returns the raw bearer token.
func CreateSession(ctx context.Context, rdb *redis.Client, userID, orgID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	payload, _ := json.Marshal(sessionData{UserID: userID, OrgID: orgID, CreatedAt: time.Now().Unix()})
	if err := rdb.Set(ctx, sessionKey(token), payload, sessionTTL).Err(); err != nil {
		return "", err
	}
	return token, nil
}

// GetSession resolves a raw bearer token to a user ID, refreshing the TTL.
func GetSession(ctx context.Context, rdb *redis.Client, rawToken string) (string, bool) {
	uid, _, ok := GetSessionFull(ctx, rdb, rawToken)
	return uid, ok
}

// GetSessionFull additionally returns the org the session acts within.
//
// Sessions minted before tenancy existed carry no org id. Rather than logging
// those users out, the caller derives the personal org from the user id — see
// tenancy.PersonalOrgID, which computes the same value this would have stored.
func GetSessionFull(ctx context.Context, rdb *redis.Client, rawToken string) (userID, orgID string, ok bool) {
	key := sessionKey(rawToken)
	raw, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		return "", "", false
	}
	var data sessionData
	if json.Unmarshal(raw, &data) != nil || data.UserID == "" {
		return "", "", false
	}
	rdb.Expire(ctx, key, sessionTTL)
	return data.UserID, data.OrgID, true
}

// DestroySession removes the session behind a raw bearer token.
func DestroySession(ctx context.Context, rdb *redis.Client, rawToken string) {
	rdb.Del(ctx, sessionKey(rawToken))
}
