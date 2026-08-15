package repository

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/metrics"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// ----------------------------------------------------------------------------
// Fake idempotency store: in-memory, mirrors the Redis semantics our engine
// relies on (PROCESSING marker, XX semantics on Store, etc.) without pulling
// in miniredis. The cache package's own tests cover the Redis backing.
// ----------------------------------------------------------------------------

type fakeIdem struct {
	mu sync.Mutex

	state map[string]string

	acquireErr error
	storeErr   error
	releaseErr error

	stored   map[string]string // payloads passed to Store
	released map[string]int    // call-count per key
	// fingerprints records the fingerprint bound to each key, mirroring the
	// real store's "<fingerprint>|<payload>" layout.
	fingerprints map[string]string
}

func newFakeIdem() *fakeIdem {
	return &fakeIdem{
		state:        map[string]string{},
		stored:       map[string]string{},
		released:     map[string]int{},
		fingerprints: map[string]string{},
	}
}

func (f *fakeIdem) Acquire(_ context.Context, key, fingerprint string) (cache.AcquireStatus, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return cache.StatusUnknown, "", f.acquireErr
	}
	if v, ok := f.state[key]; ok {
		// Same binding check the Lua script performs: a key presented with a
		// different request is refused, never served the original's payload.
		if stored, seen := f.fingerprints[key]; seen && stored != fingerprint {
			return cache.StatusUnknown, "", cache.ErrFingerprintMismatch
		}
		if v == cache.ProcessingMarker {
			return cache.StatusPending, "", nil
		}
		return cache.StatusCached, v, nil
	}
	f.state[key] = cache.ProcessingMarker
	f.fingerprints[key] = fingerprint
	return cache.StatusAcquired, "", nil
}

func (f *fakeIdem) Store(_ context.Context, key, fingerprint, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return f.storeErr
	}
	if _, ok := f.state[key]; !ok {
		return nil // XX semantics
	}
	f.state[key] = payload
	f.stored[key] = payload
	f.fingerprints[key] = fingerprint
	return nil
}

func (f *fakeIdem) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released[key]++
	if f.releaseErr != nil {
		return f.releaseErr
	}
	delete(f.state, key)
	delete(f.fingerprints, key)
	return nil
}

// ----------------------------------------------------------------------------
// Test fixtures
// ----------------------------------------------------------------------------

const (
	operatorCode = "PRAGMATIC"
	gameID       = "GAME-42"
	roundID      = "ROUND-99"
)

// argDec is a pgxmock argument matcher that compares decimal.Decimal values
// by numeric equality (Cmp == 0), not by reflect.DeepEqual. The default
// matcher would compare the internal *big.Int pointers and would flag two
// "0.0000" values built from different parse paths as unequal even though
// they represent the same number.
type argDec struct{ want decimal.Decimal }

func dec(s string) argDec { return argDec{want: decimal.RequireFromString(s)} }

func (a argDec) Match(v any) bool {
	got, ok := v.(decimal.Decimal)
	if !ok {
		return false
	}
	return a.want.Equal(got)
}

func mustMoney(t *testing.T, s string) domain.Money {
	t.Helper()
	m, err := domain.MoneyFromString(s)
	if err != nil {
		t.Fatalf("money %q: %v", s, err)
	}
	return m
}

// newEngine returns a fresh engine wired to a regex-matcher pgxmock pool
// (regex avoids brittle whitespace-equality on multi-line SQL constants)
// and a clean fake idempotency store. The slog drain is /dev/null so test
// output stays focused on failures.
func newEngine(t *testing.T) (*engine, pgxmock.PgxPoolIface, *fakeIdem) {
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
	idem := newFakeIdem()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &engine{db: mock, idem: idem, logger: logger}, mock, idem
}

// Regex shortcuts for the long SQL constants.
var (
	rxSelectForUpdate   = regexp.QuoteMeta("FOR UPDATE")
	rxSelectBalances    = `SELECT gc_balance.*FROM wallets`
	rxUpdateWallet      = `UPDATE wallets`
	rxInsertLedgerTx    = `INSERT INTO ledger_transactions \(`
	rxInsertDedup       = `INSERT INTO ledger_transaction_dedup`
	rxInsertLedgerEntry = `INSERT INTO ledger_entries`
	rxSelectLedgerByOp  = `SELECT id, player_id, transaction_type.*FROM ledger_transactions`
	// Append-only rollback flow: header lookup is by id (no status, no lock)
	// and the double-rollback guard is an EXISTS over the audit trail.
	rxSelectLedgerByID = `SELECT player_id, transaction_type`
	rxRollbackExists   = `SELECT EXISTS`
)

func walletRows(gc, scU, scR string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"gc_balance", "sc_unplayed_balance", "sc_redeemable_balance"}).
		AddRow(decimal.RequireFromString(gc), decimal.RequireFromString(scU), decimal.RequireFromString(scR))
}

// rxSelectPlayerStatus matches the KYC guard query that runs right after the
// wallet FOR UPDATE in every money-moving flow.
var rxSelectPlayerStatus = `SELECT status FROM users WHERE id`

