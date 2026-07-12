package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// Sessions live only in Redis: the cookie carries a random token, Redis maps
// its hash to the user with a sliding 30-day TTL. Losing Redis just means
// users sign in again.

const (
	SessionCookie = "wf_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type sessionData struct {
	UserID    string `json:"uid"`
	CreatedAt int64  `json:"ca"`
}

func sessionKey(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return "sess:" + hex.EncodeToString(sum[:])
}

// CreateSession stores a new session and returns the raw token for the cookie.
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

// GetSession resolves a raw cookie token to a user ID, refreshing the TTL.
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

// DestroySession removes the session behind a raw cookie token.
func DestroySession(ctx context.Context, rdb *redis.Client, rawToken string) {
	rdb.Del(ctx, sessionKey(rawToken))
}

func cookieSecure() bool {
	return os.Getenv("COOKIE_SECURE") == "true"
}

// sameSiteMode picks the cookie's SameSite policy. In production the frontend
// and API live on different registrable domains (e.g. *.vercel.app frontend,
// *.railway.app API), so the browser treats auth requests as cross-site and
// drops a Lax cookie — SameSite=None is required, which browsers only honour
// with Secure. In local dev (COOKIE_SECURE unset) both origins are localhost,
// where Lax works and None would be rejected for lacking Secure.
func sameSiteMode() http.SameSite {
	if cookieSecure() {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

// SetSessionCookie attaches the session cookie to the response.
// http.SetCookie is used directly so SameSite is explicit.
func SetSessionCookie(c *gin.Context, token string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   cookieSecure(),
		SameSite: sameSiteMode(),
	})
}

// ClearSessionCookie expires the session cookie immediately.
func ClearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   cookieSecure(),
		SameSite: sameSiteMode(),
	})
}
