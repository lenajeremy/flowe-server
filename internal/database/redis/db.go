package redis

import (
	"fmt"
	"log/slog"

	"workflow-ai/server/config"

	"github.com/redis/go-redis/v9"
)

func New() *redis.Client {
	addr := fmt.Sprintf("%s:%s", config.GetEnv("REDIS_HOST"), config.GetEnv("REDIS_PORT"))
	slog.Info("connecting to Redis", "addr", addr)
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		DB:       0,
		Protocol: 3,
	})
}
