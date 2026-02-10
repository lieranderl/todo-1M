package env

import "os"

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
