package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"

	"github.com/yourorg/rate-limiter/internal/config"
	"github.com/yourorg/rate-limiter/internal/middleware"
	"github.com/yourorg/rate-limiter/internal/models"
	"github.com/yourorg/rate-limiter/internal/store"
)

type Handler struct {
	cfg    *config.Config
	redis  *store.RedisClient
	pg     *store.PostgresPool
	events *middleware.EventBuffer
	rl     *middleware.RateLimiter
}

func NewRouter(cfg *config.Config, redis *store.RedisClient, pg *store.PostgresPool) http.Handler {
	ctx := context.Background()
	events := middleware.NewEventBuffer(ctx, pg, cfg.AnalyticsFlushInterval)
	rl := middleware.NewRateLimiter(cfg, redis, pg, events)

	h := &Handler{cfg: cfg, redis: redis, pg: pg, events: events, rl: rl}

	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.RealIP)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-API-Key", "X-Admin-Key"},
		ExposedHeaders:   []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-Request-ID"},
		AllowCredentials: false,
		MaxAge:           300,
	}))
	r.Use(middleware.RequestLogger)

	// ---- Health ----
	r.Get("/health", h.health)

	// ---- Check endpoint (rate-limited, requires X-API-Key) ----
	r.Group(func(r chi.Router) {
		r.Use(rl.Middleware)
		r.Post("/check", h.check)
		r.Get("/check", h.check)
		// Demo endpoint group that exercises the limiter
		r.Get("/api/v1/data", h.demoEndpoint)
		r.Post("/api/v1/submit", h.demoEndpoint)
		r.Get("/api/v1/search", h.demoEndpoint)
	})

	// ---- Admin API (requires X-Admin-Key) ----
	r.Group(func(r chi.Router) {
		r.Use(middleware.AdminAuth(cfg.AdminAPIKey))

		// API Key management
		r.Get("/admin/keys", h.listAPIKeys)
		r.Post("/admin/keys", h.createAPIKey)
		r.Put("/admin/keys/{id}/disable", h.disableAPIKey)
		r.Put("/admin/keys/{id}/enable", h.enableAPIKey)

		// Rule management
		r.Get("/admin/rules", h.listRules)
		r.Post("/admin/rules", h.createRule)
		r.Delete("/admin/rules/{id}", h.deleteRule)

		// Analytics
		r.Get("/admin/analytics/summary", h.analyticsSummary)
		r.Get("/admin/analytics/endpoints", h.analyticsEndpoints)
		r.Get("/admin/analytics/timeseries", h.analyticsTimeSeries)
	})

	return r
}

// ---- Health ----

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// ---- Check / Demo endpoints ----

func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	// If we reach here, the request was allowed by the middleware
	respond(w, http.StatusOK, map[string]interface{}{
		"allowed":   true,
		"message":   "request within rate limit",
		"timestamp": time.Now().Unix(),
	})
}

func (h *Handler) demoEndpoint(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]interface{}{
		"path":    r.URL.Path,
		"method":  r.Method,
		"message": "demo response - rate limit passed",
	})
}

// ---- API Key handlers ----

func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.pg.ListAPIKeys(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list API keys")
		return
	}
	respond(w, http.StatusOK, keys)
}

func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		TierID      *string `json:"tier_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}

	key := &models.APIKey{
		Key:         uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		TierID:      req.TierID,
		Enabled:     true,
	}

	if err := h.pg.CreateAPIKey(r.Context(), key); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create API key")
		return
	}

	respond(w, http.StatusCreated, key)
}

func (h *Handler) disableAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.pg.UpdateAPIKeyEnabled(r.Context(), id, false); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to disable key")
		return
	}
	// Invalidate cache
	_ = h.redis.Del(r.Context(), "apikey:"+id)
	respond(w, http.StatusOK, map[string]string{"status": "disabled"})
}

func (h *Handler) enableAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.pg.UpdateAPIKeyEnabled(r.Context(), id, true); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to enable key")
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "enabled"})
}

// ---- Rule handlers ----

func (h *Handler) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.pg.ListRules(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list rules")
		return
	}
	respond(w, http.StatusOK, rules)
}

func (h *Handler) createRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string              `json:"name"`
		APIKeyID   string              `json:"api_key_id"`
		Endpoint   *string             `json:"endpoint"`
		Strategy   models.RuleStrategy `json:"strategy"`
		Limit      int                 `json:"limit"`
		WindowSecs int                 `json:"window_secs"`
		BurstSize  int                 `json:"burst_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.APIKeyID == "" || req.Limit <= 0 || req.WindowSecs <= 0 {
		respondError(w, http.StatusBadRequest, "name, api_key_id, limit, and window_secs are required")
		return
	}

	if req.Strategy == "" {
		req.Strategy = models.StrategyFixedWindow
	}

	rule := &models.Rule{
		Name:       req.Name,
		APIKeyID:   req.APIKeyID,
		Endpoint:   req.Endpoint,
		Strategy:   req.Strategy,
		Limit:      req.Limit,
		WindowSecs: req.WindowSecs,
		BurstSize:  req.BurstSize,
		Enabled:    true,
	}

	if err := h.pg.CreateRule(r.Context(), rule); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create rule")
		return
	}

	respond(w, http.StatusCreated, rule)
}

func (h *Handler) deleteRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.pg.DeleteRule(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete rule")
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- Analytics handlers ----

func (h *Handler) analyticsSummary(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r)
	summaries, err := h.pg.GetAnalyticsSummary(r.Context(), since)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get analytics")
		return
	}
	respond(w, http.StatusOK, summaries)
}

func (h *Handler) analyticsEndpoints(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r)
	endpoints, err := h.pg.GetTopEndpoints(r.Context(), since, 20)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get endpoint analytics")
		return
	}
	respond(w, http.StatusOK, endpoints)
}

func (h *Handler) analyticsTimeSeries(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r)
	apiKeyID := r.URL.Query().Get("api_key_id")
	series, err := h.pg.GetRequestTimeSeries(r.Context(), apiKeyID, since)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get time series")
		return
	}
	respond(w, http.StatusOK, series)
}

// ---- Helpers ----

func parseSince(r *http.Request) time.Time {
	hours := r.URL.Query().Get("hours")
	switch hours {
	case "1":
		return time.Now().Add(-1 * time.Hour)
	case "24":
		return time.Now().Add(-24 * time.Hour)
	case "168":
		return time.Now().Add(-7 * 24 * time.Hour)
	default:
		return time.Now().Add(-1 * time.Hour)
	}
}

func respond(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, code int, msg string) {
	respond(w, code, map[string]string{"error": msg})
}