package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const ctxUserID = "auth.userID"

// BearerToken extracts the session token from the Authorization header
// ("Bearer <token>"); returns "" when absent or malformed.
func BearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// RequireAuth rejects requests without a valid bearer token and stores the
// resolved user ID on the context for UserID(c).
func RequireAuth(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := BearerToken(c)
		if token == "" {
			telemetry.AuthEvent(c.Request.Context(), "unauthorized", "no_token")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		userID, ok := GetSession(c.Request.Context(), rdb, token)
		if !ok {
			telemetry.AuthEvent(c.Request.Context(), "unauthorized", "bad_session")
			slog.DebugContext(c.Request.Context(), "rejected expired or unknown session", "route", c.FullPath())
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		c.Set(ctxUserID, userID)
		c.Next()
	}
}

// UserID returns the session user set by RequireAuth ("" when unauthenticated).
func UserID(c *gin.Context) string {
	v, _ := c.Get(ctxUserID)
	s, _ := v.(string)
	return s
}
