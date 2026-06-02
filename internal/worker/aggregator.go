// Package worker holds the background goroutines that run alongside the HTTP
// server. The GGR aggregator derives platform revenue asynchronously from the
// ledger so real-time gameplay never contends on a single "platform wallet" row
// (architecture §6.C — Platform Revenue Hotspots).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// DB is the minimal pgxpool surface the aggregator needs: a single Begin so the
// whole rollup-and-advance-watermark cycle runs in one transaction. Production
// passes *pgxpool.Pool; tests pass pgxmock.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

const (
	defaultInterval  = time.Minute
	defaultSafetyLag = time.Minute
	defaultGamingTZ  = "UTC"
)

// Aggregator periodically rolls ledger_entries up into daily_ggr using a high
// watermark for exactly-once accounting.
type Aggregator struct {
	db     DB
	logger *slog.Logger

	// interval is how often a cycle runs.
	interval time.Duration
	// safetyLag excludes rows newer than now()-safetyLag from each cycle. This
	// closes the "in-flight commit" gap: created_at is the transaction START
	// time (now() is stable within a tx), so a bet that started before the
	// watermark but commits after our read could otherwise be skipped forever.
	// safetyLag MUST exceed the maximum possible transaction duration; the core
	// engine's lock window is sub-millisecond, so the 1-minute default is a
	// ~1000x margin.
	safetyLag time.Duration
	// gamingTZ is the timezone whose calendar day defines a "gaming date".
	gamingTZ string
}

// Option configures the Aggregator.
type Option func(*Aggregator)

// WithInterval sets the cycle period (ignored if <= 0).
func WithInterval(d time.Duration) Option {
	return func(a *Aggregator) {
		if d > 0 {
			a.interval = d
		}
	}
}

// WithSafetyLag sets how far behind "now" each cycle stops (ignored if < 0).
func WithSafetyLag(d time.Duration) Option {
	return func(a *Aggregator) {
		if d >= 0 {
			a.safetyLag = d
		}
	}
}

// WithGamingTimezone sets the timezone used to bucket entries into gaming days.
func WithGamingTimezone(tz string) Option {
	return func(a *Aggregator) {
		if tz != "" {
			a.gamingTZ = tz
		}
	}
}

