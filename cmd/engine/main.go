// Command engine is the bootstrapper for the High-Frequency Sweepstakes
// Casino Transaction Engine. It loads configuration, wires the Postgres pool,
// Redis client, idempotency cache, wallet engine, and HTTP router, then runs
// the server until SIGINT/SIGTERM, at which point it drains in-flight requests
// and closes every connection cleanly.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/True/internal/api"
	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/config"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/internal/telemetry"
	"github.com/Gavrielsh/True/internal/worker"
)

func main() {
	if err := run(); err != nil {
		// Logger may not be initialised yet if config failed, so use the
		// default. This is the only place os.Exit is allowed — keeping it here
		// means every deferred cleanup in run() executes before we exit.
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("configuration loaded", slog.Any("config", cfg))

	// Boot context: bounds the time we'll wait to establish connections.
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelBoot()

	// --- Distributed tracing --------------------------------------------------
	// Before everything else so every dependency below is born instrumented.
	// An empty OTEL_EXPORTER_OTLP_ENDPOINT yields a no-op tracer (dev mode).
	otelShutdown, err := telemetry.Init(bootCtx, "true-engine")
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer otelShutdown()

	// --- Postgres -----------------------------------------------------------
	pool, err := newPostgresPool(bootCtx, cfg)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres connected",
		slog.Int("max_conns", int(cfg.DB.MaxConns)),
		slog.Int("min_conns", int(cfg.DB.MinConns)))

	// --- Schema migrations ----------------------------------------------------
	// FAIL FAST: never serve traffic against a stale or dirty schema.
	//
	// Migrations run on the PRIVILEGED URL, not the runtime pool: applying them
	// needs CREATE ROLE / GRANT / CREATE TABLE, which the runtime role must not
	// hold (see assertLedgerAppendOnly below). MigrationPostgresURL falls back
	// to PostgresURL, so a single-role dev database is unaffected.
	if err := RunMigrations(bootCtx, logger, cfg.MigrationPostgresURL); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	// --- Ledger append-only invariant ----------------------------------------
	// Verify what the RUNTIME connection can actually do, after the migrations
	// that establish the grants have run. Fails the boot if this connection can
	// rewrite or erase settled money lines.
	if cfg.LedgerRoleCheckDisabled {
		logger.Warn("INSECURE: LEDGER_ROLE_CHECK=disabled — the engine is not " +
			"verifying that its database role is unable to mutate the ledger. " +
			"The privilege half of the append-only guarantee is NOT in force; only " +
			"the 000008 triggers remain. Never set this outside local development.")
	} else if err := assertLedgerAppendOnly(bootCtx, pool, logger); err != nil {
		return err
	}

	// --- Redis --------------------------------------------------------------
	rdb, err := newRedisClient(bootCtx, cfg)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer func() {
		if cerr := rdb.Close(); cerr != nil {
			logger.Warn("redis close", slog.String("error", cerr.Error()))
		}
	}()
	logger.Info("redis connected")

	// --- Wiring -------------------------------------------------------------
	idem := cache.NewRedis(rdb)
	// Third-party win ceiling. Parsed here so a malformed value fails the
	// boot rather than silently disabling the cap at request time.
	maxWin, err := domain.MoneyFromString(cfg.MaxProviderWin)
	if err != nil {
		return fmt.Errorf("MAX_PROVIDER_WIN %q: %w", cfg.MaxProviderWin, err)
	}
	if !maxWin.IsPositive() {
		return fmt.Errorf("MAX_PROVIDER_WIN must be > 0, got %s", maxWin)
	}
	logger.Info("third-party win ceiling active", slog.String("max_provider_win", maxWin.String()))

	eng := repository.New(pool, idem, logger, repository.WithMaxWinAmount(maxWin))
	casinoEng := repository.NewCasino(pool, idem, logger)
	// Server-authoritative game engine. Passing a nil RNG selects crypto/rand
	// — there is deliberately no config knob for a weaker entropy source.
	gameEng := repository.NewGame(pool, idem, nil, logger)

	// Geo-fence: jurisdiction blocking for restricted US states. FAIL CLOSED —
	// any request whose region cannot be positively established is rejected.
	// Running without it requires the explicit GEOFENCE_MODE=disabled opt-out.
	var geoFence *api.GeoFence
	if cfg.GeofenceDisabled {
		geoFence = api.NewDisabledGeoFence(logger)
	} else {
		geoFence, err = api.NewGeoFence(cfg.GeoIPDBPath, cfg.BlockedRegions, cfg.TrustedProxies, logger)
		if err != nil {
			return fmt.Errorf("geofence: %w", err)
		}
	}
	defer func() {
		if cerr := geoFence.Close(); cerr != nil {
			logger.Warn("geofence close", slog.String("error", cerr.Error()))
		}
	}()

	limiter := api.NewRateLimiter(rdb, logger, api.WithOperatorRPS(cfg.RateLimitRPS))

	router := api.NewRouter(api.Config{
		Engine:      eng,
		Casino:      casinoEng,
		Game:        gameEng,
		DB:          pool,
		Redis:       rdb,
		Secrets:     cfg.OperatorSecrets,
		GeoFence:    geoFence,
		RateLimiter: limiter,
		Logger:      logger,
	})

	srv := &http.Server{
		Addr:              addr(cfg.Port),
		Handler:           router,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// --- Admin API (separate engine + port; never via the operator gateway) --
	var adminSrv *http.Server
	if cfg.AdminAPIKey == "" {
		logger.Warn("admin API disabled: ADMIN_API_KEY is empty — " +
			"the admin server never runs unauthenticated")
	} else {
		adminRouter := api.NewAdminRouter(api.AdminConfig{
			APIKey: cfg.AdminAPIKey,
			DB:     pool,
			PoolStats: func() api.PoolStats {
				s := pool.Stat()
				return api.PoolStats{
					TotalConns: s.TotalConns(),
					IdleConns:  s.IdleConns(),
					InUseConns: s.AcquiredConns(),
					MaxConns:   s.MaxConns(),
				}
			},
			Redis:  rdb,
			Logger: logger,
		})
		adminSrv = &http.Server{
			Addr:              addr(cfg.AdminPort),
			Handler:           adminRouter,
			ReadTimeout:       cfg.ReadTimeout,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
		}
	}

	// --- Serve until signal -------------------------------------------------
	// signal.NotifyContext cancels ctx on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Background workers -------------------------------------------------
	// GGR aggregator: derives platform revenue from the ledger on an interval so
	// gameplay never contends on a single platform-wallet row (architecture
	// §6.C).
	//
	// Dedup pruner: trims ledger_transaction_dedup to its retention horizon in
	// small batches so the GLOBAL idempotency index stays bounded at 50k TPS
	// (architecture §6.B / migration 000005).
	//
	// Partitioner: pre-creates daily ledger partitions for today plus a
	// 14-day lookahead so a time-boundary INSERT can never fail on a missing
	// partition (migration 000005's ensure_ledger_partitions, Go-side).
	//
	// All observe ctx and stop cleanly on signal; we wait for their in-flight
	// cycles during shutdown. Each recovers per-cycle panics internally, and
	// runWorker adds an outermost defensive recover so a panic in the worker
	// scaffolding itself can never crash the HTTP server. A failed cycle (e.g.
	// migration 000007 not yet applied) is logged and retried — never fatal.
	// Reconciler: re-derives every wallet balance from the append-only ledger
	// and alerts (never corrects) on divergence — the integrity audit.
	aggregator := worker.New(pool, logger)
	pruner := worker.NewDedupPruner(pool, logger)
	partitioner := worker.NewPartitioner(pool, logger)
	reconciler := worker.NewReconciler(pool, logger)

	var workersWG sync.WaitGroup
	runWorker(ctx, &workersWG, logger, "ggr_aggregator", aggregator.Run)
	runWorker(ctx, &workersWG, logger, "dedup_pruner", pruner.Run)
	runWorker(ctx, &workersWG, logger, "partitioner", partitioner.Run)
	runWorker(ctx, &workersWG, logger, "reconciler", reconciler.Run)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	if adminSrv != nil {
		go func() {
			logger.Info("admin server listening", slog.String("addr", adminSrv.Addr))
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- fmt.Errorf("admin: %w", err)
			}
		}()
	}

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining in-flight requests")
	}

	// --- Graceful shutdown --------------------------------------------------
	// Stop accepting new connections and wait (bounded) for in-flight requests
	// to finish. The deferred pool.Close()/rdb.Close() run AFTER this returns,
	// so live transactions complete before their backing pools disappear.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("admin graceful shutdown", slog.String("error", err.Error()))
		}
	}

	// ctx (cancelled by the signal) also stops the aggregator; wait for its
	// current cycle to unwind before the deferred pool.Close() runs.
	workersWG.Wait()

	logger.Info("server stopped cleanly")
	return nil
}

