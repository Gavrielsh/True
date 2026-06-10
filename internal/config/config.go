// Package config loads and validates the engine's runtime configuration from
// environment variables. Load() is the single source of truth; it fails fast
// (returns an error) if any required variable is missing or malformed, so a
// misconfigured deploy never boots into a half-working state.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved, validated runtime configuration.
type Config struct {
	Port        string
	PostgresURL string
	RedisURL    string

	// OperatorSecrets maps operator_code → HMAC-SHA256 shared secret.
	// Never logged (see LogValue).
	OperatorSecrets map[string]string

	LogLevel string

	// HTTP server timeouts.
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// requests to drain before forcing the server closed.
	ShutdownTimeout time.Duration

	// AdminPort serves the standalone admin API (separate gin.Engine, never
	// exposed through the operator gateway).
	AdminPort string
	// AdminAPIKey authenticates X-Admin-Key on the admin API. Empty disables
	// the admin server entirely (never run it unauthenticated). Never logged.
	AdminAPIKey string

	// GeoIPDBPath locates the MaxMind GeoLite2 database. Empty disables
	// geo-fencing (dev mode; logged as WARN at startup).
	GeoIPDBPath string
	// BlockedRegions are ISO-3166-2 codes (e.g. US-WA) rejected by the
	// geo-fence. Parsed from comma-separated BLOCKED_REGIONS.
	BlockedRegions []string

	// DB holds the HFT-ready pgxpool tuning (see DBConfig).
	DB DBConfig
}

// DBConfig is the fully-resolved pgxpool tuning. The four primary knobs are
// settable via env: DB_MAX_CONNS, DB_MIN_CONNS, DB_MAX_CONN_IDLE_TIME,
// DB_MAX_CONN_LIFETIME (HealthCheckPeriod via DB_HEALTH_CHECK_PERIOD). Sized for
// 50k-TPS writer nodes where connection-pool exhaustion, not CPU, is the first
// wall (architecture §6.B).
type DBConfig struct {
	// MaxConns caps concurrent connections. Default (2*CPU)+1 — past that,
	// context-switching between backends collapses writer throughput.
	MaxConns int32
	// MinConns is the warm floor kept open so a slot-spin burst never pays
	// cold-connect (TCP + TLS + auth) latency on the hot path. Default: a
	// quarter of MaxConns, never below 2.
	MinConns int32
	// MaxConnIdleTime closes connections idle beyond this, releasing server
	// resources during troughs without churning the warm MinConns floor.
	MaxConnIdleTime time.Duration
	// MaxConnLifetime caps total connection age so the pool rotates steadily:
	// this rebalances backends after a failover/replica swap and bounds
	// server-side per-connection memory growth.
	MaxConnLifetime time.Duration
	// HealthCheckPeriod is how often the pool probes idle connections and tops
	// the pool back up to MinConns.
	HealthCheckPeriod time.Duration
}

// defaultMaxConns implements the architecture's writer-pool heuristic.
func defaultMaxConns() int {
	return 2*runtime.NumCPU() + 1
}

// defaultMinConns keeps a warm floor of ~25% of MaxConns (at least 2, never
// above MaxConns) so traffic bursts don't stall establishing connections.
func defaultMinConns(maxConns int) int {
	m := maxConns / 4
	if m < 2 {
		m = 2
	}
	if m > maxConns {
		m = maxConns
	}
	return m
}

// Load reads configuration from the process environment.
//
// Required: POSTGRES_URL, REDIS_URL, OPERATOR_SECRETS.
// Optional (with defaults): PORT (8080), LOG_LEVEL (info), the HTTP timeouts,
// SHUTDOWN_TIMEOUT (30s), MAX_DB_CONNS ((2*CPU)+1).
func Load() (Config, error) {
	var l loader

	cfg := Config{
		Port:              l.str("PORT", "8080"),
		PostgresURL:       l.required("POSTGRES_URL"),
		RedisURL:          l.required("REDIS_URL"),
		LogLevel:          l.str("LOG_LEVEL", "info"),
		ReadTimeout:       l.duration("HTTP_READ_TIMEOUT", 15*time.Second),
		ReadHeaderTimeout: l.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		WriteTimeout:      l.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
		IdleTimeout:       l.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout:   l.duration("SHUTDOWN_TIMEOUT", 30*time.Second),
		AdminPort:         l.str("ADMIN_PORT", "9090"),
		AdminAPIKey:       os.Getenv("ADMIN_API_KEY"),
		GeoIPDBPath:       l.str("GEOIP_DB_PATH", ""),
		BlockedRegions:    splitCSV(l.str("BLOCKED_REGIONS", "")),
		DB:                l.dbConfig(),
	}
	rawSecrets := l.required("OPERATOR_SECRETS")

	if l.err != nil {
		return Config{}, l.err
	}

	secrets, err := parseOperatorSecrets(rawSecrets)
	if err != nil {
		return Config{}, fmt.Errorf("OPERATOR_SECRETS: %w", err)
	}
	cfg.OperatorSecrets = secrets

	return cfg, nil
}

