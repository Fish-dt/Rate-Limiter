package models

import (
	"time"
)

// RuleStrategy defines the algorithm used to enforce rate limits.
type RuleStrategy string

const (
	StrategyFixedWindow   RuleStrategy = "fixed_window"
	StrategySlidingWindow RuleStrategy = "sliding_window"
	StrategyTokenBucket   RuleStrategy = "token_bucket"
	StrategyLeakyBucket   RuleStrategy = "leaky_bucket"
)

// Rule defines rate limit parameters for a specific API key + endpoint combination.
// A nil Endpoint means the rule applies to all endpoints for that key.
type Rule struct {
	ID         string       `json:"id" db:"id"`
	Name       string       `json:"name" db:"name"`
	APIKeyID   string       `json:"api_key_id" db:"api_key_id"`
	Endpoint   *string      `json:"endpoint,omitempty" db:"endpoint"`
	Strategy   RuleStrategy `json:"strategy" db:"strategy"`
	Limit      int          `json:"limit" db:"limit"`
	WindowSecs int          `json:"window_secs" db:"window_secs"`
	BurstSize  int          `json:"burst_size" db:"burst_size"`
	Enabled    bool         `json:"enabled" db:"enabled"`
	CreatedAt  time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at" db:"updated_at"`
}

// APIKey represents a client credential used to identify rate limit subjects.
type APIKey struct {
	ID          string    `json:"id" db:"id"`
	Key         string    `json:"key" db:"key"`
	Name        string    `json:"name" db:"name"`
	Description string    `json:"description" db:"description"`
	TierID      *string   `json:"tier_id,omitempty" db:"tier_id"`
	Enabled     bool      `json:"enabled" db:"enabled"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty" db:"expires_at"`
}

// Tier groups API keys and applies shared quota defaults.
type Tier struct {
	ID           string    `json:"id" db:"id"`
	Name         string    `json:"name" db:"name"`
	DefaultLimit int       `json:"default_limit" db:"default_limit"`
	DefaultWindow int      `json:"default_window" db:"default_window"` // seconds
	BurstSize    int       `json:"burst_size" db:"burst_size"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
}

// RateLimitDecision is the result of a rate limit check.
type RateLimitDecision struct {
	Allowed    bool   `json:"allowed"`
	Remaining  int    `json:"remaining"`
	Limit      int    `json:"limit"`
	ResetAt    int64  `json:"reset_at"` // Unix timestamp
	RetryAfter int    `json:"retry_after_secs,omitempty"`
	RuleID     string `json:"rule_id,omitempty"`
}

// RequestEvent is stored in PostgreSQL for analytics.
type RequestEvent struct {
	ID        string    `db:"id"`
	APIKeyID  string    `db:"api_key_id"`
	Endpoint  string    `db:"endpoint"`
	Allowed   bool      `db:"allowed"`
	Remaining int       `db:"remaining"`
	RuleID    *string   `db:"rule_id"`
	CreatedAt time.Time `db:"created_at"`
}

// AnalyticsSummary is a pre-aggregated view returned by the analytics API.
type AnalyticsSummary struct {
	APIKeyID      string  `json:"api_key_id"`
	APIKeyName    string  `json:"api_key_name"`
	TotalRequests int64   `json:"total_requests"`
	AllowedCount  int64   `json:"allowed_count"`
	BlockedCount  int64   `json:"blocked_count"`
	AllowRate     float64 `json:"allow_rate"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
}

// TopEndpoint aggregates request counts per endpoint.
type TopEndpoint struct {
	Endpoint      string  `json:"endpoint"`
	TotalRequests int64   `json:"total_requests"`
	BlockedCount  int64   `json:"blocked_count"`
	BlockRate     float64 `json:"block_rate"`
}