// expectPlayerStatus registers the KYC guard expectation returning the given
// user_status for playerID.
func expectPlayerStatus(mock pgxmock.PgxPoolIface, playerID uuid.UUID, status string) {
	mock.ExpectQuery(rxSelectPlayerStatus).
		WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow(status))
}

// ----------------------------------------------------------------------------
// Happy path: GC bet (single debit, no SC split)
// ----------------------------------------------------------------------------

func TestProcessBet_GC_HappyPath(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-bet-1", playerID, "BET", gameID, roundID, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	// Global idempotency anchor (migration 000005), same tx.
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-bet-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Player wallet debit
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "GC", "DEBIT", dec("10.0000"), dec("90.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// House bet pool credit
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_BET_POOL", "GC", "CREDIT", dec("10.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-bet-1",
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "10.0000"),
		GameID:                gameID,
		RoundID:               roundID,
	})
	if err != nil {
		t.Fatalf("ProcessBet: %v", err)
	}

	// Assert result shape
	if got.Status != StatusProcessed {
		t.Errorf("Status: got %v want %v", got.Status, StatusProcessed)
	}
	if got.LedgerTransactionID != ledgerTxID {
		t.Errorf("LedgerTransactionID: got %v want %v", got.LedgerTransactionID, ledgerTxID)
	}
	if got.PostBalances.GC.String() != "90.0000" {
		t.Errorf("PostBalances.GC: got %s", got.PostBalances.GC)
	}

	// Assert the response was cached
	idemKey := idempotencyKey(operatorCode, "op-bet-1")
	if _, ok := idem.stored[idemKey]; !ok {
		t.Errorf("expected idempotency store to be populated for %s", idemKey)
	}
	if cnt := idem.released[idemKey]; cnt != 0 {
		t.Errorf("Release was called %d times on the happy path (want 0)", cnt)
	}
}

// ----------------------------------------------------------------------------
// Happy path: SC bet that straddles SC_UNPLAYED and SC_REDEEMABLE
// (validates the priority rule end-to-end through the SQL layer)
// ----------------------------------------------------------------------------

func TestProcessBet_SC_SplitsUnplayedThenRedeemable(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "30.0000", "100.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	// Post-state: 0/0/80 — debit 30 from unplayed, 20 from redeemable.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("80.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-bet-2", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-bet-2", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// First debit: SC_UNPLAYED 30, balance_after 0
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_UNPLAYED", "DEBIT", dec("30.0000"), dec("0.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_BET_POOL", "SC_UNPLAYED", "CREDIT", dec("30.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Second debit: SC_REDEEMABLE 20, balance_after 80
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "DEBIT", dec("20.0000"), dec("80.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_BET_POOL", "SC_REDEEMABLE", "CREDIT", dec("20.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-bet-2",
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		Amount:                mustMoney(t, "50.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessBet: %v", err)
	}
	if got.PostBalances.SCUnplayed.String() != "0.0000" {
		t.Errorf("SCUnplayed post: got %s want 0.0000", got.PostBalances.SCUnplayed)
	}
	if got.PostBalances.SCRedeemable.String() != "80.0000" {
		t.Errorf("SCRedeemable post: got %s want 80.0000", got.PostBalances.SCRedeemable)
	}
}

// ----------------------------------------------------------------------------
// Happy path: WIN credits SC_REDEEMABLE even when wallet has SC_UNPLAYED
// ----------------------------------------------------------------------------

func TestProcessWin_SC_AlwaysRoutesToRedeemable(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "5.0000", "10.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	// Win 7 → SC_REDEEMABLE 10 -> 17 (NOT split into unplayed).
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("5.0000"), dec("17.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-win-1", playerID, "WIN", gameID, roundID, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-win-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "CREDIT", dec("7.0000"), dec("17.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_WIN_POOL", "SC_REDEEMABLE", "DEBIT", dec("7.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessWin(context.Background(), WinRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-win-1",
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		Amount:                mustMoney(t, "7.0000"),
		GameID:                gameID,
		RoundID:               roundID,
	})
	if err != nil {
		t.Fatalf("ProcessWin: %v", err)
	}
	if got.PostBalances.SCRedeemable.String() != "17.0000" {
		t.Errorf("SCRedeemable: got %s want 17.0000", got.PostBalances.SCRedeemable)
	}
	if got.PostBalances.SCUnplayed.String() != "5.0000" {
		t.Errorf("SCUnplayed must be untouched on win; got %s", got.PostBalances.SCUnplayed)
	}
}

// ----------------------------------------------------------------------------
// Insufficient funds: rolls back, releases idempotency, surfaces sentinel
// ----------------------------------------------------------------------------

func TestProcessBet_InsufficientFunds_ReleasesLockAndRollsBack(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("5.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	// No UPDATE, no INSERTs — the allocator rejects before any mutating SQL.
	mock.ExpectRollback()

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-fail-1",
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "10.0000"),
	})
	if !errors.Is(err, errs.ErrInsufficientFunds) {
		t.Fatalf("err: got %v want wrapping ErrInsufficientFunds", err)
	}

	idemKey := idempotencyKey(operatorCode, "op-fail-1")
	if cnt := idem.released[idemKey]; cnt != 1 {
		t.Errorf("Release call count: got %d want 1 (must clear PROCESSING on failure)", cnt)
	}
	if _, ok := idem.stored[idemKey]; ok {
		t.Errorf("must NOT cache a failed response")
	}
}

