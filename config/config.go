package config

import (
	"log/slog"
	"os"

	"github.com/joho/godotenv"
)

func init() {
	if err := godotenv.Load(); err != nil {
		// In Docker, env vars are injected directly — warn but don't panic
		slog.Warn("could not load .env file", "error", err)
	}
}

func GetEnv(key string) string {
	return os.Getenv(key)
}
