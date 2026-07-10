package api

import (
	"net/http"

	"workflow-ai/server/internal/auth"

	"github.com/gin-gonic/gin"
)

// CorsMiddleware allows credentialed requests from permitted origins: the
// FRONTEND_URL allowlist plus any loopback origin (see auth.OriginAllowed) so
// dev servers on dynamic ports keep working. Cookies require echoing the
// exact origin — a wildcard would make browsers drop them. Server-to-server
// calls (webhooks, API-key triggers) send no Origin header and are unaffected.
func CorsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if origin := c.GetHeader("Origin"); auth.OriginAllowed(origin) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
