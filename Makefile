.PHONY: run build test docker-up docker-down seed load-test fmt lint

# Run locally (requires Redis + Postgres running)
run:
	go run ./cmd/server

# Build binary
build:
	CGO_ENABLED=0 go build -ldflags="-w -s" -o bin/rate-limiter ./cmd/server

# Run all tests
test:
	go test -race -count=1 ./...

# Start the full stack with Docker Compose
docker-up:
	docker compose up --build -d
	@echo "Waiting for services to be healthy..."
	@sleep 5
	@echo "Service running at http://localhost:8080"

# Stop all containers
docker-down:
	docker compose down

# Seed development data
seed:
	bash scripts/seed.sh http://localhost:8080

# Run load test
load-test:
	bash scripts/load_test.sh http://localhost:8080 demo-key-12345

# Format code
fmt:
	gofmt -w .
	goimports -w .

# Lint
lint:
	golangci-lint run ./...

# Quick smoke test
smoke:
	@echo "--- Health check ---"
	curl -s http://localhost:8080/health | jq .
	@echo ""
	@echo "--- Allowed request ---"
	curl -s -H "X-API-Key: demo-key-12345" http://localhost:8080/check | jq .
	@echo ""
	@echo "--- Rate limit headers ---"
	curl -si -H "X-API-Key: demo-key-12345" http://localhost:8080/check | grep -i "x-rate"
	@echo ""
	@echo "--- No API key (401) ---"
	curl -s http://localhost:8080/check | jq .

# Watch logs
logs:
	docker compose logs -f app