// ----------------------------------------------------------------------------
// Idempotency replay: cached response returned without any DB activity
// ----------------------------------------------------------------------------

func TestProcessBet_CachedReplay_NoDBHit(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	// Pre-populate the idempotency store with a previously-successful response.
	prior := TxResult{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-replay",
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              playerID,
		TransactionType:       "BET",
		Family:                "GC",
		Amount:                mustMoney(t, "5.0000"),
		PostBalances:          BalanceSummary{GC: mustMoney(t, "95.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000")},
		Status:                StatusProcessed,
	}
	b, err := json.Marshal(prior)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	idem.state[idempotencyKey(operatorCode, "op-replay")] = string(b)

	// No mock.Expect* — the DB must not be touched.
	got, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-replay",
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "5.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessBet: %v", err)
	}
	if got.Status != StatusCached {
		t.Errorf("Status: got %v want %v", got.Status, StatusCached)
	}
	if got.LedgerTransactionID != ledgerTxID {
		t.Errorf("LedgerTransactionID: got %v want %v", got.LedgerTransactionID, ledgerTxID)
	}
	if got.PostBalances.GC.String() != "95.0000" {
		t.Errorf("PostBalances.GC: got %s want 95.0000", got.PostBalances.GC)
	}
}

// ----------------------------------------------------------------------------
// Idempotency PROCESSING: returns Pending sentinel, no DB hit, no release
// ----------------------------------------------------------------------------

func TestProcessBet_PendingDuplicate_Returns409Sentinel(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	playerID := uuid.New()
	idem.state[idempotencyKey(operatorCode, "op-dup")] = cache.ProcessingMarker

	// No mock.Expect* — DB must not be touched.
	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-dup",
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "1.0000"),
	})
	if !errors.Is(err, errs.ErrTransactionPending) {
		t.Fatalf("err: got %v want wrapping ErrTransactionPending", err)
	}
	if cnt := idem.released[idempotencyKey(operatorCode, "op-dup")]; cnt != 0 {
		t.Errorf("Release must NOT be called when we never held the lock; got %d", cnt)
	}
}

// ----------------------------------------------------------------------------
// Idempotency Redis-down: FAIL CLOSED — no DB transaction is started
// ----------------------------------------------------------------------------

func TestProcessBet_FailClosed_OnIdempotencyError(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	idem.acquireErr = errors.New("redis: connection refused")

	// No mock.Expect* — DB must not be touched.
	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-rdown",
		PlayerID:              uuid.New(),
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "1.0000"),
	})
	if err == nil {
		t.Fatal("expected error when idempotency layer fails")
	}
}

// ----------------------------------------------------------------------------
// Ghost Spin recovery (architecture §6.A)
// ----------------------------------------------------------------------------

func TestProcessBet_GhostSpinRecovery_OnUniqueViolation(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	committedLedgerID := uuid.New() // the tx id from the first (committed) attempt
	ghostsBefore := testutil.ToFloat64(metrics.GhostSpinsRecovered)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("90.0000", "0.0000", "0.0000")) // post-state of original commit
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("80.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// ledger_transactions insert succeeds (its unique is only partition-local);
	// the GLOBAL dedup insert is what raises 23505 on the duplicate.
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-ghost", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-ghost", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{
			Code:           pgerrcode.UniqueViolation,
			ConstraintName: "ledger_transaction_dedup_pkey",
		})
	// Engine rolls the (now stale) tx back.
	mock.ExpectRollback()
	// Recovery: look up the existing ledger tx + current wallet state.
	mock.ExpectQuery(rxSelectLedgerByOp).
		WithArgs(operatorCode, "op-ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(committedLedgerID, playerID, "BET"))
	mock.ExpectQuery(rxSelectBalances).
		WithArgs(playerID).
		WillReturnRows(walletRows("90.0000", "0.0000", "0.0000"))

	got, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-ghost",
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "10.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessBet: %v", err)
	}
	if got.Status != StatusGhostRecovered {
		t.Errorf("Status: got %v want %v", got.Status, StatusGhostRecovered)
	}
	if got.LedgerTransactionID != committedLedgerID {
		t.Errorf("LedgerTransactionID: got %v want %v (the original committed id)", got.LedgerTransactionID, committedLedgerID)
	}
	if got.PostBalances.GC.String() != "90.0000" {
		t.Errorf("PostBalances.GC must reflect the *current* DB state; got %s", got.PostBalances.GC)
	}

	// The recovered response must be cached so subsequent retries hit the
	// fast Cached path.
	idemKey := idempotencyKey(operatorCode, "op-ghost")
	if _, ok := idem.stored[idemKey]; !ok {
		t.Errorf("ghost recovery must populate the idempotency cache")
	}

	// Successful recovery must be observable. The counter is global and other
	// parallel ghost tests may also increment it, so assert >= +1, not == +1.
	if got := testutil.ToFloat64(metrics.GhostSpinsRecovered); got < ghostsBefore+1 {
		t.Errorf("engine_ghost_spins_recovered_total: got %v want >= %v", got, ghostsBefore+1)
	}
}

