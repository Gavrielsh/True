package api

import (
	"log/slog"
	"net/http"

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
	Casino  repository.CasinoEngine
	Redis   redis.UniversalClient
	Secrets map[string]string // operator_code → HMAC-SHA256 shared secret
	Logger  *slog.Logger
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

	// Liveness probe — unauthenticated, intentionally trivial.
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Prometheus scrape endpoint. Unauthenticated like /healthz — exposed for
	// the cluster-internal scraper, NOT routed through the operator gateway.
	// Serves the default registry, where internal/metrics registers the engine
	// instruments at init.
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	handlers := NewHandlers(cfg.Engine)
	hmacMW := NewHMACVerifier(cfg.Secrets).Middleware()
	replayMW := NewReplayGuard(cfg.Redis).Middleware()

	v1 := r.Group("/api/v1")
	v1.Use(hmacMW, replayMW)
	{
		v1.POST("/bet", handlers.Bet)
		v1.POST("/win", handlers.Win)
		v1.POST("/rollback", handlers.Rollback)
		v1.GET("/session", handlers.Session)

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
