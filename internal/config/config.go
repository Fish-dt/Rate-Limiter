package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all service configuration loaded from environment variables.
type Config struct {
	ServerAddr  string
	RedisURL    string
	DatabaseURL string
	AdminAPIKey string

	// Default rate limit applied when no rule matches
	DefaultLimit    int
	DefaultWindow   time.Duration
	DefaultBurst    int

	// Analytics flush interval
	AnalyticsFlushInterval time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{
		ServerAddr:             getEnv("SERVER_ADDR", ":8080"),
		RedisURL:               getEnv("REDIS_URL", "redis://localhost:6379/0"),
		DatabaseURL:            getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/ratelimiter?sslmode=disable"),
		AdminAPIKey:            getEnv("ADMIN_API_KEY", "dev-admin-secret"),
		DefaultLimit:           getEnvInt("DEFAULT_LIMIT", 100),
		DefaultWindow:          getEnvDuration("DEFAULT_WINDOW", time.Minute),
		DefaultBurst:           getEnvInt("DEFAULT_BURST", 20),
		AnalyticsFlushInterval: getEnvDuration("ANALYTICS_FLUSH_INTERVAL", 30*time.Second),
	}

	if cfg.RedisURL == "" {
		return nil, fmt.Errorf("REDIS_URL is required")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}