#!/usr/bin/env bash
# load_test.sh - Generates realistic traffic to populate analytics data
# Usage: ./scripts/load_test.sh [base_url] [api_key]

set -e

BASE_URL="${1:-http://localhost:8080}"
API_KEY="${2:-demo-key-12345}"
TOTAL=500
DELAY=0.02

ENDPOINTS=(
  "/api/v1/data"
  "/api/v1/search"
  "/api/v1/submit"
  "/check"
)

echo "🚀 Load testing $BASE_URL with key $API_KEY"
echo "   Sending $TOTAL requests across ${#ENDPOINTS[@]} endpoints..."
echo ""

ok=0
blocked=0

for i in $(seq 1 $TOTAL); do
  ep=${ENDPOINTS[$((RANDOM % ${#ENDPOINTS[@]}))]}
  method="GET"
  if [[ "$ep" == *"submit"* ]]; then
    method="POST"
  fi

  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X "$method" \
    -H "X-API-Key: $API_KEY" \
    "$BASE_URL$ep" 2>/dev/null)

  if [[ "$HTTP_CODE" == "200" ]]; then
    ((ok++)) || true
  elif [[ "$HTTP_CODE" == "429" ]]; then
    ((blocked++)) || true
  fi

  if (( i % 50 == 0 )); then
    echo "  Progress: $i/$TOTAL | allowed=$ok blocked=$blocked"
  fi

  sleep $DELAY
done

echo ""
echo "✅ Done. Results:"
echo "   Allowed:  $ok"
echo "   Blocked:  $blocked"
echo "   Total:    $TOTAL"