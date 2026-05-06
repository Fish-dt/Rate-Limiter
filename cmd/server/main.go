package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/yourorg/rate-limiter/internal/config"
	"github.com/yourorg/rate-limiter/internal/handlers"
	"github.com/yourorg/rate-limiter/internal/store"
)

func main() {
	// Pretty logging in development
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load configuration")
	}

	// Initialize Redis
	redisClient, err := store.NewRedisClient(cfg.RedisURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to Redis")
	}
	defer redisClient.Close()

	// Initialize PostgreSQL
	pgPool, err := store.NewPostgresPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to PostgreSQL")
	}
	defer pgPool.Close()

	// Run migrations
	if err := store.RunMigrations(context.Background(), pgPool); err != nil {
		log.Fatal().Err(err).Msg("failed to run migrations")
	}

	// Build and start server
	srv := &http.Server{
		Addr:         cfg.ServerAddr,
		Handler:      handlers.NewRouter(cfg, redisClient, pgPool),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Info().Str("addr", cfg.ServerAddr).Msg("rate limiter service starting")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	<-done
	log.Info().Msg("shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("server forced shutdown")
	}

	log.Info().Msg("server exited")
}