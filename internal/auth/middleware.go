package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"workflow-ai/server/internal/telemetry"
	"workflow-ai/server/internal/tenancy"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	ctxUserID = "auth.userID"
	ctxOrgID  = "auth.orgID"
)

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
		userID, orgID, ok := GetSessionFull(c.Request.Context(), rdb, token)
		if !ok {
			telemetry.AuthEvent(c.Request.Context(), "unauthorized", "bad_session")
			slog.DebugContext(c.Request.Context(), "rejected expired or unknown session", "route", c.FullPath())
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		if orgID == "" {
			// Session predates tenancy. Derive the personal org rather than forcing
			// a re-login; this is the same value CreateSession would have cached.
			orgID = tenancy.PersonalOrgID(userID).String()
		}
		c.Set(ctxUserID, userID)
		c.Set(ctxOrgID, orgID)
		c.Next()
	}
}

// UserID returns the session user set by RequireAuth ("" when unauthenticated).
func UserID(c *gin.Context) string {
	v, _ := c.Get(ctxUserID)
	s, _ := v.(string)
	return s
}

// OrgID returns the organization the request acts within ("" when
// unauthenticated). This is the scope predicate for every tenant-owned query —
// missing it is a cross-tenant data leak rather than a bug.
func OrgID(c *gin.Context) string {
	v, _ := c.Get(ctxOrgID)
	s, _ := v.(string)
	return s
}
