package worker

// reconciler.go is the engine's financial integrity audit: it re-derives
// every player's balance from the append-only ledger (Σ CREDIT − Σ DEBIT per
// currency) and compares it with the wallets cache. The ledger is the source
// of truth (architecture §3); the wallets table is a materialized view of
// it, so ANY divergence means a bug or tampering.
//
// OBSERVE-ONLY: the reconciler NEVER auto-corrects. A mismatch is logged at
// ERROR and counted in engine_reconciliation_mismatches_total{currency} for
// alerting — a human must review the ledger trail before any adjustment.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/metrics"
)

// ReconcileDB is the minimal pgx surface the reconciler needs. Each batch is
// two independent snapshot reads — no transaction, no locks, so the audit
// can never block OLTP. *pgxpool.Pool satisfies it; tests pass pgxmock.
type ReconcileDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const (
	// Six-hourly keeps drift detection latency low without measurable load.
	defaultReconcileInterval = 6 * time.Hour
	// 1000 players per batch bounds each aggregate's working set.
	defaultReconcileBatchSize = 1000
	// Brief yield between batches so the audit never starves OLTP traffic.
	defaultReconcileBatchPause = 100 * time.Millisecond
)

const (
	// sqlReconcileBatchPlayers pages the wallet table by player_id keyset —
	// stable under concurrent inserts, no OFFSET scans.
	sqlReconcileBatchPlayers = `
		SELECT player_id
		FROM wallets
		WHERE player_id > $1
		ORDER BY player_id
		LIMIT $2
	`

	// sqlReconcileMismatches re-derives each batched player's balance per
	// currency from ledger_entries (Σ CREDIT − Σ DEBIT over PLAYER_WALLET
	// rows) and returns ONLY the (player, currency) pairs whose wallets cache
	// disagrees. The CROSS JOIN over the three currencies makes a missing
	// ledger aggregate compare as 0 — catching a non-zero wallet with no
	// ledger trail at all.
	sqlReconcileMismatches = `
		WITH derived AS (
			SELECT e.player_id,
			       e.currency,
			       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'CREDIT'), 0)
			     - COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'DEBIT'), 0) AS ledger_balance
			FROM ledger_entries e
			WHERE e.account_type = 'PLAYER_WALLET'
			  AND e.player_id = ANY($1::uuid[])
			GROUP BY e.player_id, e.currency
		),
		expected AS (
			SELECT w.player_id,
			       c.currency,
			       CASE c.currency
			           WHEN 'GC'          THEN w.gc_balance
			           WHEN 'SC_UNPLAYED' THEN w.sc_unplayed_balance
			           ELSE                    w.sc_redeemable_balance
			       END AS wallet_balance,
			       COALESCE(d.ledger_balance, 0) AS ledger_balance
			FROM wallets w
			CROSS JOIN (VALUES ('GC'), ('SC_UNPLAYED'), ('SC_REDEEMABLE')) AS c(currency)
			LEFT JOIN derived d
			       ON d.player_id = w.player_id
			      AND d.currency::text = c.currency
			WHERE w.player_id = ANY($1::uuid[])
		)
		SELECT player_id, currency, wallet_balance, ledger_balance
		FROM expected
		WHERE wallet_balance <> ledger_balance
	`
)

// Reconciler periodically audits wallets against the ledger.
type Reconciler struct {
	db     ReconcileDB
	logger *slog.Logger

	interval   time.Duration
	batchSize  int
	batchPause time.Duration
}

// ReconcileOption configures the Reconciler.
type ReconcileOption func(*Reconciler)

// WithReconcileInterval sets the audit cadence (ignored if <= 0).
func WithReconcileInterval(d time.Duration) ReconcileOption {
	return func(r *Reconciler) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithReconcileBatchSize sets players audited per batch (ignored if <= 0).
func WithReconcileBatchSize(n int) ReconcileOption {
	return func(r *Reconciler) {
		if n > 0 {
			r.batchSize = n
		}
	}
}

// WithReconcileBatchPause sets the inter-batch yield (ignored if < 0).
func WithReconcileBatchPause(d time.Duration) ReconcileOption {
	return func(r *Reconciler) {
		if d >= 0 {
			r.batchPause = d
		}
	}
}

// NewReconciler builds a reconciler. A nil logger falls back to slog.Default().
func NewReconciler(db ReconcileDB, logger *slog.Logger, opts ...ReconcileOption) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reconciler{
		db:         db,
		logger:     logger,
		interval:   defaultReconcileInterval,
		batchSize:  defaultReconcileBatchSize,
		batchPause: defaultReconcileBatchPause,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run drives the audit loop until ctx is cancelled (then returns nil). Like
// the other workers it runs once at startup, then on the interval, and never
// returns a non-nil error — a failed sweep is logged and retried next cycle.
func (r *Reconciler) Run(ctx context.Context) error {
	r.logger.InfoContext(ctx, "reconciler started",
		slog.Duration("interval", r.interval),
		slog.Int("batch_size", r.batchSize),
	)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.sweepCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			r.logger.InfoContext(ctx, "reconciler stopped")
			return nil
		case <-ticker.C:
			r.sweepCycle(ctx)
		}
	}
}

