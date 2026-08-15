package repository

// settle_integration_test.go exercises the single-round-trip write path against
// a REAL PostgreSQL instance.
//
// These tests cannot be written against pgxmock: the properties under test —
// row-level locking, the dedup primary key, CTE atomicity — are behaviours of
// the database engine itself. A mock would only assert that we call the
// functions we call.
//
// Run with:
//
//	TEST_POSTGRES_URL=postgres://postgres@127.0.0.1:5433/true_engine?sslmode=disable \
//	    go test ./internal/repository/ -run Integration
//
// Skipped when TEST_POSTGRES_URL is unset, so the default suite stays hermetic.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/game"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// ----------------------------------------------------------------------------
// Harness
// ----------------------------------------------------------------------------

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL not set — skipping real-database integration test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_URL: %v", err)
	}
	// Enough connections that 100 goroutines genuinely contend rather than
	// queueing on the client side — the contention must reach Postgres.
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// openIdem is a Store that ALWAYS grants the lock and never caches.
//
// This is deliberate: it removes Redis from the picture so every goroutine
// reaches Postgres. What is under test is whether the DATABASE alone prevents
// double-spending — the Redis barrier is an optimisation in front of it, not
// the guarantee. If these tests pass with the barrier disabled, they pass with
// it enabled.
type openIdem struct{}

func (openIdem) Acquire(context.Context, string, string) (cache.AcquireStatus, string, error) {
	return cache.StatusAcquired, "", nil
}
func (openIdem) Store(context.Context, string, string, string) error { return nil }
func (openIdem) Release(context.Context, string) error               { return nil }

// losingRNG always produces CHERRY, LEMON, CHERRY — reels 1 and 2 differ, so
// the line pays nothing. Removes payout variance so balance assertions are
// exact.
type losingRNG struct {
	mu sync.Mutex
	i  int
}

func (r *losingRNG) Uint64N(n uint64) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// CHERRY occupies rolls [0,30), LEMON [30,55).
	seq := []uint64{0, 30, 0}
	v := seq[r.i%len(seq)]
	r.i++
	return v % n, nil
}

// seedPlayer creates a user and funds their wallet THROUGH THE LEDGER.
//
// The opening balance is posted as a real double-entry ADJUSTMENT — player
// CREDIT balanced by a HOUSE_ADJUSTMENT_POOL DEBIT — rather than written
// straight into the wallets row.
//
// This matters: `wallets` is a materialized cache of the ledger, so the
// production invariant is ABSOLUTE equality between the two. Seeding a balance
// without a ledger trail would fabricate money the ledger never saw and force
// the assertion to be weakened to a delta comparison, which would no longer be
// the check the reconciliation worker actually performs.
func seedPlayer(t *testing.T, pool *pgxpool.Pool, scUnplayed string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ext := "it-" + uuid.NewString()

	var playerID uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO users (external_id, username, country_code, status)
		 VALUES ($1, $1, 'US', 'ACTIVE') RETURNING id`, ext).Scan(&playerID)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO wallets (player_id, gc_balance, sc_unplayed_balance, sc_redeemable_balance)
		 VALUES ($1, 0, 0, 0)`, playerID); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
	creditOpeningBalance(t, pool, playerID, "SC_UNPLAYED", scUnplayed)
	return playerID
}

// creditOpeningBalance posts a ledger-backed credit to one currency bucket,
// keeping the wallets cache and the ledger in agreement.
func creditOpeningBalance(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID, currency, amount string) {
	t.Helper()
	ctx := context.Background()

	column := map[string]string{
		"GC":            "gc_balance",
		"SC_UNPLAYED":   "sc_unplayed_balance",
		"SC_REDEEMABLE": "sc_redeemable_balance",
	}[currency]
	if column == "" {
		t.Fatalf("unknown currency %q", currency)
	}

	opTxID := "seed-" + uuid.NewString()
	var ledgerTxID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO ledger_transactions (
			operator_code, operator_transaction_id, player_id, transaction_type,
			status, request_metadata, completed_at)
		VALUES ('SEED', $1, $2, 'ADJUSTMENT', 'COMPLETED', '{}'::jsonb, now())
		RETURNING id`, opTxID, playerID).Scan(&ledgerTxID); err != nil {
		t.Fatalf("seed ledger tx: %v", err)
	}

	// Player credit + balancing house debit, then bring the cache into line.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		WITH bumped AS (
			UPDATE wallets SET %s = %s + $1::numeric WHERE player_id = $2
			RETURNING %s AS balance_after
		)
		INSERT INTO ledger_entries (
			ledger_transaction_id, player_id, account_type, currency, direction, amount, balance_after)
		SELECT $3::uuid, $2::uuid, 'PLAYER_WALLET'::account_type, $4::currency_type, 'CREDIT'::entry_direction, $1::numeric, b.balance_after FROM bumped b
		UNION ALL
		SELECT $3::uuid, NULL, 'HOUSE_ADJUSTMENT_POOL'::account_type, $4::currency_type, 'DEBIT'::entry_direction, $1::numeric, NULL`,
		column, column, column), amount, playerID, ledgerTxID, currency); err != nil {
		t.Fatalf("seed ledger entries: %v", err)
	}
}

