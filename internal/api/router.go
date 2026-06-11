package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/True/internal/repository"
)

// Config bundles the dependencies needed to build the HTTP router. All are
// required except Logger (defaults to slog.Default).
type Config struct {
	Engine repository.Engine
	// Casino wires the real-money wrapper endpoints (player/create,
	// store/purchase, store/redeem). Optional: when nil those routes are not
	// registered (keeps the core-engine-only configuration valid).
	Casino repository.CasinoEngine
	// DB is the Postgres pool surface used by the deep health probe. A nil DB
	// makes /healthz report postgres as down (fail closed).
	DB      Pinger
	Redis   redis.UniversalClient
	Secrets map[string]string // operator_code → HMAC-SHA256 shared secret
	// GeoFence rejects blocked jurisdictions on /api/v1. Optional: nil (or a
	// disabled fence) skips geo-checking entirely (dev mode).
	GeoFence *GeoFence
	// RateLimiter enforces operator- and player-scope sliding windows.
	// Optional: nil disables rate limiting.
	RateLimiter *RateLimiter
	Logger      *slog.Logger
}

// NewRouter assembles the gin.Engine with the full middleware stack.
//
// Middleware order (outermost first):
//
//	Recovery   — turn panics into clean 500s, wraps everything.
//	Telemetry  — assign trace_id, log 4xx/5xx only.
//	[/api/v1]  HMAC      — verify operator + raw-body signature.
//	           ReplayGuard — timestamp window + nonce single-use.
//
// HMAC runs before ReplayGuard so the nonce is scoped to a *verified*
// operator; an unauthenticated caller never reaches the nonce store.
func NewRouter(cfg Config) *gin.Engine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// ReleaseMode: no debug logging, no per-route gin chatter (§9 hot path).
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(Recovery(logger))
	r.Use(Telemetry(logger))

	// Deep health probe — unauthenticated; verifies Postgres + Redis with a
	// bounded deadline and fails closed (503) when either is degraded.
	r.GET("/healthz", NewHealthHandler(cfg.DB, cfg.Redis, logger))

	// Prometheus scrape endpoint. Unauthenticated like /healthz — exposed for
	// the cluster-internal scraper, NOT routed through the operator gateway.
	// Serves the default registry, where internal/metrics registers the engine
	// instruments at init.
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	handlers := NewHandlers(cfg.Engine).WithRateLimiter(cfg.RateLimiter)
	hmacMW := NewHMACVerifier(cfg.Secrets).Middleware()
	replayMW := NewReplayGuard(cfg.Redis).Middleware()

	v1 := r.Group("/api/v1")
	// Operator-scope rate limiting sits AFTER HMAC (operator_code is verified,
	// never client-claimed) and BEFORE the replay guard (a throttled retry
	// must not burn its nonce).
	v1.Use(hmacMW)
	if cfg.RateLimiter != nil {
		v1.Use(cfg.RateLimiter.OperatorMiddleware())
	}
	v1.Use(replayMW)
	// Geo-fence runs AFTER HMAC + replay: only authenticated, non-replayed
	// operator traffic reaches the jurisdiction check, so the fence can never
	// be used as an unauthenticated region-probing oracle.
	if cfg.GeoFence.Enabled() {
		v1.Use(cfg.GeoFence.Middleware())
	}
	{
		v1.POST("/bet", handlers.Bet)
		v1.POST("/win", handlers.Win)
		v1.POST("/rollback", handlers.Rollback)
		// POST (not GET): the player_id must live in the HMAC-signed body —
		// an empty-body GET signature is the constant HMAC(secret, "") and
		// authenticates nothing. See Handlers.Session.
		v1.POST("/session", handlers.Session)

		// Real-money wrapper endpoints share the same HMAC + replay stack.
		if cfg.Casino != nil {
			casino := NewCasinoHandlers(cfg.Casino)
			v1.POST("/player/create", casino.CreatePlayer)
			v1.POST("/store/purchase", casino.Purchase)
			v1.POST("/store/redeem", casino.Redeem)
		}
	}

	return r
}
