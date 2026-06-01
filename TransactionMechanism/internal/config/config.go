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

	// MaxDBConns sizes the pgx pool. Defaults to (2*CPU)+1 per architecture
	// §6.B (writer-node sizing to avoid context-switch collapse).
	MaxDBConns int32
}

// defaultMaxConns implements the architecture's writer-pool heuristic.
func defaultMaxConns() int {
	return 2*runtime.NumCPU() + 1
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
		MaxDBConns:        int32(l.positiveInt("MAX_DB_CONNS", defaultMaxConns())),
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
		slog.Int("max_db_conns", int(c.MaxDBConns)),
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

func (l *loader) positiveInt(key string, def int) int {
	if l.err != nil {
		return def
	}
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.err = fmt.Errorf("env %s: invalid int %q: %w", key, v, err)
		return def
	}
	if n <= 0 {
		l.err = fmt.Errorf("env %s: must be positive, got %d", key, n)
		return def
	}
	return n
}