func walletBalances(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) (gc, scU, scR decimal.Decimal) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT gc_balance, sc_unplayed_balance, sc_redeemable_balance FROM wallets WHERE player_id = $1`,
		playerID).Scan(&gc, &scU, &scR)
	if err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	return
}

// assertLedgerReconciles re-derives the wallet from the append-only ledger
// (Σ CREDIT − Σ DEBIT per currency) and compares it with the wallets cache —
// the same invariant the reconciliation worker enforces in production.
func assertLedgerReconciles(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.currency,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'CREDIT'), 0)
		     - COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'DEBIT'), 0) AS derived
		FROM (VALUES ('GC'),('SC_UNPLAYED'),('SC_REDEEMABLE')) AS c(currency)
		LEFT JOIN ledger_entries e
		       ON e.player_id = $1
		      AND e.account_type = 'PLAYER_WALLET'
		      AND e.currency::text = c.currency
		GROUP BY c.currency`, playerID)
	if err != nil {
		t.Fatalf("derive ledger balances: %v", err)
	}
	defer rows.Close()

	derived := map[string]decimal.Decimal{}
	for rows.Next() {
		var cur string
		var v decimal.Decimal
		if err := rows.Scan(&cur, &v); err != nil {
			t.Fatalf("scan derived: %v", err)
		}
		derived[cur] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("derived rows: %v", err)
	}

	gc, scU, scR := walletBalances(t, pool, playerID)
	for label, pair := range map[string][2]decimal.Decimal{
		"GC":            {gc, derived["GC"]},
		"SC_UNPLAYED":   {scU, derived["SC_UNPLAYED"]},
		"SC_REDEEMABLE": {scR, derived["SC_REDEEMABLE"]},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("RECONCILIATION MISMATCH %s: wallet=%s ledger=%s", label, pair[0], pair[1])
		}
	}
}

