package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// PartitionDB is the minimal pgx surface the partitioner needs. Each DDL
// statement runs as its own autocommit execution (NEVER inside one long
// transaction) so catalog locks are released between statements. *pgxpool.Pool
// satisfies this; tests pass pgxmock or a fake.
type PartitionDB interface {
	ExecDB
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const (
	// Daily cadence: each cycle is idempotent, so one run per day is enough to
	// keep the runway full even across restarts (Run also sweeps at boot).
	defaultPartitionInterval = 24 * time.Hour
	// 14 days of pre-created partitions ahead of "today". An INSERT hitting a
	// missing partition fails the WHOLE transaction, so the runway must far
	// exceed any plausible worker outage.
	defaultPartitionLookaheadDays = 14
)

// partitionedParents are the RANGE(created_at)-partitioned ledger tables
// (migration 000005). Order is deterministic for tests and logs.
var partitionedParents = []string{"ledger_entries", "ledger_transactions"}

// sqlPartitionExists reports whether a relation with the given name already
// exists (search_path-resolved), letting the worker distinguish "created" from
// "skipped" in its logs. to_regclass returns NULL for a missing relation.
const sqlPartitionExists = `SELECT to_regclass($1) IS NOT NULL`

// Partitioner pre-creates the daily partitions of ledger_entries and
// ledger_transactions for today plus a lookahead window, so a time-boundary
// INSERT can never fail on a missing partition. It is the Go-side counterpart
// to the pg_cron-schedulable ensure_ledger_partitions() function (migration
// 000005) and uses the SAME <parent>_YYYYMMDD child naming — a different name
// for an already-covered range would make Postgres reject the DDL as an
// overlapping partition instead of skipping it.
type Partitioner struct {
	db     PartitionDB
	logger *slog.Logger

	interval      time.Duration
	lookaheadDays int
}

// PartitionOption configures the Partitioner.
type PartitionOption func(*Partitioner)

// WithPartitionInterval sets the cycle cadence (ignored if <= 0).
func WithPartitionInterval(d time.Duration) PartitionOption {
	return func(p *Partitioner) {
		if d > 0 {
			p.interval = d
		}
	}
}

// WithLookaheadDays sets how many days ahead of today are pre-created
// (ignored if < 0; 0 means today only).
func WithLookaheadDays(n int) PartitionOption {
	return func(p *Partitioner) {
		if n >= 0 {
			p.lookaheadDays = n
		}
	}
}

// NewPartitioner builds a partitioner. A nil logger falls back to slog.Default().
func NewPartitioner(db PartitionDB, logger *slog.Logger, opts ...PartitionOption) *Partitioner {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Partitioner{
		db:            db,
		logger:        logger,
		interval:      defaultPartitionInterval,
		lookaheadDays: defaultPartitionLookaheadDays,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run drives the ensure loop until ctx is cancelled (then returns nil). It
// runs an initial cycle at startup — so a freshly deployed or restarted
// service always has its runway — then on every tick. Like the other workers
// it never returns a non-nil error: a failed cycle is logged and retried on
// the next tick.
func (p *Partitioner) Run(ctx context.Context) error {
	p.logger.InfoContext(ctx, "partitioner started",
		slog.Duration("interval", p.interval),
		slog.Int("lookahead_days", p.lookaheadDays),
	)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Initial runway at boot.
	p.ensureCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.InfoContext(ctx, "partitioner stopped")
			return nil
		case <-ticker.C:
			p.ensureCycle(ctx)
		}
	}
}

// ensureCycle runs one panic-recovered cycle anchored at the current UTC day
// and logs the outcome.
func (p *Partitioner) ensureCycle(ctx context.Context) {
	created, skipped, err := p.ensureSafely(ctx, time.Now().UTC())
	if err != nil {
		// Cancellation during a cycle is a clean stop, not an error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		p.logger.ErrorContext(ctx, "partition ensure cycle failed",
			slog.String("error", err.Error()),
			slog.Int("created_before_failure", created),
		)
		return
	}
	p.logger.InfoContext(ctx, "partition ensure cycle complete",
		slog.Int("created", created),
		slog.Int("skipped", skipped),
	)
}

// ensureSafely wraps ensureFrom in panic recovery so a poisoned cycle can
// never unwind the worker goroutine (which would crash the HTTP server).
func (p *Partitioner) ensureSafely(ctx context.Context, base time.Time) (created, skipped int, err error) {
	gErr := guard(ctx, p.logger, "partitioner", func() error {
		var inner error
		created, skipped, inner = p.ensureFrom(ctx, base)
		return inner
	})
	return created, skipped, gErr
}

// ensureFrom pre-creates the partitions covering [day(base), day(base)+lookahead]
// for every partitioned parent. Each DDL is its own autocommit statement, and
// ctx is checked before every statement so cancellation aborts instantly —
// CREATE TABLE IF NOT EXISTS is idempotent, so a half-finished cycle leaves no
// inconsistent state for the next run.
func (p *Partitioner) ensureFrom(ctx context.Context, base time.Time) (created, skipped int, err error) {
	day0 := midnightUTC(base)
	for off := 0; off <= p.lookaheadDays; off++ {
		day := day0.AddDate(0, 0, off)
		for _, parent := range partitionedParents {
			if err := ctx.Err(); err != nil {
				return created, skipped, err
			}
			name := partitionName(parent, day)

			// Existence pre-check only refines logging (created vs skipped);
			// correctness is carried by IF NOT EXISTS, so the check-then-create
			// race with a concurrent replica is harmless.
			var exists bool
			if err := p.db.QueryRow(ctx, sqlPartitionExists, name).Scan(&exists); err != nil {
				return created, skipped, fmt.Errorf("check partition %s: %w", name, err)
			}
			if exists {
				skipped++
				p.logger.InfoContext(ctx, "partition already exists, skipped",
					slog.String("partition", name))
				continue
			}

			if _, err := p.db.Exec(ctx, partitionDDL(parent, day)); err != nil {
				p.logger.ErrorContext(ctx, "partition creation failed",
					slog.String("partition", name),
					slog.String("error", err.Error()),
				)
				return created, skipped, fmt.Errorf("create partition %s: %w", name, err)
			}
			created++
			p.logger.InfoContext(ctx, "partition created",
				slog.String("partition", name),
				slog.String("from", day.Format(dateLayout)),
				slog.String("to", day.AddDate(0, 0, 1).Format(dateLayout)),
			)
		}
	}
	return created, skipped, nil
}

const dateLayout = "2006-01-02"

// partitionName matches migration 000005's create_daily_partition convention:
// <parent>_YYYYMMDD.
func partitionName(parent string, day time.Time) string {
	return parent + "_" + day.Format("20060102")
}

// partitionDDL renders the idempotent one-day partition DDL. The upper bound is
// the next day, strictly exclusive per Postgres range-partition semantics.
// Identifiers are quoted via pgx.Identifier (DDL cannot take bind parameters);
// both name parts and the date literals come from compile-time constants and
// time formatting, never from external input.
func partitionDDL(parent string, day time.Time) string {
	return fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
		pgx.Identifier{partitionName(parent, day)}.Sanitize(),
		pgx.Identifier{parent}.Sanitize(),
		day.Format(dateLayout),
		day.AddDate(0, 0, 1).Format(dateLayout),
	)
}

// midnightUTC truncates t to the start of its UTC calendar day.
func midnightUTC(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
