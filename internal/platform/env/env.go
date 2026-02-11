package env

import (
	"os"
	"strconv"
	"time"
)

const (
	DefaultNATSURL      = "nats://localhost:4222"
	DefaultDatabaseURL  = "postgres://app:password@localhost:5432/app?sslmode=disable"
	DefaultCommandAddr  = ":8080"
	DefaultStreamerAddr = ":8081"
)

func String(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func Int(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func Duration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
