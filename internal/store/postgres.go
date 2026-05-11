package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/yourorg/rate-limiter/internal/models"
)

// PostgresPool wraps pgxpool with domain methods.
type PostgresPool struct {
	pool *pgxpool.Pool
}

func NewPostgresPool(ctx context.Context, connStr string) (*PostgresPool, error) {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}

	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = 15 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}

	log.Info().Str("conn", cfg.ConnConfig.Host).Msg("connected to PostgreSQL")
	return &PostgresPool{pool: pool}, nil
}

func (p *PostgresPool) Close() {
	p.pool.Close()
}

// ---- Migrations ----

func RunMigrations(ctx context.Context, p *PostgresPool) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS tiers (
			id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			name         TEXT NOT NULL UNIQUE,
			default_limit  INT NOT NULL DEFAULT 1000,
			default_window INT NOT NULL DEFAULT 60,
			burst_size   INT NOT NULL DEFAULT 50,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			key         TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			tier_id     TEXT REFERENCES tiers(id),
			enabled     BOOL NOT NULL DEFAULT TRUE,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			expires_at  TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS api_keys_key_idx ON api_keys(key)`,
		`CREATE TABLE IF NOT EXISTS rules (
			id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			name         TEXT NOT NULL,
			api_key_id   TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
			endpoint     TEXT,
			strategy     TEXT NOT NULL DEFAULT 'fixed_window',
			limit_count  INT NOT NULL DEFAULT 100,
			window_secs  INT NOT NULL DEFAULT 60,
			burst_size   INT NOT NULL DEFAULT 10,
			enabled      BOOL NOT NULL DEFAULT TRUE,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS rules_api_key_idx ON rules(api_key_id, enabled)`,
		`CREATE TABLE IF NOT EXISTS request_events (
			id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			api_key_id  TEXT NOT NULL,
			endpoint    TEXT NOT NULL,
			allowed     BOOL NOT NULL,
			remaining   INT NOT NULL,
			rule_id     TEXT,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS events_api_key_time_idx ON request_events(api_key_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS events_created_at_idx ON request_events(created_at DESC)`,
		// Seed a default tier and demo API key for development
		`INSERT INTO tiers (id, name, default_limit, default_window, burst_size)
		 VALUES ('tier-free', 'free', 60, 60, 10),
		        ('tier-pro', 'pro', 1000, 60, 100),
		        ('tier-enterprise', 'enterprise', 10000, 60, 500)
		 ON CONFLICT (name) DO NOTHING`,
		`INSERT INTO api_keys (id, key, name, description, tier_id)
		 VALUES ('key-demo', 'demo-key-12345', 'Demo Client', 'Pre-seeded demo key', 'tier-pro')
		 ON CONFLICT (key) DO NOTHING`,
	}

	for i, m := range migrations {
		if _, err := p.pool.Exec(ctx, m); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}

	log.Info().Int("count", len(migrations)).Msg("migrations applied")
	return nil
}

// ---- API Key queries ----

func (p *PostgresPool) GetAPIKey(ctx context.Context, key string) (*models.APIKey, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT id, key, name, description, tier_id, enabled, created_at, expires_at
		 FROM api_keys WHERE key = $1`, key)

	var k models.APIKey
	err := row.Scan(&k.ID, &k.Key, &k.Name, &k.Description, &k.TierID, &k.Enabled, &k.CreatedAt, &k.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (p *PostgresPool) ListAPIKeys(ctx context.Context) ([]*models.APIKey, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, key, name, description, tier_id, enabled, created_at, expires_at
		 FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*models.APIKey
	for rows.Next() {
		var k models.APIKey
		if err := rows.Scan(&k.ID, &k.Key, &k.Name, &k.Description, &k.TierID, &k.Enabled, &k.CreatedAt, &k.ExpiresAt); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}
	return keys, nil
}

func (p *PostgresPool) CreateAPIKey(ctx context.Context, k *models.APIKey) error {
	return p.pool.QueryRow(ctx,
		`INSERT INTO api_keys (key, name, description, tier_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at`,
		k.Key, k.Name, k.Description, k.TierID, k.ExpiresAt,
	).Scan(&k.ID, &k.CreatedAt)
}

func (p *PostgresPool) UpdateAPIKeyEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := p.pool.Exec(ctx, `UPDATE api_keys SET enabled = $1 WHERE id = $2`, enabled, id)
	return err
}

// ---- Rule queries ----

func (p *PostgresPool) GetRulesForKey(ctx context.Context, apiKeyID string) ([]*models.Rule, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, api_key_id, endpoint, strategy, limit_count, window_secs, burst_size, enabled, created_at, updated_at
		 FROM rules WHERE api_key_id = $1 AND enabled = TRUE
		 ORDER BY endpoint NULLS LAST, created_at`, apiKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []*models.Rule
	for rows.Next() {
		var r models.Rule
		if err := rows.Scan(&r.ID, &r.Name, &r.APIKeyID, &r.Endpoint, &r.Strategy,
			&r.Limit, &r.WindowSecs, &r.BurstSize, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, &r)
	}
	return rules, nil
}

func (p *PostgresPool) ListRules(ctx context.Context) ([]*models.Rule, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, api_key_id, endpoint, strategy, limit_count, window_secs, burst_size, enabled, created_at, updated_at
		 FROM rules ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []*models.Rule
	for rows.Next() {
		var r models.Rule
		if err := rows.Scan(&r.ID, &r.Name, &r.APIKeyID, &r.Endpoint, &r.Strategy,
			&r.Limit, &r.WindowSecs, &r.BurstSize, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, &r)
	}
	return rules, nil
}

func (p *PostgresPool) CreateRule(ctx context.Context, r *models.Rule) error {
	return p.pool.QueryRow(ctx,
		`INSERT INTO rules (name, api_key_id, endpoint, strategy, limit_count, window_secs, burst_size)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at, updated_at`,
		r.Name, r.APIKeyID, r.Endpoint, r.Strategy, r.Limit, r.WindowSecs, r.BurstSize,
	).Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
}

func (p *PostgresPool) DeleteRule(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM rules WHERE id = $1`, id)
	return err
}

// ---- Analytics queries ----

func (p *PostgresPool) InsertEvents(ctx context.Context, events []*models.RequestEvent) error {
	if len(events) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	// Use a batch insert
	for _, e := range events {
		batch.Queue(
			`INSERT INTO request_events (id, api_key_id, endpoint, allowed, remaining, rule_id, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.ID, e.APIKeyID, e.Endpoint, e.Allowed, e.Remaining, e.RuleID, e.CreatedAt,
		)
	}

	results := p.pool.SendBatch(ctx, batch)
	defer results.Close()

	for range events {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("batch insert event: %w", err)
		}
	}
	return nil
}

func (p *PostgresPool) GetAnalyticsSummary(ctx context.Context, since time.Time) ([]*models.AnalyticsSummary, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT
			re.api_key_id,
			ak.name,
			COUNT(*) AS total_requests,
			SUM(CASE WHEN re.allowed THEN 1 ELSE 0 END) AS allowed_count,
			SUM(CASE WHEN NOT re.allowed THEN 1 ELSE 0 END) AS blocked_count,
			$1::timestamptz AS period_start,
			NOW() AS period_end
		FROM request_events re
		JOIN api_keys ak ON ak.id = re.api_key_id
		WHERE re.created_at >= $1
		GROUP BY re.api_key_id, ak.name
		ORDER BY total_requests DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*models.AnalyticsSummary
	for rows.Next() {
		var s models.AnalyticsSummary
		if err := rows.Scan(&s.APIKeyID, &s.APIKeyName, &s.TotalRequests, &s.AllowedCount, &s.BlockedCount, &s.PeriodStart, &s.PeriodEnd); err != nil {
			return nil, err
		}
		if s.TotalRequests > 0 {
			s.AllowRate = float64(s.AllowedCount) / float64(s.TotalRequests)
		}
		summaries = append(summaries, &s)
	}
	return summaries, nil
}

func (p *PostgresPool) GetTopEndpoints(ctx context.Context, since time.Time, limit int) ([]*models.TopEndpoint, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT
			endpoint,
			COUNT(*) AS total_requests,
			SUM(CASE WHEN NOT allowed THEN 1 ELSE 0 END) AS blocked_count
		FROM request_events
		WHERE created_at >= $1
		GROUP BY endpoint
		ORDER BY total_requests DESC
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var endpoints []*models.TopEndpoint
	for rows.Next() {
		var e models.TopEndpoint
		if err := rows.Scan(&e.Endpoint, &e.TotalRequests, &e.BlockedCount); err != nil {
			return nil, err
		}
		if e.TotalRequests > 0 {
			e.BlockRate = float64(e.BlockedCount) / float64(e.TotalRequests)
		}
		endpoints = append(endpoints, &e)
	}
	return endpoints, nil
}

func (p *PostgresPool) GetRequestTimeSeries(ctx context.Context, apiKeyID string, since time.Time) ([]map[string]interface{}, error) {
	query := `
		SELECT
			date_trunc('minute', created_at) AS bucket,
			COUNT(*) AS total,
			SUM(CASE WHEN allowed THEN 1 ELSE 0 END) AS allowed,
			SUM(CASE WHEN NOT allowed THEN 1 ELSE 0 END) AS blocked
		FROM request_events
		WHERE created_at >= $1`

	args := []interface{}{since}
	if apiKeyID != "" {
		query += ` AND api_key_id = $2`
		args = append(args, apiKeyID)
	}
	query += ` GROUP BY bucket ORDER BY bucket`

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var series []map[string]interface{}
	for rows.Next() {
		var bucket time.Time
		var total, allowed, blocked int64
		if err := rows.Scan(&bucket, &total, &allowed, &blocked); err != nil {
			return nil, err
		}
		series = append(series, map[string]interface{}{
			"time":    bucket.Format(time.RFC3339),
			"total":   total,
			"allowed": allowed,
			"blocked": blocked,
		})
	}
	return series, nil
}