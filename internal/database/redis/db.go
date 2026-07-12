package redis

import (
	"fmt"
	"log/slog"

	"workflow-ai/server/config"

	"github.com/redis/go-redis/v9"
)

// New builds the Redis client. Managed hosts (Railway, Upstash, …) provide a
// single REDIS_URL that embeds the password, username, and TLS scheme
// (redis:// or rediss://); use it when present. Local dev and Docker Compose
// set REDIS_HOST/REDIS_PORT with no auth, which is the fallback.
func New() *redis.Client {
	if url := config.GetEnv("REDIS_URL"); url != "" {
		opts, err := redis.ParseURL(url)
		if err != nil {
			slog.Error("invalid REDIS_URL, falling back to host/port", "error", err)
		} else {
			opts.Protocol = 3
			slog.Info("connecting to Redis", "addr", opts.Addr, "tls", opts.TLSConfig != nil)
			return redis.NewClient(opts)
		}
	}

	addr := fmt.Sprintf("%s:%s", config.GetEnv("REDIS_HOST"), config.GetEnv("REDIS_PORT"))
	slog.Info("connecting to Redis", "addr", addr)
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: config.GetEnv("REDIS_PASSWORD"), // empty locally
		DB:       0,
		Protocol: 3,
	})
}