// sweepCycle runs one panic-recovered audit and logs the outcome.
func (r *Reconciler) sweepCycle(ctx context.Context) {
	players, mismatches, err := r.sweepSafely(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		r.logger.ErrorContext(ctx, "reconciliation sweep failed",
			slog.String("error", err.Error()),
			slog.Int("players_checked_before_failure", players),
		)
		return
	}
	if mismatches > 0 {
		// The per-mismatch detail was already logged at ERROR by the sweep.
		r.logger.ErrorContext(ctx, "reconciliation sweep found mismatches",
			slog.Int("players_checked", players),
			slog.Int("mismatches", mismatches),
		)
		return
	}
	r.logger.InfoContext(ctx, "reconciliation sweep clean",
		slog.Int("players_checked", players))
}

// sweepSafely wraps sweep in panic recovery so a poisoned sweep can never
// unwind the worker goroutine (which would crash the HTTP server).
func (r *Reconciler) sweepSafely(ctx context.Context) (players, mismatches int, err error) {
	gErr := guard(ctx, r.logger, "reconciler", func() error {
		var inner error
		players, mismatches, inner = r.sweep(ctx)
		return inner
	})
	return players, mismatches, gErr
}

// sweep audits every player in keyset batches, yielding between batches.
func (r *Reconciler) sweep(ctx context.Context) (players, mismatches int, err error) {
	cursor := uuid.Nil
	for {
		if err := ctx.Err(); err != nil {
			return players, mismatches, err
		}
		ids, err := r.fetchBatch(ctx, cursor)
		if err != nil {
			return players, mismatches, err
		}
		if len(ids) == 0 {
			return players, mismatches, nil // all players audited
		}
		players += len(ids)
		cursor = ids[len(ids)-1]

		n, err := r.auditBatch(ctx, ids)
		if err != nil {
			return players, mismatches, err
		}
		mismatches += n

		if len(ids) < r.batchSize {
			return players, mismatches, nil // last (partial) batch
		}
		if !sleepCtx(ctx, r.batchPause) {
			return players, mismatches, ctx.Err()
		}
	}
}

// fetchBatch returns the next batchSize player ids after cursor.
func (r *Reconciler) fetchBatch(ctx context.Context, cursor uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, sqlReconcileBatchPlayers, cursor, r.batchSize)
	if err != nil {
		return nil, fmt.Errorf("fetch player batch: %w", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0, r.batchSize)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan player id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("player batch rows: %w", err)
	}
	return ids, nil
}

// auditBatch compares ledger-derived balances with the wallets cache for one
// batch and reports every mismatch. OBSERVE-ONLY: no UPDATE is ever issued.
func (r *Reconciler) auditBatch(ctx context.Context, ids []uuid.UUID) (int, error) {
	// uuid[] is passed as text and cast server-side — keeps the driver
	// encoding trivially portable across pgx and pgxmock.
	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = id.String()
	}

	rows, err := r.db.Query(ctx, sqlReconcileMismatches, idStrs)
	if err != nil {
		return 0, fmt.Errorf("mismatch query: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			playerID       uuid.UUID
			currency       string
			wallet, ledger decimal.Decimal
		)
		if err := rows.Scan(&playerID, &currency, &wallet, &ledger); err != nil {
			return count, fmt.Errorf("scan mismatch: %w", err)
		}
		count++
		metrics.ReconciliationMismatches.WithLabelValues(currency).Inc()
		r.logger.ErrorContext(ctx, "RECONCILIATION MISMATCH — wallet diverges from ledger",
			slog.String("player_id", playerID.String()),
			slog.String("currency", currency),
			slog.String("expected_ledger_balance", ledger.StringFixed(4)),
			slog.String("actual_wallet_balance", wallet.StringFixed(4)),
			slog.String("delta", wallet.Sub(ledger).StringFixed(4)),
		)
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("mismatch rows: %w", err)
	}
	return count, nil
}