// LogValue implements slog.LogValuer so a Config can be logged at boot
// WITHOUT leaking secrets or DB/Redis credentials (cursor rule §9, DATA
// PRIVACY). Connection URLs are omitted entirely because they embed
// passwords; operator secrets are reduced to a count.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("port", c.Port),
		slog.String("log_level", c.LogLevel),
		slog.Int("operator_count", len(c.OperatorSecrets)),
		slog.Int("db_max_conns", int(c.DB.MaxConns)),
		slog.Int("db_min_conns", int(c.DB.MinConns)),
		slog.Duration("db_max_conn_idle_time", c.DB.MaxConnIdleTime),
		slog.Duration("db_max_conn_lifetime", c.DB.MaxConnLifetime),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Duration("read_timeout", c.ReadTimeout),
		slog.Duration("write_timeout", c.WriteTimeout),
	)
}

// parseOperatorSecrets accepts two formats:
//
//	JSON  : {"PRAGMATIC":"secret1","HACKSAW":"secret2"}
//	CSV   : PRAGMATIC:secret1,HACKSAW:secret2
//
// JSON is recommended when secrets contain commas. CSV splits each pair on
// the FIRST colon, so secrets may themselves contain colons.
func parseOperatorSecrets(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("must not be empty")
	}

	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return validateSecrets(m)
	}

	out := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		code, secret, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, fmt.Errorf("malformed pair %q (want code:secret)", pair)
		}
		code = strings.TrimSpace(code)
		secret = strings.TrimSpace(secret)
		if code == "" || secret == "" {
			return nil, fmt.Errorf("empty code or secret in %q", pair)
		}
		out[code] = secret
	}
	return validateSecrets(out)
}

// splitCSV splits a comma-separated list, trimming whitespace and dropping
// empty entries. Returns nil for an empty input.
func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validateSecrets(m map[string]string) (map[string]string, error) {
	if len(m) == 0 {
		return nil, errors.New("no operators defined")
	}
	for code, secret := range m {
		if strings.TrimSpace(code) == "" {
			return nil, errors.New("empty operator code")
		}
		if secret == "" {
			return nil, fmt.Errorf("empty secret for operator %q", code)
		}
	}
	return m, nil
}

// ----------------------------------------------------------------------------
// loader: accumulates the first error so Load() stays a flat struct literal.
// ----------------------------------------------------------------------------

type loader struct {
	err error
}

func (l *loader) str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	if l.err != nil {
		return ""
	}
	v := os.Getenv(key)
	if v == "" {
		l.err = fmt.Errorf("required env var %s is not set", key)
	}
	return v
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	if l.err != nil {
		return def
	}
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.err = fmt.Errorf("env %s: invalid duration %q: %w", key, v, err)
		return def
	}
	if d <= 0 {
		l.err = fmt.Errorf("env %s: must be positive, got %s", key, v)
		return def
	}
	return d
}

// dbConfig resolves the pgxpool tuning, applying HFT-ready defaults and
// validating MinConns <= MaxConns. The legacy MAX_DB_CONNS name is accepted as
// a fallback for DB_MAX_CONNS so existing deploys upgrade without a config edit.
func (l *loader) dbConfig() DBConfig {
	maxConns := l.intEnv([]string{"DB_MAX_CONNS", "MAX_DB_CONNS"}, defaultMaxConns(), false)
	minConns := l.intEnv([]string{"DB_MIN_CONNS"}, defaultMinConns(maxConns), true)

	cfg := DBConfig{
		MaxConns:          int32(maxConns),
		MinConns:          int32(minConns),
		MaxConnIdleTime:   l.duration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
		MaxConnLifetime:   l.duration("DB_MAX_CONN_LIFETIME", time.Hour),
		HealthCheckPeriod: l.duration("DB_HEALTH_CHECK_PERIOD", time.Minute),
	}
	if l.err == nil && minConns > maxConns {
		l.err = fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", minConns, maxConns)
	}
	return cfg
}

// intEnv reads the first present key among keys and parses a positive integer
// (or non-negative when allowZero). Returns def when none of the keys is set.
func (l *loader) intEnv(keys []string, def int, allowZero bool) int {
	if l.err != nil {
		return def
	}
	var raw, key string
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			raw, key = v, k
			break
		}
	}
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.err = fmt.Errorf("env %s: invalid int %q: %w", key, raw, err)
		return def
	}
	if n < 0 || (!allowZero && n == 0) {
		want := "positive"
		if allowZero {
			want = "non-negative"
		}
		l.err = fmt.Errorf("env %s: must be %s, got %d", key, want, n)
		return def
	}
	return n
}
