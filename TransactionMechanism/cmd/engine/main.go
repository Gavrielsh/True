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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/TransactionMechanism/internal/api"
	"github.com/Gavrielsh/TransactionMechanism/internal/cache"
	"github.com/Gavrielsh/TransactionMechanism/internal/config"
	"github.com/Gavrielsh/TransactionMechanism/internal/repository"
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

	// --- Postgres -----------------------------------------------------------
	pool, err := newPostgresPool(bootCtx, cfg)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres connected", slog.Int("max_conns", int(cfg.MaxDBConns)))

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
	eng := repository.New(pool, idem, logger)
	router := api.NewRouter(api.Config{
		Engine:  eng,
		Redis:   rdb,
		Secrets: cfg.OperatorSecrets,
		Logger:  logger,
	})

	srv := &http.Server{
		Addr:              addr(cfg.Port),
		Handler:           router,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// --- Serve until signal -------------------------------------------------
	// signal.NotifyContext cancels ctx on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

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
	logger.Info("server stopped cleanly")
	return nil
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

// newPostgresPool builds and verifies the pgx pool. MaxConns follows the
// architecture's (2*CPU)+1 writer heuristic via cfg.MaxDBConns.
func newPostgresPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxDBConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

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
