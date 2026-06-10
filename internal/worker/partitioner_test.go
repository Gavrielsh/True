package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// fakeRow satisfies pgx.Row with a fixed bool result or error.
type fakeRow struct {
	exists bool
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("fakeRow: expected exactly one scan target")
	}
	b, ok := dest[0].(*bool)
	if !ok {
		return errors.New("fakeRow: expected *bool scan target")
	}
	*b = r.exists
	return nil
}

// fakePartitionDB is a deterministic PartitionDB: existence checks answer from
// `exists`/`existsErr`, Exec answers from `execErr`, and either side can panic
// on demand — all without pgxmock's strict ordered expectations.
type fakePartitionDB struct {
	mu         sync.Mutex
	queryCalls int
	execCalls  int

	exists       bool
	existsErr    error
	execErr      error
	panicOnQuery bool
	// called, if non-nil, receives a non-blocking signal on every QueryRow so a
	// test can synchronize on "the cycle actually started" instead of sleeping.
	called chan struct{}
}

func (f *fakePartitionDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryCalls++
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	if f.panicOnQuery {
		panic("synthetic queryrow panic")
	}
	return fakeRow{exists: f.exists, err: f.existsErr}
}

func (f *fakePartitionDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls++
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.NewCommandTag("CREATE TABLE"), nil
}

func (f *fakePartitionDB) counts() (queries, execs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queryCalls, f.execCalls
}

// The generated DDL must follow migration 000005's naming (<parent>_YYYYMMDD)
// and standard Postgres range syntax with a strictly exclusive upper bound.
func TestPartitionDDL_NameAndBounds(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	if got, want := partitionName("ledger_entries", day), "ledger_entries_20260115"; got != want {
		t.Errorf("name: got %q want %q", got, want)
	}
	got := partitionDDL("ledger_entries", day)
	want := `CREATE TABLE IF NOT EXISTS "ledger_entries_20260115" PARTITION OF "ledger_entries" ` +
		`FOR VALUES FROM ('2026-01-15') TO ('2026-01-16')`
	if got != want {
		t.Errorf("ddl:\n got %q\nwant %q", got, want)
	}
}

// Month/year boundaries: the exclusive upper bound must roll over correctly.
func TestPartitionDDL_MonthAndYearRollover(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	got := partitionDDL("ledger_transactions", day)
	want := `CREATE TABLE IF NOT EXISTS "ledger_transactions_20261231" PARTITION OF "ledger_transactions" ` +
		`FOR VALUES FROM ('2026-12-31') TO ('2027-01-01')`
	if got != want {
		t.Errorf("ddl:\n got %q\nwant %q", got, want)
	}
}

// ensureFrom must pre-create today + lookahead for BOTH parents, skipping the
// partitions that already exist, with each DDL as its own statement.
func TestEnsureFrom_CreatesMissingAndSkipsExisting(t *testing.T) {
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

	base := time.Date(2026, 6, 10, 13, 45, 0, 0, time.UTC) // mid-day: must truncate to midnight
	existsRe := `SELECT to_regclass\(\$1\) IS NOT NULL`

	// Day 0: ledger_entries already exists (skipped), ledger_transactions created.
	mock.ExpectQuery(existsRe).WithArgs("ledger_entries_20260610").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(existsRe).WithArgs("ledger_transactions_20260610").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS "ledger_transactions_20260610" PARTITION OF "ledger_transactions" FOR VALUES FROM \('2026-06-10'\) TO \('2026-06-11'\)`).
		WillReturnResult(pgxmock.NewResult("CREATE TABLE", 0))
	// Day 1 (lookahead): both created.
	mock.ExpectQuery(existsRe).WithArgs("ledger_entries_20260611").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS "ledger_entries_20260611" PARTITION OF "ledger_entries" FOR VALUES FROM \('2026-06-11'\) TO \('2026-06-12'\)`).
		WillReturnResult(pgxmock.NewResult("CREATE TABLE", 0))
	mock.ExpectQuery(existsRe).WithArgs("ledger_transactions_20260611").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS "ledger_transactions_20260611" PARTITION OF "ledger_transactions" FOR VALUES FROM \('2026-06-11'\) TO \('2026-06-12'\)`).
		WillReturnResult(pgxmock.NewResult("CREATE TABLE", 0))

	p := NewPartitioner(mock, discardLogger(), WithLookaheadDays(1))
	created, skipped, err := p.ensureFrom(context.Background(), base)
	if err != nil {
		t.Fatalf("ensureFrom: %v", err)
	}
	if created != 3 {
		t.Errorf("created: got %d want 3", created)
	}
	if skipped != 1 {
		t.Errorf("skipped: got %d want 1", skipped)
	}
}

// A DDL failure aborts the cycle and is wrapped with the partition name.
func TestEnsureFrom_DDLErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("deadlock detected")
	db := &fakePartitionDB{exists: false, execErr: sentinel}
	p := NewPartitioner(db, discardLogger(), WithLookaheadDays(14))

	created, _, err := p.ensureSafely(context.Background(), time.Now())
	if !errors.Is(err, sentinel) {
		t.Fatalf("error: got %v want wrapping %v", err, sentinel)
	}
	if created != 0 {
		t.Errorf("created: got %d want 0", created)
	}
	if _, execs := db.counts(); execs != 1 {
		t.Errorf("execs after first failure: got %d want 1", execs)
	}
}

// A panic inside a cycle is recovered and surfaced as an error — never crashes.
func TestEnsureFrom_RecoversPanic(t *testing.T) {
	t.Parallel()
	db := &fakePartitionDB{panicOnQuery: true}
	p := NewPartitioner(db, discardLogger())

	if _, _, err := p.ensureSafely(context.Background(), time.Now()); err == nil {
		t.Fatal("expected recovered panic to surface as an error")
	}
}

// A cancelled context stops the cycle before any statement is issued.
func TestEnsureFrom_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	db := &fakePartitionDB{}
	p := NewPartitioner(db, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, _, err := p.ensureFrom(ctx, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ensureFrom on cancelled ctx: got %v want context.Canceled", err)
	}
	if queries, execs := db.counts(); queries != 0 || execs != 0 {
		t.Errorf("statements on cancelled ctx: got %d/%d want 0/0", queries, execs)
	}
}

// Run performs an initial cycle then returns nil on context cancel. Synchronize
// on the first existence check so the assertion can never race the scheduler.
func TestPartitioner_Run_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	db := &fakePartitionDB{exists: true, called: called} // everything pre-exists
	p := NewPartitioner(db, discardLogger(), WithPartitionInterval(time.Hour), WithLookaheadDays(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("initial cycle never issued an existence check")
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
	if queries, _ := db.counts(); queries < 1 {
		t.Error("expected at least the initial cycle to run")
	}
}

func TestPartitioner_OptionGuards(t *testing.T) {
	t.Parallel()
	p := NewPartitioner(&fakePartitionDB{}, discardLogger(),
		WithPartitionInterval(-1), // ignored
		WithLookaheadDays(-7),     // ignored
	)
	if p.interval != defaultPartitionInterval {
		t.Errorf("interval: got %v want %v", p.interval, defaultPartitionInterval)
	}
	if p.lookaheadDays != defaultPartitionLookaheadDays {
		t.Errorf("lookaheadDays: got %d want %d", p.lookaheadDays, defaultPartitionLookaheadDays)
	}
}