// New builds an Aggregator. A nil logger falls back to slog.Default().
func New(db DB, logger *slog.Logger, opts ...Option) *Aggregator {
	if logger == nil {
		logger = slog.Default()
	}
	a := &Aggregator{
		db:        db,
		logger:    logger,
		interval:  defaultInterval,
		safetyLag: defaultSafetyLag,
		gamingTZ:  defaultGamingTZ,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// SQL ------------------------------------------------------------------------

const (
	// Lock the single watermark row for the duration of the cycle. The FOR
	// UPDATE serialises overlapping cycles and multiple replicas: a second
	// worker blocks here, then reads the advanced watermark and finds an empty
	// window. Returns the exclusive lower bound for this cycle's window.
	sqlLockGGRState = `
		SELECT last_processed_at
		FROM ggr_aggregator_state
		WHERE id
		FOR UPDATE
	`

	// Roll up the window (last_processed_at, now()-safety] into daily_ggr.
	//   $1 = last_processed_at (exclusive lower bound)
	//   $2 = safety lag in seconds (double precision)
	//   $3 = gaming-day timezone
	// Net bets  = HOUSE_BET_POOL credits − debits (debits come from BET rollbacks).
	// Net wins  = HOUSE_WIN_POOL debits − credits.
	// GGR       = net bets − net wins. Deltas ACCUMULATE via ON CONFLICT so a
	// gaming day spanning many cycles sums correctly.
	sqlAggregateGGR = `
		WITH delta AS (
			SELECT
				(e.created_at AT TIME ZONE $3)::date AS gaming_date,
				e.currency,
				COALESCE(SUM(e.amount) FILTER (WHERE e.account_type = 'HOUSE_BET_POOL' AND e.direction = 'CREDIT'), 0)
				  - COALESCE(SUM(e.amount) FILTER (WHERE e.account_type = 'HOUSE_BET_POOL' AND e.direction = 'DEBIT'), 0) AS bets,
				COALESCE(SUM(e.amount) FILTER (WHERE e.account_type = 'HOUSE_WIN_POOL' AND e.direction = 'DEBIT'), 0)
				  - COALESCE(SUM(e.amount) FILTER (WHERE e.account_type = 'HOUSE_WIN_POOL' AND e.direction = 'CREDIT'), 0) AS wins
			FROM ledger_entries e
			WHERE e.created_at > $1
			  AND e.created_at <= now() - make_interval(secs => $2)
			  AND e.account_type IN ('HOUSE_BET_POOL', 'HOUSE_WIN_POOL')
			GROUP BY 1, 2
		)
		INSERT INTO daily_ggr (gaming_date, currency, total_bets, total_wins, ggr, updated_at)
		SELECT gaming_date, currency, bets, wins, bets - wins, now()
		FROM delta
		ON CONFLICT (gaming_date, currency) DO UPDATE
		SET total_bets = daily_ggr.total_bets + EXCLUDED.total_bets,
		    total_wins = daily_ggr.total_wins + EXCLUDED.total_wins,
		    ggr        = daily_ggr.ggr + EXCLUDED.ggr,
		    updated_at = now()
	`

	// Advance the watermark to the SAME upper bound the aggregate used. now() is
	// the transaction start time (stable within the tx), and $1 is the same
	// safety-seconds value, so the new watermark equals the window's upper
	// bound exactly — no gap, no double-count. Atomic with the upsert.
	sqlAdvanceWatermark = `
		UPDATE ggr_aggregator_state
		SET last_processed_at = now() - make_interval(secs => $1),
		    last_run_at       = now()
		WHERE id
	`
)

// Run drives the aggregation loop until ctx is cancelled, at which point it
// returns nil (clean shutdown). It never returns a non-nil error: a failed
// cycle is logged and retried on the next tick, because the watermark only
// advances on commit, so no ledger row is ever lost or double-counted.
func (a *Aggregator) Run(ctx context.Context) error {
	a.logger.InfoContext(ctx, "ggr aggregator started",
		slog.Duration("interval", a.interval),
		slog.Duration("safety_lag", a.safetyLag),
		slog.String("gaming_tz", a.gamingTZ),
	)
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.InfoContext(ctx, "ggr aggregator stopped")
			return nil
		case <-ticker.C:
			n, err := a.runOnce(ctx)
			if err != nil {
				// Cancellation during a cycle is a clean stop, not an error.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					a.logger.InfoContext(ctx, "ggr aggregator stopped")
					return nil
				}
				a.logger.ErrorContext(ctx, "ggr aggregation cycle failed",
					slog.String("error", err.Error()))
				continue
			}
			if n > 0 {
				a.logger.InfoContext(ctx, "ggr aggregation cycle complete",
					slog.Int64("daily_ggr_rows_upserted", n))
			}
		}
	}
}

// runOnce executes one aggregation cycle in a single transaction: lock the
// watermark, upsert the rollup for (watermark, now()-safety], advance the
// watermark to the same upper bound, commit. Returns the number of daily_ggr
// rows upserted. Exported-ish via tests through the package.
func (a *Aggregator) runOnce(ctx context.Context) (int64, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lastProcessed time.Time
	if err := tx.QueryRow(ctx, sqlLockGGRState).Scan(&lastProcessed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("ggr_aggregator_state is empty; run migration 000007")
		}
		return 0, fmt.Errorf("lock watermark: %w", err)
	}

	safetySecs := a.safetyLag.Seconds()

	tag, err := tx.Exec(ctx, sqlAggregateGGR, lastProcessed, safetySecs, a.gamingTZ)
	if err != nil {
		return 0, fmt.Errorf("aggregate upsert: %w", err)
	}

	if _, err := tx.Exec(ctx, sqlAdvanceWatermark, safetySecs); err != nil {
		return 0, fmt.Errorf("advance watermark: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return tag.RowsAffected(), nil
}