// ----------------------------------------------------------------------------
// Ghost Spin: tx id reuse across players is rejected, not silently replayed
// ----------------------------------------------------------------------------

func TestProcessBet_GhostSpin_RejectsTxIDReuseAcrossPlayers(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerA := uuid.New()
	playerB := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerA).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerA, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerA).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-reuse", playerA, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-reuse", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	// Recovery lookup returns a DIFFERENT player — the operator violated the contract.
	mock.ExpectQuery(rxSelectLedgerByOp).
		WithArgs(operatorCode, "op-reuse").
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(uuid.New(), playerB, "BET"))

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-reuse",
		PlayerID:              playerA,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "10.0000"),
	})
	if !errors.Is(err, errs.ErrTransactionConflict) {
		t.Fatalf("got %v, want wrapping ErrTransactionConflict", err)
	}
}

// ----------------------------------------------------------------------------
// Player-not-found: SELECT FOR UPDATE returns ErrNoRows
// ----------------------------------------------------------------------------

func TestProcessBet_PlayerNotFound(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-nopl",
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		Amount:                mustMoney(t, "1.0000"),
	})
	if !errors.Is(err, errs.ErrPlayerNotFound) {
		t.Fatalf("got %v, want wrapping ErrPlayerNotFound", err)
	}
}

// ----------------------------------------------------------------------------
// Request-level validation: rejected before any Redis / DB activity
// ----------------------------------------------------------------------------

func TestProcessBet_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  BetRequest
		err  error
	}{
		{
			name: "empty_operator_code",
			req:  BetRequest{OperatorTransactionID: "x", PlayerID: uuid.New(), Family: domain.FamilyGC, Amount: mustMoney(t, "1.0000")},
			err:  errs.ErrInvalidAmount,
		},
		{
			name: "empty_op_tx_id",
			req:  BetRequest{OperatorCode: "X", PlayerID: uuid.New(), Family: domain.FamilyGC, Amount: mustMoney(t, "1.0000")},
			err:  errs.ErrInvalidAmount,
		},
		{
			name: "nil_player",
			req:  BetRequest{OperatorCode: "X", OperatorTransactionID: "x", Family: domain.FamilyGC, Amount: mustMoney(t, "1.0000")},
			err:  errs.ErrPlayerNotFound,
		},
		{
			name: "bad_family",
			req:  BetRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(), Amount: mustMoney(t, "1.0000")},
			err:  errs.ErrUnsupportedCurrency,
		},
		{
			name: "zero_amount",
			req:  BetRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(), Family: domain.FamilyGC, Amount: domain.ZeroMoney()},
			err:  errs.ErrInvalidAmount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, idem := newEngine(t)
			_, err := e.ProcessBet(context.Background(), tc.req)
			if !errors.Is(err, tc.err) {
				t.Errorf("got %v, want wrapping %v", err, tc.err)
			}
			// Validation must short-circuit BEFORE acquiring the idempotency lock.
			if len(idem.state) != 0 {
				t.Errorf("idempotency state must remain empty on validation failure; got %v", idem.state)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// GetBalances: clean snapshot read, no lock
// ----------------------------------------------------------------------------

func TestGetBalances_Success(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()

	mock.ExpectQuery(rxSelectBalances).
		WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "5.0000", "10.0000"))

	w, err := e.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("GetBalances: %v", err)
	}
	if w.GC.String() != "100.0000" || w.SCUnplayed.String() != "5.0000" || w.SCRedeemable.String() != "10.0000" {
		t.Errorf("balances: got %+v", w)
	}
}

func TestGetBalances_NotFound(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	mock.ExpectQuery(rxSelectBalances).WithArgs(playerID).WillReturnError(pgx.ErrNoRows)
	_, err := e.GetBalances(context.Background(), playerID)
	if !errors.Is(err, errs.ErrPlayerNotFound) {
		t.Fatalf("got %v, want wrapping ErrPlayerNotFound", err)
	}
}

func TestGetBalances_NilPlayer(t *testing.T) {
	t.Parallel()
	e, _, _ := newEngine(t)
	_, err := e.GetBalances(context.Background(), uuid.Nil)
	if !errors.Is(err, errs.ErrPlayerNotFound) {
		t.Errorf("got %v, want wrapping ErrPlayerNotFound", err)
	}
}

// ----------------------------------------------------------------------------
// New: constructor wiring
// ----------------------------------------------------------------------------

func TestNew_NilLoggerFallsBackToDefault(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	idem := newFakeIdem()
	got := New(mock, idem, nil)
	if got == nil {
		t.Fatal("New returned nil")
	}
	// Internal: confirm the engine's logger field is non-nil after fallback.
	if e, ok := got.(*engine); !ok || e.logger == nil {
		t.Errorf("logger fallback did not populate slog.Default()")
	}
}

