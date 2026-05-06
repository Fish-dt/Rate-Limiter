package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// RedisClient wraps the go-redis client with helper methods.
type RedisClient struct {
	client *redis.Client
}

func NewRedisClient(redisURL string) (*RedisClient, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	log.Info().Str("addr", opts.Addr).Msg("connected to Redis")
	return &RedisClient{client: rdb}, nil
}

func (r *RedisClient) Close() error {
	return r.client.Close()
}

// ---- Fixed Window Counter ----
// Uses a simple INCR with TTL. Fast, O(1) per request.
// Key: rl:fw:{api_key}:{endpoint}:{window_bucket}

var fixedWindowScript = redis.NewScript(`
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])

local current = redis.call("INCR", key)
if current == 1 then
	redis.call("EXPIRE", key, window)
end

local ttl = redis.call("TTL", key)
return {current, limit, ttl}
`)

// CheckFixedWindow returns (remaining, resetAfterSecs, allowed).
func (r *RedisClient) CheckFixedWindow(ctx context.Context, key string, limit, windowSecs int) (remaining int, resetAt int64, allowed bool) {
	now := time.Now()
	bucket := now.Unix() / int64(windowSecs)
	redisKey := fmt.Sprintf("rl:fw:%s:%d", key, bucket)

	res, err := fixedWindowScript.Run(ctx, r.client, []string{redisKey}, limit, windowSecs).Int64Slice()
	if err != nil {
		log.Error().Err(err).Msg("fixed window script error")
		return limit, now.Add(time.Duration(windowSecs) * time.Second).Unix(), true // fail open
	}

	current := int(res[0])
	ttl := int(res[2])
	allowed = current <= limit
	remaining = limit - current
	if remaining < 0 {
		remaining = 0
	}
	resetAt = now.Add(time.Duration(ttl) * time.Second).Unix()
	return
}

// ---- Sliding Window Log ----
// Uses a sorted set of request timestamps. More precise, O(log N).
// Key: rl:sw:{api_key}:{endpoint}

var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local request_id = ARGV[4]

local window_start = now - window_ms

redis.call("ZREMRANGEBYSCORE", key, "-inf", window_start)
local count = redis.call("ZCARD", key)

if count < limit then
	redis.call("ZADD", key, now, request_id)
	redis.call("PEXPIRE", key, window_ms)
	return {1, limit - count - 1}
else
	return {0, 0}
end
`)

// CheckSlidingWindow returns (allowed, remaining).
func (r *RedisClient) CheckSlidingWindow(ctx context.Context, key string, limit, windowSecs int, requestID string) (allowed bool, remaining int) {
	nowMs := time.Now().UnixMilli()
	windowMs := int64(windowSecs) * 1000
	redisKey := fmt.Sprintf("rl:sw:%s", key)

	res, err := slidingWindowScript.Run(ctx, r.client, []string{redisKey}, nowMs, windowMs, limit, requestID).Int64Slice()
	if err != nil {
		log.Error().Err(err).Msg("sliding window script error")
		return true, limit // fail open
	}

	allowed = res[0] == 1
	remaining = int(res[1])
	return
}

// ---- Token Bucket ----
// Refills at rate = limit/window. Allows controlled bursting.
// Key: rl:tb:{api_key}:{endpoint}

var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])  -- tokens per second (float)
local now = tonumber(ARGV[3])          -- unix timestamp milliseconds

local data = redis.call("HMGET", key, "tokens", "last_refill")
local tokens = tonumber(data[1]) or capacity
local last_refill = tonumber(data[2]) or now

local elapsed = (now - last_refill) / 1000.0  -- seconds
local new_tokens = math.min(capacity, tokens + elapsed * refill_rate)

local allowed = 0
if new_tokens >= 1 then
	new_tokens = new_tokens - 1
	allowed = 1
end

redis.call("HMSET", key, "tokens", new_tokens, "last_refill", now)
redis.call("PEXPIRE", key, math.ceil(capacity / refill_rate) * 2000)

return {allowed, math.floor(new_tokens)}
`)

// CheckTokenBucket returns (allowed, remaining).
func (r *RedisClient) CheckTokenBucket(ctx context.Context, key string, capacity, windowSecs int) (allowed bool, remaining int) {
	refillRate := float64(capacity) / float64(windowSecs)
	nowMs := time.Now().UnixMilli()
	redisKey := fmt.Sprintf("rl:tb:%s", key)

	res, err := tokenBucketScript.Run(ctx, r.client, []string{redisKey}, capacity, refillRate, nowMs).Int64Slice()
	if err != nil {
		log.Error().Err(err).Msg("token bucket script error")
		return true, capacity // fail open
	}

	allowed = res[0] == 1
	remaining = int(res[1])
	return
}

// ---- Generic helpers ----

// Get retrieves a string value.
func (r *RedisClient) Get(ctx context.Context, key string) (string, error) {
	return r.client.Get(ctx, key).Result()
}

// Set stores a string value with TTL.
func (r *RedisClient) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

// Del removes keys.
func (r *RedisClient) Del(ctx context.Context, keys ...string) error {
	return r.client.Del(ctx, keys...).Err()
}

// IncrBy increments a counter and returns new value.
func (r *RedisClient) IncrBy(ctx context.Context, key string, val int64) (int64, error) {
	return r.client.IncrBy(ctx, key, val).Result()
}

// Expire sets a TTL on an existing key.
func (r *RedisClient) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return r.client.Expire(ctx, key, ttl).Err()
}

// HGetAll returns all fields of a hash.
func (r *RedisClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return r.client.HGetAll(ctx, key).Result()
}

// Pipeline returns a Redis pipeline for batch operations.
func (r *RedisClient) Pipeline() redis.Pipeliner {
	return r.client.Pipeline()
}

// Keys returns keys matching a pattern (use sparingly - not for hot paths).
func (r *RedisClient) Keys(ctx context.Context, pattern string) ([]string, error) {
	return r.client.Keys(ctx, pattern).Result()
}