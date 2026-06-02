package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeExecDB is a deterministic ExecDB: it returns a per-call RowsAffected from
// `sequence` (the last value repeats), can inject an error, and can panic on a
// chosen call — all without pgxmock's strict ordered expectations.
type fakeExecDB struct {
	mu       sync.Mutex
	calls    int
	sequence []int64 // rows affected per call; last entry repeats
	err      error
	panicOn  int // 1-based call index to panic on; 0 = never
	// called, if non-nil, receives a non-blocking signal on every Exec entry so
	// a test can synchronize on "the sweep actually issued a statement" instead
	// of guessing with a sleep.
	called chan struct{}
}

func (f *fakeExecDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	if f.panicOn == f.calls {
		panic("synthetic exec panic")
	}
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	var rows int64
	switch {
	case len(f.sequence) == 0:
		rows = 0
	case f.calls-1 < len(f.sequence):
		rows = f.sequence[f.calls-1]
	default:
		rows = f.sequence[len(f.sequence)-1]
	}
	return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", rows)), nil
}

func (f *fakeExecDB) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sweep must keep deleting full batches until a short batch signals the backlog
// is drained, returning the running total.
func TestPruneSweep_BatchesUntilDrained(t *testing.T) {
	t.Parallel()
	db := &fakeExecDB{sequence: []int64{2, 2, 1}}
	p := NewDedupPruner(db, discardLogger(), WithBatchSize(2), WithBatchPause(0))

	total, err := p.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if total != 5 {
		t.Errorf("total deleted: got %d want 5", total)
	}
	if db.callCount() != 3 {
		t.Errorf("batch statements: got %d want 3", db.callCount())
	}
}

// A first short batch ends the sweep after a single statement.
func TestPruneSweep_SingleShortBatch(t *testing.T) {
	t.Parallel()
	db := &fakeExecDB{sequence: []int64{10}}
	p := NewDedupPruner(db, discardLogger(), WithBatchSize(5000), WithBatchPause(0))

	total, err := p.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if total != 10 {
		t.Errorf("total: got %d want 10", total)
	}
	if db.callCount() != 1 {
		t.Errorf("statements: got %d want 1", db.callCount())
	}
}

// A DB error aborts the sweep and is wrapped (not swallowed).
func TestPruneSweep_ErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("deadlock detected")
	db := &fakeExecDB{err: sentinel}
	p := NewDedupPruner(db, discardLogger(), WithBatchPause(0))

	_, err := p.sweepSafely(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("sweep error: got %v want wrapping %v", err, sentinel)
	}
}

// A panic inside a batch is recovered and surfaced as an error — never crashes.
func TestPruneSweep_RecoversPanic(t *testing.T) {
	t.Parallel()
	db := &fakeExecDB{panicOn: 1}
	p := NewDedupPruner(db, discardLogger(), WithBatchPause(0))

	deleted, err := p.sweepSafely(context.Background())
	if err == nil {
		t.Fatal("expected recovered panic to surface as an error")
	}
	if deleted != 0 {
		t.Errorf("deleted: got %d want 0", deleted)
	}
}

// A cancelled context stops the batch loop promptly.
func TestPruneSweep_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	// Always-full batches would loop forever; cancellation must break out.
	db := &fakeExecDB{sequence: []int64{100}}
	p := NewDedupPruner(db, discardLogger(), WithBatchSize(100), WithBatchPause(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := p.sweep(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep on cancelled ctx: got %v want context.Canceled", err)
	}
}

// Run performs an initial sweep then returns nil on context cancel. We
// synchronize on the initial sweep's first Exec (via the `called` channel)
// before cancelling, so the assertion can never race the goroutine scheduler —
// a sleep-then-cancel could fire before Run was scheduled, leaving callCount 0.
func TestPrune_Run_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	db := &fakeExecDB{sequence: []int64{0}, called: called} // nothing to delete
	p := NewDedupPruner(db, discardLogger(), WithPruneInterval(time.Hour), WithBatchPause(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Wait until the initial sweep has actually issued a prune statement, THEN
	// cancel — deterministic, no timing assumptions.
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("initial sweep never issued a prune statement")
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
	if db.callCount() < 1 {
		t.Error("expected at least the initial sweep to run")
	}
}

// SQL-shape check via pgxmock: the worker issues the batched ctid DELETE with
// (retention-seconds, batch-size) args.
func TestPruneSweep_IssuesBatchedDelete(t *testing.T) {
	t.Parallel()
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

	mock.ExpectExec(`DELETE FROM ledger_transaction_dedup\s+WHERE ctid IN`).
		WithArgs(pgxmock.AnyArg(), 250).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))

	p := NewDedupPruner(mock, discardLogger(), WithBatchSize(250), WithBatchPause(0))
	total, err := p.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if total != 3 {
		t.Errorf("total: got %d want 3", total)
	}
}

func TestPrune_OptionGuards(t *testing.T) {
	t.Parallel()
	p := NewDedupPruner(&fakeExecDB{}, discardLogger(),
		WithPruneInterval(-1), // ignored
		WithRetention(0),      // ignored
		WithBatchSize(-5),     // ignored
		WithBatchPause(-1),    // ignored
	)
	if p.interval != defaultPruneInterval {
		t.Errorf("interval: got %v want %v", p.interval, defaultPruneInterval)
	}
	if p.retention != defaultPruneRetention {
		t.Errorf("retention: got %v want %v", p.retention, defaultPruneRetention)
	}
	if p.batchSize != defaultPruneBatchSize {
		t.Errorf("batchSize: got %d want %d", p.batchSize, defaultPruneBatchSize)
	}
	if p.batchPause != defaultPruneBatchPause {
		t.Errorf("batchPause: got %v want %v", p.batchPause, defaultPruneBatchPause)
	}
}
