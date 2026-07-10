package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const ctxUserID = "auth.userID"

// RequireAuth rejects requests without a valid session cookie and stores the
// resolved user ID on the context for UserID(c).
func RequireAuth(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie(SessionCookie)
		if err != nil || cookie == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		userID, ok := GetSession(c.Request.Context(), rdb, cookie)
		if !ok {
			ClearSessionCookie(c)
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