// runWorker launches a background worker on wg with an outermost defensive
// panic guard. The workers already recover per-cycle (internal/worker), so this
// is belt-and-suspenders: even a panic in a worker's own loop scaffolding logs
// a stack and unwinds only this goroutine — never the process or HTTP server.
func runWorker(
	ctx context.Context,
	wg *sync.WaitGroup,
	logger *slog.Logger,
	name string,
	run func(context.Context) error,
) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Error("worker goroutine panic recovered",
					slog.String("worker", name),
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())))
			}
		}()
		if err := run(ctx); err != nil {
			logger.Error("worker exited with error",
				slog.String("worker", name),
				slog.String("error", err.Error()))
		}
	}()
}

// newLogger builds the global JSON slog logger, decorated so every record
// carries trace_id / operator_code from its context (cursor rule §9).
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(api.NewContextHandler(base))
}

// newPostgresPool builds and verifies the pgx pool from the HFT-ready
// cfg.DB tuning (MaxConns/MinConns/idle/lifetime + lifetime jitter).
func newPostgresPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	poolCfg.MaxConns = cfg.DB.MaxConns
	poolCfg.MinConns = cfg.DB.MinConns
	poolCfg.MaxConnIdleTime = cfg.DB.MaxConnIdleTime
	poolCfg.MaxConnLifetime = cfg.DB.MaxConnLifetime
	poolCfg.HealthCheckPeriod = cfg.DB.HealthCheckPeriod
	// Spread connection rotation over a jitter window so MaxConnLifetime never
	// expires a large cohort simultaneously (reconnect thundering-herd) — a
	// must at 50k TPS where a synchronized drop would stall the writer pool.
	poolCfg.MaxConnLifetimeJitter = cfg.DB.MaxConnLifetime / 10

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err) // FAIL CLOSED at boot
	}
	return pool, nil
}

// newRedisClient builds and verifies the go-redis client.
func newRedisClient(ctx context.Context, cfg config.Config) (*redis.Client, error) {
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	client := redis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping: %w", err) // FAIL CLOSED at boot
	}
	return client, nil
}

// addr normalises the configured port into a listen address. Accepts both
// "8080" and ":8080" (and a full "host:port") forms.
func addr(port string) string {
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}