// ----------------------------------------------------------------------------
// ProcessWin — parallel coverage for the Win flow (validation, idem replay)
// ----------------------------------------------------------------------------

func TestProcessWin_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  WinRequest
		err  error
	}{
		{"empty_op_code",
			WinRequest{OperatorTransactionID: "x", PlayerID: uuid.New(), Family: domain.FamilySC, Amount: mustMoney(t, "1.0000")},
			errs.ErrInvalidAmount},
		{"empty_op_tx",
			WinRequest{OperatorCode: "X", PlayerID: uuid.New(), Family: domain.FamilySC, Amount: mustMoney(t, "1.0000")},
			errs.ErrInvalidAmount},
		{"nil_player",
			WinRequest{OperatorCode: "X", OperatorTransactionID: "x", Family: domain.FamilySC, Amount: mustMoney(t, "1.0000")},
			errs.ErrPlayerNotFound},
		{"bad_family",
			WinRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(), Amount: mustMoney(t, "1.0000")},
			errs.ErrUnsupportedCurrency},
		{"zero_amount",
			WinRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(), Family: domain.FamilyGC, Amount: domain.ZeroMoney()},
			errs.ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t)
			_, err := e.ProcessWin(context.Background(), tc.req)
			if !errors.Is(err, tc.err) {
				t.Errorf("got %v, want wrapping %v", err, tc.err)
			}
		})
	}
}

func TestProcessWin_CachedReplay(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()
	prior := TxResult{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-win-replay",
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              playerID,
		TransactionType:       "WIN",
		Family:                "SC",
		Amount:                mustMoney(t, "3.0000"),
		PostBalances:          BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "3.0000")},
		Status:                StatusProcessed,
	}
	b, _ := json.Marshal(prior)
	idem.state[idempotencyKey(operatorCode, "op-win-replay")] = string(b)

	got, err := e.ProcessWin(context.Background(), WinRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-win-replay",
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "3.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessWin: %v", err)
	}
	if got.Status != StatusCached {
		t.Errorf("Status: got %v want %v", got.Status, StatusCached)
	}
}

func TestProcessWin_PendingDuplicate(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	idem.state[idempotencyKey(operatorCode, "op-win-dup")] = cache.ProcessingMarker
	_, err := e.ProcessWin(context.Background(), WinRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-win-dup",
		PlayerID: uuid.New(), Family: domain.FamilySC, Amount: mustMoney(t, "1.0000"),
	})
	if !errors.Is(err, errs.ErrTransactionPending) {
		t.Fatalf("got %v, want wrapping ErrTransactionPending", err)
	}
}

func TestProcessWin_FailClosed_OnIdempotencyError(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	idem.acquireErr = errors.New("redis: down")
	_, err := e.ProcessWin(context.Background(), WinRequest{
		OperatorCode: "X", OperatorTransactionID: "x",
		PlayerID: uuid.New(), Family: domain.FamilySC, Amount: mustMoney(t, "1.0000"),
	})
	if err == nil {
		t.Fatal("expected error when idempotency layer fails")
	}
}

func TestProcessWin_GhostSpinRecovery(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	committedID := uuid.New()
	referenceID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "10.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("15.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-win-ghost", playerID, "WIN", nil, nil, referenceID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-win-ghost", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	mock.ExpectQuery(rxSelectLedgerByOp).
		WithArgs(operatorCode, "op-win-ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(committedID, playerID, "WIN"))
	mock.ExpectQuery(rxSelectBalances).
		WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "15.0000"))

	got, err := e.ProcessWin(context.Background(), WinRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-win-ghost",
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "5.0000"),
		ReferenceTransactionID: referenceID,
	})
	if err != nil {
		t.Fatalf("ProcessWin: %v", err)
	}
	if got.Status != StatusGhostRecovered {
		t.Errorf("Status: got %v want %v", got.Status, StatusGhostRecovered)
	}
	if got.LedgerTransactionID != committedID {
		t.Errorf("LedgerTransactionID: got %v want %v", got.LedgerTransactionID, committedID)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-win-ghost")]; !ok {
		t.Error("ghost recovery must cache the response")
	}
}

// ----------------------------------------------------------------------------
// SQL error injection: BEGIN, COMMIT, UPDATE failure paths
// ----------------------------------------------------------------------------

func TestProcessBet_BeginFails_ReleasesIdempotency(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted}).
		WillReturnError(errors.New("pg: pool exhausted"))
	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-bf",
		PlayerID: uuid.New(), Family: domain.FamilyGC, Amount: mustMoney(t, "1.0000"),
	})
	if err == nil {
		t.Fatal("expected error on BEGIN failure")
	}
	if cnt := idem.released[idempotencyKey(operatorCode, "op-bf")]; cnt != 1 {
		t.Errorf("Release count: got %d want 1", cnt)
	}
}

