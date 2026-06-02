package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ExecDB is the minimal pgx surface the dedup pruner needs. Each batch runs as
// its own autocommit statement (NOT inside a long transaction) so row locks are
// released between batches. *pgxpool.Pool satisfies this; tests pass pgxmock.
type ExecDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const (
	// 30-day retention is far beyond any operator's webhook retry horizon
	// (hours–days), so a duplicate can never arrive after its dedup row is gone.
	defaultPruneRetention = 30 * 24 * time.Hour
	// Daily cadence keeps the global dedup index bounded without churn.
	defaultPruneInterval = 24 * time.Hour
	// Small batches bound lock duration and B-Tree index bloat per statement.
	defaultPruneBatchSize = 5000
	// Brief yield between batches so the sweep never starves OLTP traffic.
	defaultPruneBatchPause = 50 * time.Millisecond
)

// sqlPruneDedupBatch deletes at most $2 expired rows per statement. The ctid
// sub-select picks the oldest rows via ledger_transaction_dedup_created_at_idx
// (migration 000005), so each DELETE touches a bounded, index-ordered set —
// short locks, no full-table scan, and steady index maintenance instead of one
// monster DELETE that bloats the B-Tree and holds locks for minutes.
//
//	$1 = retention in seconds (double precision)
//	$2 = batch size
const sqlPruneDedupBatch = `
	DELETE FROM ledger_transaction_dedup
	WHERE ctid IN (
		SELECT ctid
		FROM ledger_transaction_dedup
		WHERE created_at < now() - make_interval(secs => $1)
		ORDER BY created_at
		LIMIT $2
	)
`

// DedupPruner periodically deletes rows older than the retention horizon from
// ledger_transaction_dedup, in bounded batches, so the GLOBAL (non-partitioned)
// idempotency index stays small even at 50k TPS (architecture §6.B / migration
// 000005). It is the Go-side counterpart to the pg_cron-schedulable
// prune_ledger_transaction_dedup_batched() procedure (migration 000008); deploy
// exactly one of the two.
type DedupPruner struct {
	db     ExecDB
	logger *slog.Logger

	interval   time.Duration
	retention  time.Duration
	batchSize  int
	batchPause time.Duration
}

// PruneOption configures the DedupPruner.
type PruneOption func(*DedupPruner)

// WithPruneInterval sets the full-sweep cadence (ignored if <= 0).
func WithPruneInterval(d time.Duration) PruneOption {
	return func(p *DedupPruner) {
		if d > 0 {
			p.interval = d
		}
	}
}

// WithRetention sets how long a dedup row is kept (ignored if <= 0).
func WithRetention(d time.Duration) PruneOption {
	return func(p *DedupPruner) {
		if d > 0 {
			p.retention = d
		}
	}
}

// WithBatchSize sets the max rows deleted per statement (ignored if <= 0).
func WithBatchSize(n int) PruneOption {
	return func(p *DedupPruner) {
		if n > 0 {
			p.batchSize = n
		}
	}
}

// WithBatchPause sets the yield between batches (ignored if < 0; 0 disables it).
func WithBatchPause(d time.Duration) PruneOption {
	return func(p *DedupPruner) {
		if d >= 0 {
			p.batchPause = d
		}
	}
}

// NewDedupPruner builds a pruner. A nil logger falls back to slog.Default().
func NewDedupPruner(db ExecDB, logger *slog.Logger, opts ...PruneOption) *DedupPruner {
	if logger == nil {
		logger = slog.Default()
	}
	p := &DedupPruner{
		db:         db,
		logger:     logger,
		interval:   defaultPruneInterval,
		retention:  defaultPruneRetention,
		batchSize:  defaultPruneBatchSize,
		batchPause: defaultPruneBatchPause,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run drives the prune loop until ctx is cancelled (then returns nil). It runs
// an initial sweep at startup — so a service that restarts often still prunes —
// then sweeps on the interval. Like the aggregator it never returns a non-nil
// error: a failed sweep is logged and retried next cycle.
func (p *DedupPruner) Run(ctx context.Context) error {
	p.logger.InfoContext(ctx, "dedup pruner started",
		slog.Duration("interval", p.interval),
		slog.Duration("retention", p.retention),
		slog.Int("batch_size", p.batchSize),
	)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Initial sweep at boot.
	p.sweepCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.InfoContext(ctx, "dedup pruner stopped")
			return nil
		case <-ticker.C:
			p.sweepCycle(ctx)
		}
	}
}

// sweepCycle runs one panic-recovered sweep and logs the outcome.
func (p *DedupPruner) sweepCycle(ctx context.Context) {
	deleted, err := p.sweepSafely(ctx)
	if err != nil {
		// Cancellation during a sweep is a clean stop, not an error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		p.logger.ErrorContext(ctx, "dedup prune sweep failed",
			slog.String("error", err.Error()),
			slog.Int64("rows_deleted_before_failure", deleted),
		)
		return
	}
	if deleted > 0 {
		p.logger.InfoContext(ctx, "dedup prune sweep complete",
			slog.Int64("rows_deleted", deleted))
	}
}

// sweepSafely wraps sweep in panic recovery so a poisoned sweep can never
// unwind the worker goroutine (which would crash the HTTP server).
func (p *DedupPruner) sweepSafely(ctx context.Context) (deleted int64, err error) {
	gErr := guard(ctx, p.logger, "dedup_pruner", func() error {
		var inner error
		deleted, inner = p.sweep(ctx)
		return inner
	})
	return deleted, gErr
}

// sweep deletes expired rows batch-by-batch until a short (less-than-full)
// batch signals the backlog is drained, pausing between batches to yield.
func (p *DedupPruner) sweep(ctx context.Context) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		tag, err := p.db.Exec(ctx, sqlPruneDedupBatch, p.retention.Seconds(), p.batchSize)
		if err != nil {
			return total, fmt.Errorf("prune batch: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < int64(p.batchSize) {
			return total, nil // last (partial) batch — backlog drained
		}
		if !sleepCtx(ctx, p.batchPause) {
			return total, ctx.Err()
		}
	}
}

// sleepCtx sleeps for d or until ctx is done. Returns true if the full duration
// elapsed, false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
