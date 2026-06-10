package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/metrics"
)

const (
	rxReconcileBatch    = `SELECT player_id\s+FROM wallets\s+WHERE player_id > \$1`
	rxReconcileMismatch = `WITH derived AS`
)

func newReconcilerMock(t *testing.T, opts ...ReconcileOption) (*Reconciler, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(func() {
		mock.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})
	return NewReconciler(mock, discardLogger(), opts...), mock
}

func playerIDRows(ids ...uuid.UUID) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"player_id"})
	for _, id := range ids {
		rows.AddRow(id)
	}
	return rows
}

func emptyMismatchRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"player_id", "currency", "wallet_balance", "ledger_balance"})
}

// sortedIDs returns n uuids in ascending order so keyset cursors are stable.
func sortedIDs(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		id := uuid.UUID{}
		id[15] = byte(i + 1)
		out[i] = id
	}
	return out
}

// Clean audit: every batch reconciles, no mismatch rows, counter untouched.
func TestReconcile_CleanSweep(t *testing.T) {
	t.Parallel()
	ids := sortedIDs(3)
	// batchSize 2 → one full batch (2) + one partial (1) ending the sweep.
	r, mock := newReconcilerMock(t, WithReconcileBatchSize(2), WithReconcileBatchPause(0))

	mock.ExpectQuery(rxReconcileBatch).WithArgs(uuid.Nil, 2).
		WillReturnRows(playerIDRows(ids[0], ids[1]))
	mock.ExpectQuery(rxReconcileMismatch).
		WithArgs([]string{ids[0].String(), ids[1].String()}).
		WillReturnRows(emptyMismatchRows())
	mock.ExpectQuery(rxReconcileBatch).WithArgs(ids[1], 2).
		WillReturnRows(playerIDRows(ids[2]))
	mock.ExpectQuery(rxReconcileMismatch).
		WithArgs([]string{ids[2].String()}).
		WillReturnRows(emptyMismatchRows())

	players, mismatches, err := r.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if players != 3 {
		t.Errorf("players: got %d want 3", players)
	}
	if mismatches != 0 {
		t.Errorf("mismatches: got %d want 0", mismatches)
	}
}

// Mismatch detection: the divergent (player, currency) is reported, the
// currency-labelled counter increments, and NO corrective statement is ever
// issued (pgxmock would fail on an unexpected Exec).
func TestReconcile_DetectsMismatch(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	r, mock := newReconcilerMock(t, WithReconcileBatchSize(10), WithReconcileBatchPause(0))

	mock.ExpectQuery(rxReconcileBatch).WithArgs(uuid.Nil, 10).
		WillReturnRows(playerIDRows(playerID))
	mismatch := pgxmock.NewRows([]string{"player_id", "currency", "wallet_balance", "ledger_balance"}).
		AddRow(playerID, "SC_REDEEMABLE",
			decimal.RequireFromString("105.0000"), // wallet says 105
			decimal.RequireFromString("100.0000")) // ledger derives 100
	mock.ExpectQuery(rxReconcileMismatch).
		WithArgs([]string{playerID.String()}).
		WillReturnRows(mismatch)

	before := testutil.ToFloat64(metrics.ReconciliationMismatches.WithLabelValues("SC_REDEEMABLE"))
	players, mismatches, err := r.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if players != 1 || mismatches != 1 {
		t.Errorf("players/mismatches: got %d/%d want 1/1", players, mismatches)
	}
	if got := testutil.ToFloat64(metrics.ReconciliationMismatches.WithLabelValues("SC_REDEEMABLE")); got != before+1 {
		t.Errorf("mismatch counter: got %v want %v", got, before+1)
	}
}

// fakeReconcileDB lets us inject panics and errors without pgxmock.
type fakeReconcileDB struct {
	panicOn bool
	err     error
	// called signals each Query so Run tests can synchronize deterministically.
	called chan struct{}
}

func (f *fakeReconcileDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	if f.panicOn {
		panic("synthetic reconcile panic")
	}
	return nil, f.err
}

// A panic inside a sweep is recovered and surfaced as an error — never crashes.
func TestReconcile_RecoversPanic(t *testing.T) {
	t.Parallel()
	r := NewReconciler(&fakeReconcileDB{panicOn: true}, discardLogger())
	if _, _, err := r.sweepSafely(context.Background()); err == nil {
		t.Fatal("expected recovered panic to surface as an error")
	}
}

// A DB error aborts the sweep and is wrapped (not swallowed).
func TestReconcile_ErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("connection reset")
	r := NewReconciler(&fakeReconcileDB{err: sentinel}, discardLogger())
	if _, _, err := r.sweepSafely(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("error: got %v want wrapping %v", err, sentinel)
	}
}

// A cancelled context stops the sweep before any statement is issued.
func TestReconcile_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	r, _ := newReconcilerMock(t) // zero expectations — any query would fail
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep on cancelled ctx: got %v want context.Canceled", err)
	}
}

// Run performs an initial sweep then returns nil on context cancel.
func TestReconciler_Run_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	// err path: the initial sweep fails fast and is logged — Run keeps going.
	db := &fakeReconcileDB{err: errors.New("not ready"), called: called}
	r := NewReconciler(db, discardLogger(), WithReconcileInterval(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("initial sweep never issued a query")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on cancel: got %v want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestReconciler_OptionGuards(t *testing.T) {
	t.Parallel()
	r := NewReconciler(&fakeReconcileDB{}, discardLogger(),
		WithReconcileInterval(-1),   // ignored
		WithReconcileBatchSize(0),   // ignored
		WithReconcileBatchPause(-1), // ignored
	)
	if r.interval != defaultReconcileInterval {
		t.Errorf("interval: got %v want %v", r.interval, defaultReconcileInterval)
	}
	if r.batchSize != defaultReconcileBatchSize {
		t.Errorf("batchSize: got %d want %d", r.batchSize, defaultReconcileBatchSize)
	}
	if r.batchPause != defaultReconcileBatchPause {
		t.Errorf("batchPause: got %v want %v", r.batchPause, defaultReconcileBatchPause)
	}
}
