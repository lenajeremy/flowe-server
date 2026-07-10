package auth

import (
	"context"
	"time"

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
	return count <= int64(limit)
}