// assertDoubleEntryBalanced proves every ledger transaction is internally
// balanced: per currency, debits equal credits across ALL account types.
func assertDoubleEntryBalanced(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var unbalanced int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
			SELECT ledger_transaction_id, currency,
			       SUM(CASE WHEN direction='DEBIT' THEN amount ELSE -amount END) AS delta
			FROM ledger_entries
			GROUP BY ledger_transaction_id, currency
			HAVING SUM(CASE WHEN direction='DEBIT' THEN amount ELSE -amount END) <> 0
		) AS bad`).Scan(&unbalanced)
	if err != nil {
		t.Fatalf("balance check: %v", err)
	}
	if unbalanced != 0 {
		t.Errorf("%d (transaction, currency) groups are not double-entry balanced", unbalanced)
	}
}

func newIntegrationGame(pool *pgxpool.Pool) GameEngine {
	return NewGame(pool, openIdem{}, &losingRNG{}, discardLoggerRepo())
}

// ----------------------------------------------------------------------------
// Test 1 — same spin, 100 concurrent attempts
// ----------------------------------------------------------------------------

// TestIntegration_ConcurrentSameSpinSettlesOnce fires 100 goroutines at the
// SAME operator_transaction_id simultaneously, with the Redis barrier disabled.
//
// Exactly one must settle. The other 99 must receive the original receipt via
// Ghost-Spin recovery — NOT an error, and above all not a second debit. This
// is the property that makes an operator's retries safe.
func TestIntegration_ConcurrentSameSpinSettlesOnce(t *testing.T) {
	pool := integrationPool(t)
	eng := newIntegrationGame(pool)

	playerID := seedPlayer(t, pool, "1000.0000")
	spinID := "concurrent-same-" + uuid.NewString()

	const goroutines = 100
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		processed int
		recovered int
		failed    []error
	)

	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // release all at once to maximise real contention
			res, err := eng.ProcessSpin(context.Background(), SpinRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: spinID,
				PlayerID:              playerID,
				Family:                domain.FamilySC,
				BetAmount:             domain.MoneyFromInt(10),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failed = append(failed, err)
			case res.Status == StatusProcessed:
				processed++
			case res.Status == StatusGhostRecovered || res.Status == StatusCached:
				recovered++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failed) > 0 {
		t.Errorf("%d attempts errored; first: %v", len(failed), failed[0])
	}
	if processed != 1 {
		t.Errorf("exactly one attempt must settle: got %d processed, %d recovered", processed, recovered)
	}
	if processed+recovered != goroutines {
		t.Errorf("every attempt must resolve: processed=%d recovered=%d want %d total",
			processed, recovered, goroutines)
	}

	// The ledger must contain exactly ONE bet transaction for this spin.
	var betCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_transactions WHERE operator_transaction_id = $1`,
		spinID+betLegSuffix).Scan(&betCount); err != nil {
		t.Fatalf("count bet txs: %v", err)
	}
	if betCount != 1 {
		t.Errorf("ledger must hold exactly 1 bet transaction, got %d", betCount)
	}

	// And exactly one debit must have landed: 1000 − 10.
	_, scU, _ := walletBalances(t, pool, playerID)
	if want := decimal.RequireFromString("990.0000"); !scU.Equal(want) {
		t.Errorf("balance: got %s want %s — %d debits applied instead of 1",
			scU, want, decimal.RequireFromString("1000").Sub(scU).Div(decimal.NewFromInt(10)).IntPart())
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

// ----------------------------------------------------------------------------
// Test 2 — 100 distinct spins, balance for exactly one
// ----------------------------------------------------------------------------

// TestIntegration_ConcurrentDistinctSpinsCannotOverdraw is the double-spend
// test. 100 goroutines each attempt a DIFFERENT spin (so idempotency cannot
// help) against a wallet holding exactly one bet's worth of balance.
//
// Exactly one must succeed. The rest must be rejected for insufficient funds,
// and the balance must never go negative.
func TestIntegration_ConcurrentDistinctSpinsCannotOverdraw(t *testing.T) {
	pool := integrationPool(t)
	eng := newIntegrationGame(pool)

	// Exactly one 10.0000 bet is affordable.
	playerID := seedPlayer(t, pool, "10.0000")

	const goroutines = 100
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		succeeded    int
		insufficient int
		other        []error
	)

	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			<-start
			_, err := eng.ProcessSpin(context.Background(), SpinRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: fmt.Sprintf("distinct-%s-%d", uuid.NewString(), n),
				PlayerID:              playerID,
				Family:                domain.FamilySC,
				BetAmount:             domain.MoneyFromInt(10),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, errs.ErrInsufficientFunds):
				insufficient++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Errorf("%d attempts failed unexpectedly; first: %v", len(other), other[0])
	}
	if succeeded != 1 {
		t.Errorf("DOUBLE SPEND: exactly one spin must succeed, got %d (insufficient=%d)",
			succeeded, insufficient)
	}
	if insufficient != goroutines-1 {
		t.Errorf("expected %d insufficient-funds rejections, got %d", goroutines-1, insufficient)
	}

	gc, scU, scR := walletBalances(t, pool, playerID)
	if scU.IsNegative() || scR.IsNegative() || gc.IsNegative() {
		t.Fatalf("NEGATIVE BALANCE: gc=%s sc_unplayed=%s sc_redeemable=%s", gc, scU, scR)
	}
	if want := decimal.RequireFromString("0.0000"); !scU.Equal(want) {
		t.Errorf("sc_unplayed: got %s want %s", scU, want)
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

// ----------------------------------------------------------------------------
// Test 3 — sequential correctness, including a paying round
// ----------------------------------------------------------------------------

// TestIntegration_WinningRoundWritesBothLegs covers the branch the losing RNG
// skips: a paying spin must write BOTH ledger transactions, all four entry
// lines, and both dedup anchors — in one statement.
func TestIntegration_WinningRoundWritesBothLegs(t *testing.T) {
	pool := integrationPool(t)

	// Force CROWN/CROWN/CROWN: CROWN occupies the last 4 slots of the 100-wide
	// strip, so any roll in [96,100) selects it.
	eng := NewGame(pool, openIdem{}, &fixedRNG{value: 99}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "100.0000")
	spinID := "winning-" + uuid.NewString()

	res, err := eng.ProcessSpin(context.Background(), SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: spinID,
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             domain.MoneyFromInt(1),
	})
	if err != nil {
		t.Fatalf("spin: %v", err)
	}
	if res.Outcome.Line != game.LineThree {
		t.Fatalf("expected a three-of-a-kind, got %s (%v)", res.Outcome.Line, res.Outcome.Reels)
	}
	if res.WinLedgerTransactionID == nil {
		t.Fatal("a paying round must record a win ledger transaction")
	}

	// CROWN pays 400x on a stake of 1 → 400 credited to SC_REDEEMABLE.
	if want := decimal.RequireFromString("400.0000"); !res.WinAmount.Decimal().Equal(want) {
		t.Errorf("win amount: got %s want %s", res.WinAmount, want)
	}
	_, scU, scR := walletBalances(t, pool, playerID)
	if want := decimal.RequireFromString("99.0000"); !scU.Equal(want) {
		t.Errorf("sc_unplayed: got %s want %s", scU, want)
	}
	if want := decimal.RequireFromString("400.0000"); !scR.Equal(want) {
		t.Errorf("sc_redeemable: got %s want %s", scR, want)
	}

	// Four entry lines: player debit + house credit (bet), player credit +
	// house debit (win).
	var entryCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ledger_entries
		WHERE ledger_transaction_id IN (
			SELECT id FROM ledger_transactions WHERE operator_transaction_id IN ($1, $2))`,
		spinID+betLegSuffix, spinID+winLegSuffix).Scan(&entryCount); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if entryCount != 4 {
		t.Errorf("expected 4 ledger entries, got %d", entryCount)
	}

	// Both dedup anchors written — this is what makes a retry ghost-recover.
	var dedupCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_transaction_dedup WHERE operator_transaction_id IN ($1, $2)`,
		spinID+betLegSuffix, spinID+winLegSuffix).Scan(&dedupCount); err != nil {
		t.Fatalf("count dedup: %v", err)
	}
	if dedupCount != 2 {
		t.Errorf("expected 2 dedup anchors, got %d", dedupCount)
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

// TestIntegration_SuspendedPlayerRejected proves the compliance guard survived
// being folded into the locking query.
func TestIntegration_SuspendedPlayerRejected(t *testing.T) {
	pool := integrationPool(t)
	eng := newIntegrationGame(pool)

	playerID := seedPlayer(t, pool, "100.0000")
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET status = 'SUSPENDED' WHERE id = $1`, playerID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	_, err := eng.ProcessSpin(context.Background(), SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "suspended-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             domain.MoneyFromInt(1),
	})
	if !errors.Is(err, errs.ErrPlayerNotActive) {
		t.Fatalf("suspended player must be rejected: got %v", err)
	}

	// And nothing moved.
	_, scU, _ := walletBalances(t, pool, playerID)
	if want := decimal.RequireFromString("100.0000"); !scU.Equal(want) {
		t.Errorf("balance changed for a suspended player: got %s want %s", scU, want)
	}
}

// TestIntegration_SCAllocationSplitsAcrossBuckets pins the sweepstakes
// allocation rule end-to-end: a bet larger than SC_UNPLAYED drains it first,
// then draws the remainder from SC_REDEEMABLE — and both debits appear as
// separate ledger lines.
func TestIntegration_SCAllocationSplitsAcrossBuckets(t *testing.T) {
	pool := integrationPool(t)
	eng := newIntegrationGame(pool)

	ctx := context.Background()
	playerID := seedPlayer(t, pool, "3.0000")
	// Fund SC_REDEEMABLE through the ledger too, so reconciliation stays exact.
	creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "10.0000")

	spinID := "split-" + uuid.NewString()
	if _, err := eng.ProcessSpin(ctx, SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: spinID,
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             domain.MoneyFromInt(5), // 3 unplayed + 2 redeemable
	}); err != nil {
		t.Fatalf("spin: %v", err)
	}

	_, scU, scR := walletBalances(t, pool, playerID)
	if want := decimal.RequireFromString("0.0000"); !scU.Equal(want) {
		t.Errorf("sc_unplayed must be drained first: got %s want %s", scU, want)
	}
	if want := decimal.RequireFromString("8.0000"); !scR.Equal(want) {
		t.Errorf("sc_redeemable: got %s want %s", scR, want)
	}

	// Two player debits (one per bucket) + two house credits.
	var lines int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		WHERE ledger_transaction_id = (
			SELECT id FROM ledger_transactions WHERE operator_transaction_id = $1)`,
		spinID+betLegSuffix).Scan(&lines); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if lines != 4 {
		t.Errorf("a split bet must write 4 lines (2 debits + 2 house credits), got %d", lines)
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

// fixedRNG returns the same roll every time — used to force a chosen symbol.
type fixedRNG struct{ value uint64 }

func (r *fixedRNG) Uint64N(n uint64) (uint64, error) { return r.value % n, nil }

// discardLoggerRepo silences engine logging during integration runs so the
// output shows only test results.
func discardLoggerRepo() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