func TestProcessBet_CommitFails(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-cf", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-cf", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "GC", "DEBIT", dec("10.0000"), dec("90.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_BET_POOL", "GC", "CREDIT", dec("10.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-cf",
		PlayerID: playerID, Family: domain.FamilyGC, Amount: mustMoney(t, "10.0000"),
	})
	if err == nil {
		t.Fatal("expected error on COMMIT failure")
	}
	if cnt := idem.released[idempotencyKey(operatorCode, "op-cf")]; cnt != 1 {
		t.Errorf("Release count: got %d want 1", cnt)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-cf")]; ok {
		t.Error("must NOT cache a failed commit")
	}
}

func TestProcessBet_UpdateZeroRowsAffected_FailsClosed(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	// UPDATE returns 0 rows: schema corruption / bad routing — engine must reject.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-zr",
		PlayerID: playerID, Family: domain.FamilyGC, Amount: mustMoney(t, "10.0000"),
	})
	if err == nil {
		t.Fatal("expected error when UPDATE affected 0 rows")
	}
}

func TestProcessBet_GhostSpin_LedgerLookupMisses(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-gone", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-gone", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	// The recovery lookup returns nothing — should map to ErrTransactionConflict.
	mock.ExpectQuery(rxSelectLedgerByOp).
		WithArgs(operatorCode, "op-gone").
		WillReturnError(pgx.ErrNoRows)

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-gone",
		PlayerID: playerID, Family: domain.FamilyGC, Amount: mustMoney(t, "10.0000"),
	})
	if !errors.Is(err, errs.ErrTransactionConflict) {
		t.Fatalf("got %v, want wrapping ErrTransactionConflict", err)
	}
}

func TestDecodeCached_BadPayload(t *testing.T) {
	t.Parallel()
	_, err := decodeCached("not-json", StatusCached)
	if err == nil {
		t.Fatal("expected error decoding bad JSON")
	}
}

// ----------------------------------------------------------------------------
// ProcessRollback
// ----------------------------------------------------------------------------

func TestProcessRollback_HappyPath_RestoresFunds(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	originalTxID := uuid.New()
	rollbackTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	// Wallet currently shows post-bet balance (0/0/70 after a 30 SC bet).
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "70.0000"))
	// Read original tx header (append-only: no FOR UPDATE, no status column).
	mock.ExpectQuery(rxSelectLedgerByID).
		WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"player_id", "transaction_type"}).
			AddRow(playerID, "BET"))
	// APPEND-ONLY double-rollback guard: no prior ROLLBACK references this BET.
	mock.ExpectQuery(rxRollbackExists).
		WithArgs(originalTxID, operatorCode, "op-rb-1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	// Fetch original player-wallet entries — single SC_REDEEMABLE debit of 30.
	mock.ExpectQuery(`SELECT currency, direction, amount.*FROM ledger_entries`).
		WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"currency", "direction", "amount"}).
			AddRow("SC_REDEEMABLE", "DEBIT", decimal.RequireFromString("30.0000")))
	// UPDATE wallets: SC_REDEEMABLE restored to 100 (wallets is the cache, not the ledger).
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("100.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// NOTE: no UPDATE of ledger_transactions — the ledger is strictly append-only.
	// INSERT rollback header.
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-rb-1", playerID, "ROLLBACK", nil, nil, originalTxID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(rollbackTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-rb-1", rollbackTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// INSERT reverse entries: CREDIT to player, DEBIT to house bet pool.
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(rollbackTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "CREDIT", dec("30.0000"), dec("100.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(rollbackTxID, nil, "HOUSE_BET_POOL", "SC_REDEEMABLE", "DEBIT", dec("30.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessRollback(context.Background(), RollbackRequest{
		OperatorCode:           operatorCode,
		OperatorTransactionID:  "op-rb-1",
		PlayerID:               playerID,
		ReferenceTransactionID: originalTxID,
	})
	if err != nil {
		t.Fatalf("ProcessRollback: %v", err)
	}
	if got.Status != StatusProcessed {
		t.Errorf("Status: got %v want %v", got.Status, StatusProcessed)
	}
	if got.TransactionType != "ROLLBACK" {
		t.Errorf("Type: got %s want ROLLBACK", got.TransactionType)
	}
	if got.PostBalances.SCRedeemable.String() != "100.0000" {
		t.Errorf("PostBalances.SCRedeemable: got %s want 100.0000", got.PostBalances.SCRedeemable)
	}
	if got.Amount.String() != "30.0000" {
		t.Errorf("Amount: got %s want 30.0000", got.Amount)
	}
}

func TestProcessRollback_OriginalNotFound(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	originalTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	mock.ExpectQuery(rxSelectLedgerByID).
		WithArgs(originalTxID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := e.ProcessRollback(context.Background(), RollbackRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-rb-2",
		PlayerID: playerID, ReferenceTransactionID: originalTxID,
	})
	if !errors.Is(err, errs.ErrRollbackNotFound) {
		t.Fatalf("got %v, want wrapping ErrRollbackNotFound", err)
	}
}

func TestProcessRollback_AlreadyRolledBack(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	originalTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	// Original is a valid BET...
	mock.ExpectQuery(rxSelectLedgerByID).
		WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"player_id", "transaction_type"}).
			AddRow(playerID, "BET"))
	// ...but a ROLLBACK already references it (detected via the append-only
	// audit trail, NOT a mutated status flag).
	mock.ExpectQuery(rxRollbackExists).
		WithArgs(originalTxID, operatorCode, "op-rb-3").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	_, err := e.ProcessRollback(context.Background(), RollbackRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-rb-3",
		PlayerID: playerID, ReferenceTransactionID: originalTxID,
	})
	if !errors.Is(err, errs.ErrRollbackAlready) {
		t.Fatalf("got %v, want wrapping ErrRollbackAlready", err)
	}
}

