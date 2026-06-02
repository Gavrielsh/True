package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func newAgg(t *testing.T, opts ...Option) (*Aggregator, pgxmock.PgxPoolIface) {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(mock, logger, opts...), mock
}

const (
	rxLockState = `SELECT last_processed_at FROM ggr_aggregator_state`
	rxAggregate = `INSERT INTO daily_ggr`
	rxAdvance   = `UPDATE ggr_aggregator_state`
)

func TestRunOnce_HappyPath_UpsertsAndAdvances(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t)
	last := time.Now().Add(-time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(rxLockState).
		WillReturnRows(pgxmock.NewRows([]string{"last_processed_at"}).AddRow(last))
	// Aggregate window: (last, now()-safety]. tz threaded through as "UTC".
	mock.ExpectExec(rxAggregate).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "UTC").
		WillReturnResult(pgxmock.NewResult("INSERT", 3))
	mock.ExpectExec(rxAdvance).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	n, err := agg.runOnce(context.Background())
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if n != 3 {
		t.Errorf("rows upserted: got %d want 3", n)
	}
}

func TestRunOnce_EmptyWindow_StillAdvancesWatermark(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t)

	mock.ExpectBegin()
	mock.ExpectQuery(rxLockState).
		WillReturnRows(pgxmock.NewRows([]string{"last_processed_at"}).AddRow(time.Now().Add(-time.Minute)))
	// No ledger activity in the window — 0 rows upserted...
	mock.ExpectExec(rxAggregate).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "UTC").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// ...but the watermark MUST still advance so the window can't grow unbounded.
	mock.ExpectExec(rxAdvance).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	n, err := agg.runOnce(context.Background())
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("rows upserted: got %d want 0", n)
	}
}

func TestRunOnce_GamingTimezoneThreaded(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t, WithGamingTimezone("America/New_York"))

	mock.ExpectBegin()
	mock.ExpectQuery(rxLockState).
		WillReturnRows(pgxmock.NewRows([]string{"last_processed_at"}).AddRow(time.Now().Add(-time.Hour)))
	mock.ExpectExec(rxAggregate).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "America/New_York").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxAdvance).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if _, err := agg.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
}

func TestRunOnce_StateRowMissing(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t)
	mock.ExpectBegin()
	mock.ExpectQuery(rxLockState).WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := agg.runOnce(context.Background())
	if err == nil {
		t.Fatal("expected error when watermark row is missing")
	}
}

func TestRunOnce_AggregateFails_RollsBack(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t)
	mock.ExpectBegin()
	mock.ExpectQuery(rxLockState).
		WillReturnRows(pgxmock.NewRows([]string{"last_processed_at"}).AddRow(time.Now().Add(-time.Hour)))
	mock.ExpectExec(rxAggregate).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "UTC").
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	if _, err := agg.runOnce(context.Background()); err == nil {
		t.Fatal("expected error when the aggregate upsert fails")
	}
}

func TestRunOnce_BeginFails(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t)
	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))
	if _, err := agg.runOnce(context.Background()); err == nil {
		t.Fatal("expected error on BEGIN failure")
	}
}

func TestRunOnce_CommitFails(t *testing.T) {
	t.Parallel()
	agg, mock := newAgg(t)
	mock.ExpectBegin()
	mock.ExpectQuery(rxLockState).
		WillReturnRows(pgxmock.NewRows([]string{"last_processed_at"}).AddRow(time.Now().Add(-time.Hour)))
	mock.ExpectExec(rxAggregate).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "UTC").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxAdvance).WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	if _, err := agg.runOnce(context.Background()); err == nil {
		t.Fatal("expected error on COMMIT failure")
	}
}

// Run must return promptly (nil) when its context is cancelled, without ever
// touching the DB (interval is long enough that no tick fires first).
func TestRun_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	agg, _ := newAgg(t, WithInterval(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agg.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on cancel: got %v want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// panicBeginDB satisfies the aggregator's DB interface and panics on Begin,
// simulating an unexpected fault (nil-deref, driver bug) inside a cycle.
type panicBeginDB struct{}

func (panicBeginDB) Begin(context.Context) (pgx.Tx, error) { panic("synthetic begin panic") }

// runOnceSafely must convert a panic into a returned error, never propagate it.
func TestRunOnceSafely_RecoversPanic(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agg := New(panicBeginDB{}, logger)

	n, err := agg.runOnceSafely(context.Background())
	if err == nil {
		t.Fatal("expected recovered panic to surface as an error")
	}
	if n != 0 {
		t.Errorf("rows: got %d want 0", n)
	}
}

// The Run loop must survive panicking cycles and exit cleanly (nil) on cancel —
// a panic in the worker must NEVER crash the process / HTTP server.
func TestRun_SurvivesPanickingCycles(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agg := New(panicBeginDB{}, logger, WithInterval(2*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- agg.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run through panics: got %v want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after panicking cycles + cancel")
	}
}

func TestWithOptions_GuardInvalidValues(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(nil, logger,
		WithInterval(-1),       // ignored
		WithSafetyLag(-1),      // ignored
		WithGamingTimezone(""), // ignored
	)
	if a.interval != defaultInterval {
		t.Errorf("interval: got %v want default %v", a.interval, defaultInterval)
	}
	if a.safetyLag != defaultSafetyLag {
		t.Errorf("safetyLag: got %v want default %v", a.safetyLag, defaultSafetyLag)
	}
	if a.gamingTZ != defaultGamingTZ {
		t.Errorf("gamingTZ: got %q want default %q", a.gamingTZ, defaultGamingTZ)
	}
}
