package auth

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"workflow-ai/server/internal/telemetry"

	"github.com/redis/go-redis/v9"
)

// Allow increments a Redis counter and reports whether the caller is still
// under limit for the window. The window starts at the first hit.
func Allow(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) bool {
	count, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		// Redis being down shouldn't lock everyone out of sign-in.
		return true
	}
	if count == 1 {
		rdb.Expire(ctx, key, window)
	}
	if count > int64(limit) {
		// Scope, not the full key — keys embed user IDs / emails.
		scope := key
		if parts := strings.Split(key, ":"); len(parts) >= 2 {
			scope = parts[1]
		}
		telemetry.RateLimitHit(ctx, scope)
		slog.WarnContext(ctx, "rate limit exceeded", "scope", scope, "count", count, "limit", limit)
		return false
	}
	return true
}
