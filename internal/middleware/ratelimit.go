package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/yourorg/rate-limiter/internal/config"
	"github.com/yourorg/rate-limiter/internal/models"
	"github.com/yourorg/rate-limiter/internal/store"
)

// contextKey avoids collisions in context values.
type contextKey string

const (
	ctxAPIKey    contextKey = "api_key"
	ctxRequestID contextKey = "request_id"
)

// EventBuffer collects RequestEvents and flushes them to PostgreSQL periodically.
type EventBuffer struct {
	ch chan *models.RequestEvent
}

func NewEventBuffer(ctx context.Context, pg *store.PostgresPool, flushInterval time.Duration) *EventBuffer {
	buf := &EventBuffer{ch: make(chan *models.RequestEvent, 10000)}
	go buf.flusher(ctx, pg, flushInterval)
	return buf
}

func (b *EventBuffer) Push(e *models.RequestEvent) {
	select {
	case b.ch <- e:
	default:
		log.Warn().Msg("event buffer full, dropping event")
	}
}

func (b *EventBuffer) flusher(ctx context.Context, pg *store.PostgresPool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var batch []*models.RequestEvent

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := pg.InsertEvents(context.Background(), batch); err != nil {
			log.Error().Err(err).Int("count", len(batch)).Msg("failed to flush events")
		} else {
			log.Debug().Int("count", len(batch)).Msg("flushed events to PostgreSQL")
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-b.ch:
			batch = append(batch, e)
			if len(batch) >= 500 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// RateLimiter is the core middleware that enforces rate limits.
type RateLimiter struct {
	cfg    *config.Config
	redis  *store.RedisClient
	pg     *store.PostgresPool
	events *EventBuffer
}

func NewRateLimiter(cfg *config.Config, redis *store.RedisClient, pg *store.PostgresPool, events *EventBuffer) *RateLimiter {
	return &RateLimiter{cfg: cfg, redis: redis, pg: pg, events: events}
}

// Middleware returns an http.Handler that enforces rate limits.
// The X-API-Key header is used to identify the client.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.New().String()
		ctx := context.WithValue(r.Context(), ctxRequestID, requestID)

		apiKeyValue := r.Header.Get("X-API-Key")
		if apiKeyValue == "" {
			apiKeyValue = r.URL.Query().Get("api_key")
		}

		if apiKeyValue == "" {
			writeError(w, http.StatusUnauthorized, "missing X-API-Key header")
			return
		}

		// Resolve API key from database (cache in Redis for 60s)
		apiKey, err := rl.resolveAPIKey(ctx, apiKeyValue)
		if err != nil || apiKey == nil {
			writeError(w, http.StatusUnauthorized, "invalid or disabled API key")
			return
		}

		// Check expiry
		if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
			writeError(w, http.StatusForbidden, "API key has expired")
			return
		}

		ctx = context.WithValue(ctx, ctxAPIKey, apiKey)

		// Find the most specific matching rule
		decision, rule, err := rl.evaluate(ctx, apiKey, r.URL.Path)
		if err != nil {
			log.Error().Err(err).Str("key", apiKey.ID).Msg("rate limit evaluation failed")
			// Fail open - allow request but log the error
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Set response headers
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", decision.Limit))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", decision.Remaining))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", decision.ResetAt))
		w.Header().Set("X-Request-ID", requestID)

		// Record event asynchronously
		event := &models.RequestEvent{
			ID:        requestID,
			APIKeyID:  apiKey.ID,
			Endpoint:  r.URL.Path,
			Allowed:   decision.Allowed,
			Remaining: decision.Remaining,
			CreatedAt: time.Now(),
		}
		if rule != nil {
			event.RuleID = &rule.ID
		}
		rl.events.Push(event)

		if !decision.Allowed {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", decision.RetryAfter))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// evaluate finds the matching rule and runs the appropriate algorithm.
func (rl *RateLimiter) evaluate(ctx context.Context, apiKey *models.APIKey, path string) (*models.RateLimitDecision, *models.Rule, error) {
	rules, err := rl.pg.GetRulesForKey(ctx, apiKey.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("get rules: %w", err)
	}

	// Rule priority: exact endpoint match > wildcard (nil endpoint) > global default
	var matchedRule *models.Rule
	for _, r := range rules {
		if r.Endpoint != nil && *r.Endpoint == path {
			matchedRule = r
			break
		}
		if r.Endpoint == nil && matchedRule == nil {
			matchedRule = r
		}
	}

	var limit, windowSecs int
	var strategy models.RuleStrategy

	if matchedRule != nil {
		limit = matchedRule.Limit
		windowSecs = matchedRule.WindowSecs
		strategy = matchedRule.Strategy
	} else {
		limit = rl.cfg.DefaultLimit
		windowSecs = int(rl.cfg.DefaultWindow.Seconds())
		strategy = models.StrategyFixedWindow
	}

	key := fmt.Sprintf("%s:%s", apiKey.ID, path)
	requestID := uuid.New().String()

	decision := &models.RateLimitDecision{Limit: limit}
	if matchedRule != nil {
		decision.RuleID = matchedRule.ID
	}

	switch strategy {
	case models.StrategySlidingWindow:
		allowed, remaining := rl.redis.CheckSlidingWindow(ctx, key, limit, windowSecs, requestID)
		decision.Allowed = allowed
		decision.Remaining = remaining
		decision.ResetAt = time.Now().Add(time.Duration(windowSecs) * time.Second).Unix()
		decision.RetryAfter = windowSecs

	case models.StrategyTokenBucket:
		capacity := limit
		if matchedRule != nil && matchedRule.BurstSize > 0 {
			capacity = matchedRule.BurstSize
		}
		allowed, remaining := rl.redis.CheckTokenBucket(ctx, key, capacity, windowSecs)
		decision.Allowed = allowed
		decision.Remaining = remaining
		decision.ResetAt = time.Now().Add(time.Duration(windowSecs) * time.Second).Unix()
		if !allowed {
			decision.RetryAfter = windowSecs / limit
			if decision.RetryAfter < 1 {
				decision.RetryAfter = 1
			}
		}

	default: // fixed_window
		remaining, resetAt, allowed := rl.redis.CheckFixedWindow(ctx, key, limit, windowSecs)
		decision.Allowed = allowed
		decision.Remaining = remaining
		decision.ResetAt = resetAt
		if !allowed {
			decision.RetryAfter = int(time.Until(time.Unix(resetAt, 0)).Seconds())
		}
	}

	return decision, matchedRule, nil
}

// resolveAPIKey looks up an API key, with Redis caching.
func (rl *RateLimiter) resolveAPIKey(ctx context.Context, keyValue string) (*models.APIKey, error) {
	cacheKey := fmt.Sprintf("apikey:%s", keyValue)

	// Try cache first
	if cached, err := rl.redis.Get(ctx, cacheKey); err == nil && cached != "" {
		var apiKey models.APIKey
		if err := json.Unmarshal([]byte(cached), &apiKey); err == nil {
			return &apiKey, nil
		}
	}

	// Fall back to database
	apiKey, err := rl.pg.GetAPIKey(ctx, keyValue)
	if err != nil {
		return nil, err
	}

	if !apiKey.Enabled {
		return nil, nil
	}

	// Cache for 60 seconds
	if data, err := json.Marshal(apiKey); err == nil {
		_ = rl.redis.Set(ctx, cacheKey, string(data), 60*time.Second)
	}

	return apiKey, nil
}

// AdminAuth middleware enforces admin API key on management endpoints.
func AdminAuth(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Admin-Key")
			if key != adminKey {
				writeError(w, http.StatusForbidden, "invalid admin key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestLogger logs every request with latency.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(wrapped, r)
		log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", wrapped.status).
			Dur("latency", time.Since(start)).
			Msg("request")
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}