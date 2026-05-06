#!/usr/bin/env bash
# seed.sh - Creates sample API keys and rules for development
set -e

BASE_URL="${1:-http://localhost:8080}"
ADMIN_KEY="dev-admin-secret"

echo "🌱 Seeding $BASE_URL"

echo ""
echo "--- Creating API Keys ---"

curl -s -X POST "$BASE_URL/admin/keys" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"Mobile Client","description":"iOS/Android app","tier_id":"tier-pro"}' | jq .

curl -s -X POST "$BASE_URL/admin/keys" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"Partner Integration","description":"Third party partner","tier_id":"tier-enterprise"}' | jq .

curl -s -X POST "$BASE_URL/admin/keys" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"Free Trial","description":"New user trial","tier_id":"tier-free"}' | jq .

echo ""
echo "--- Creating Rules for demo key ---"

# Strict limit on search endpoint (sliding window for accuracy)
curl -s -X POST "$BASE_URL/admin/rules" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Search strict limit",
    "api_key_id": "key-demo",
    "endpoint": "/api/v1/search",
    "strategy": "sliding_window",
    "limit": 10,
    "window_secs": 60,
    "burst_size": 2
  }' | jq .

# Token bucket for submit (allows short bursts)
curl -s -X POST "$BASE_URL/admin/rules" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Submit token bucket",
    "api_key_id": "key-demo",
    "endpoint": "/api/v1/submit",
    "strategy": "token_bucket",
    "limit": 30,
    "window_secs": 60,
    "burst_size": 5
  }' | jq .

# Global fixed window for demo key
curl -s -X POST "$BASE_URL/admin/rules" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Global default for demo",
    "api_key_id": "key-demo",
    "strategy": "fixed_window",
    "limit": 100,
    "window_secs": 60
  }' | jq .

echo ""
echo "✅ Seeding complete"
echo ""
echo "Test the rate limiter:"
echo "  curl -H 'X-API-Key: demo-key-12345' $BASE_URL/check"
echo "  curl -H 'X-API-Key: demo-key-12345' $BASE_URL/api/v1/data"