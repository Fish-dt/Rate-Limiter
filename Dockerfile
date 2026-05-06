# ---- Build stage ----
    FROM golang:1.22-alpine AS builder

    WORKDIR /app
    
    RUN apk add --no-cache git ca-certificates
    
    COPY go.mod go.sum ./
    RUN go mod download
    
    COPY . .
    RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-w -s" -o /rate-limiter ./cmd/server
    
    # ---- Runtime stage ----
    FROM scratch
    
    COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
    COPY --from=builder /rate-limiter /rate-limiter
    
    EXPOSE 8080
    
    ENTRYPOINT ["/rate-limiter"]