func TestProcessRollback_WinIsUnsupported(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	originalTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	// Original is a WIN — rejected before the double-rollback guard runs.
	mock.ExpectQuery(rxSelectLedgerByID).
		WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"player_id", "transaction_type"}).
			AddRow(playerID, "WIN"))
	mock.ExpectRollback()

	_, err := e.ProcessRollback(context.Background(), RollbackRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-rb-4",
		PlayerID: playerID, ReferenceTransactionID: originalTxID,
	})
	if !errors.Is(err, errs.ErrRollbackUnsupported) {
		t.Fatalf("got %v, want wrapping ErrRollbackUnsupported", err)
	}
}

func TestProcessRollback_PlayerMismatch(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerA := uuid.New()
	playerB := uuid.New()
	originalTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerA).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	// Original belongs to a different player — rejected before the guard runs.
	mock.ExpectQuery(rxSelectLedgerByID).
		WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"player_id", "transaction_type"}).
			AddRow(playerB, "BET"))
	mock.ExpectRollback()

	_, err := e.ProcessRollback(context.Background(), RollbackRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-rb-5",
		PlayerID: playerA, ReferenceTransactionID: originalTxID,
	})
	if !errors.Is(err, errs.ErrTransactionConflict) {
		t.Fatalf("got %v, want wrapping ErrTransactionConflict", err)
	}
}

