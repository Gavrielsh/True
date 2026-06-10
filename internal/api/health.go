package api

// health.go implements the deep liveness/readiness probe: it verifies BOTH
// hard dependencies (Postgres, Redis) with a bounded deadline. Per the
// FAIL-CLOSED doctrine a degraded dependency means the engine cannot safely
// move money, so the probe returns 503 and the orchestrator stops routing
// traffic here.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// healthCheckTimeout bounds BOTH dependency pings. Probes are cheap and
// frequent; a slow answer is as actionable as a failed one.
const healthCheckTimeout = 2 * time.Second

// Pinger is the minimal pgxpool surface the health probe needs.
// *pgxpool.Pool satisfies it; tests inject pgxmock.
type Pinger interface {
	Ping(ctx context.Context) error
}

// healthResponse is the wire shape for /healthz. Error carries a static,
// credential-free description — NEVER the raw driver error, which may embed
// host/user fragments from the connection string.
type healthResponse struct {
	Status   string `json:"status"`
	Postgres string `json:"postgres"`
	Redis    string `json:"redis"`
	Error    string `json:"error,omitempty"`
}

// NewHealthHandler builds the deep health probe. Dependencies are pinged
// CONCURRENTLY so the worst-case probe latency is one timeout, not two. A nil
// dependency is reported as down (fail closed — a miswired boot must never
// look healthy).
func NewHealthHandler(db Pinger, rdb redis.UniversalClient, logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), healthCheckTimeout)
		defer cancel()

		var (
			wg              sync.WaitGroup
			pgErr, redisErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			pgErr = pingPostgres(ctx, db)
		}()
		go func() {
			defer wg.Done()
			redisErr = pingRedis(ctx, rdb)
		}()
		wg.Wait()

		resp := healthResponse{
			Status:   "ok",
			Postgres: upDown(pgErr == nil),
			Redis:    upDown(redisErr == nil),
		}
		if pgErr == nil && redisErr == nil {
			c.JSON(http.StatusOK, resp)
			return
		}

		resp.Status = "degraded"
		var parts []string
		if pgErr != nil {
			parts = append(parts, "postgres unreachable")
		}
		if redisErr != nil {
			parts = append(parts, "redis unreachable")
		}
		resp.Error = strings.Join(parts, "; ")

		// Raw driver errors go to structured logs only (they may carry
		// host/user fragments) — never into the response body.
		attrs := []any{slog.String("status", "degraded")}
		if pgErr != nil {
			attrs = append(attrs, slog.String("postgres_error", pgErr.Error()))
		}
		if redisErr != nil {
			attrs = append(attrs, slog.String("redis_error", redisErr.Error()))
		}
		logger.WarnContext(ctx, "health check degraded", attrs...)

		c.JSON(http.StatusServiceUnavailable, resp)
	}
}

func pingPostgres(ctx context.Context, db Pinger) error {
	if db == nil {
		return errNotConfigured
	}
	return db.Ping(ctx)
}

func pingRedis(ctx context.Context, rdb redis.UniversalClient) error {
	if rdb == nil {
		return errNotConfigured
	}
	return rdb.Ping(ctx).Err()
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

// errNotConfigured marks a dependency that was never wired in — treated
// exactly like an unreachable one (fail closed).
var errNotConfigured = notConfiguredError{}

type notConfiguredError struct{}

func (notConfiguredError) Error() string { return "dependency not configured" }