// A retry of the SAME rollback (same operator_transaction_id) after the Redis
// cache was lost must NOT be misreported as ErrRollbackAlready. The append-only
// guard excludes our own anchor, the flow reaches the dedup INSERT, hits 23505,
// and Ghost-Spin recovery replays the original success — true idempotency.
func TestProcessRollback_SameOpTxRetry_GhostRecovers(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	originalTxID := uuid.New()
	committedRollbackID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "70.0000"))
	mock.ExpectQuery(rxSelectLedgerByID).WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"player_id", "transaction_type"}).
			AddRow(playerID, "BET"))
	// Guard EXCLUDES our own (op-rb-retry) anchor → no "other" rollback exists.
	mock.ExpectQuery(rxRollbackExists).
		WithArgs(originalTxID, operatorCode, "op-rb-retry").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT currency, direction, amount.*FROM ledger_entries`).
		WithArgs(originalTxID).
		WillReturnRows(pgxmock.NewRows([]string{"currency", "direction", "amount"}).
			AddRow("SC_REDEEMABLE", "DEBIT", decimal.RequireFromString("30.0000")))
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("100.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-rb-retry", playerID, "ROLLBACK", nil, nil, originalTxID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	// The GLOBAL dedup anchor is what collides on the duplicate rollback.
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-rb-retry", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "ledger_transaction_dedup_pkey"})
	mock.ExpectRollback()
	// Ghost recovery: find the committed rollback + read current balances.
	mock.ExpectQuery(rxSelectLedgerByOp).WithArgs(operatorCode, "op-rb-retry").
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(committedRollbackID, playerID, "ROLLBACK"))
	mock.ExpectQuery(rxSelectBalances).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "100.0000"))

	got, err := e.ProcessRollback(context.Background(), RollbackRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-rb-retry",
		PlayerID: playerID, ReferenceTransactionID: originalTxID,
	})
	if err != nil {
		t.Fatalf("ProcessRollback: %v", err)
	}
	if got.Status != StatusGhostRecovered {
		t.Errorf("Status: got %v want %v", got.Status, StatusGhostRecovered)
	}
	if got.LedgerTransactionID != committedRollbackID {
		t.Errorf("LedgerTransactionID: got %v want %v", got.LedgerTransactionID, committedRollbackID)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-rb-retry")]; !ok {
		t.Error("ghost recovery must cache the response")
	}
}

func TestProcessRollback_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  RollbackRequest
		err  error
	}{
		{"no_op_code", RollbackRequest{OperatorTransactionID: "x", PlayerID: uuid.New(), ReferenceTransactionID: uuid.New()}, errs.ErrInvalidAmount},
		{"no_op_tx_id", RollbackRequest{OperatorCode: "X", PlayerID: uuid.New(), ReferenceTransactionID: uuid.New()}, errs.ErrInvalidAmount},
		{"nil_player", RollbackRequest{OperatorCode: "X", OperatorTransactionID: "x", ReferenceTransactionID: uuid.New()}, errs.ErrPlayerNotFound},
		{"nil_reference", RollbackRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New()}, errs.ErrRollbackNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t)
			_, err := e.ProcessRollback(context.Background(), tc.req)
			if !errors.Is(err, tc.err) {
				t.Errorf("got %v, want wrapping %v", err, tc.err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// KYC / player status guard: only ACTIVE players may move money. The check
// runs on the SAME tx handle, after the wallet FOR UPDATE, in every flow.
// ----------------------------------------------------------------------------

// TestProcessBet_PlayerStatusGuard drives the full status table through the
// bet flow: ACTIVE proceeds to commit; every other lifecycle state aborts
// before any mutating SQL with ErrPlayerNotActive.
func TestProcessBet_PlayerStatusGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status  string
		blocked bool
	}{
		{"ACTIVE", false},
		{"KYC_PENDING", true},
		{"SUSPENDED", true},
		{"CLOSED", true},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			e, mock, idem := newEngine(t)
			playerID := uuid.New()
			ledgerTxID := uuid.New()
			opTxID := "op-status-" + tc.status

			mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			mock.ExpectQuery(rxSelectForUpdate).
				WithArgs(playerID).
				WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
			expectPlayerStatus(mock, playerID, tc.status)
			if tc.blocked {
				// Guard aborts BEFORE any UPDATE/INSERT — only the rollback follows.
				mock.ExpectRollback()
			} else {
				mock.ExpectExec(rxUpdateWallet).
					WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
				mock.ExpectQuery(rxInsertLedgerTx).
					WithArgs(operatorCode, opTxID, playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
					WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
				mock.ExpectExec(rxInsertDedup).
					WithArgs(operatorCode, opTxID, ledgerTxID).
					WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectExec(rxInsertLedgerEntry).
					WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "GC", "DEBIT", dec("10.0000"), dec("90.0000")).
					WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectExec(rxInsertLedgerEntry).
					WithArgs(ledgerTxID, nil, "HOUSE_BET_POOL", "GC", "CREDIT", dec("10.0000"), nil).
					WillReturnResult(pgxmock.NewResult("INSERT", 1))
				mock.ExpectCommit()
			}

			_, err := e.ProcessBet(context.Background(), BetRequest{
				OperatorCode:          operatorCode,
				OperatorTransactionID: opTxID,
				PlayerID:              playerID,
				Family:                domain.FamilyGC,
				Amount:                mustMoney(t, "10.0000"),
			})
			if tc.blocked {
				if !errors.Is(err, errs.ErrPlayerNotActive) {
					t.Fatalf("err: got %v want wrapping ErrPlayerNotActive", err)
				}
				idemKey := idempotencyKey(operatorCode, opTxID)
				if cnt := idem.released[idemKey]; cnt != 1 {
					t.Errorf("Release count: got %d want 1 (PROCESSING must be cleared)", cnt)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProcessBet (ACTIVE): %v", err)
			}
		})
	}
}

// TestPlayerStatusGuard_AllMoneyFlowsBlocked proves the guard is wired into
// every money-moving flow (win, purchase, redeem — bet covered above): a
// SUSPENDED player is rejected right after the wallet lock with no mutation.
func TestPlayerStatusGuard_AllMoneyFlowsBlocked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		call func(e Engine, ce CasinoEngine, playerID uuid.UUID) error
	}{
		{"win", func(e Engine, _ CasinoEngine, playerID uuid.UUID) error {
			_, err := e.ProcessWin(context.Background(), WinRequest{
				OperatorCode: operatorCode, OperatorTransactionID: "op-st-win",
				PlayerID: playerID, Family: domain.FamilySC,
				Amount: decimalMoney(t, "5.0000"),
			})
			return err
		}},
		{"purchase", func(_ Engine, ce CasinoEngine, playerID uuid.UUID) error {
			_, err := ce.ProcessPurchase(context.Background(), PurchaseRequest{
				OperatorCode: operatorCode, OperatorTransactionID: "op-st-pur",
				PlayerID: playerID, GCAmount: decimalMoney(t, "100.0000"),
			})
			return err
		}},
		{"redeem", func(_ Engine, ce CasinoEngine, playerID uuid.UUID) error {
			_, err := ce.ProcessRedeem(context.Background(), RedeemRequest{
				OperatorCode: operatorCode, OperatorTransactionID: "op-st-red",
				PlayerID: playerID, Amount: decimalMoney(t, "5.0000"),
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, mock, _ := newEngine(t)
			playerID := uuid.New()

			mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			mock.ExpectQuery(rxSelectForUpdate).
				WithArgs(playerID).
				WillReturnRows(walletRows("100.0000", "100.0000", "100.0000"))
			expectPlayerStatus(mock, playerID, "SUSPENDED")
			mock.ExpectRollback()

			// The same concrete *engine implements both interfaces.
			err := tc.call(e, e, playerID)
			if !errors.Is(err, errs.ErrPlayerNotActive) {
				t.Fatalf("err: got %v want wrapping ErrPlayerNotActive", err)
			}
		})
	}
}

// decimalMoney is a tiny alias of mustMoney for the status-guard tables.
func decimalMoney(t *testing.T, s string) domain.Money { return mustMoney(t, s) }
