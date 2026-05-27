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
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
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
}

func newFakeIdem() *fakeIdem {
	return &fakeIdem{
		state:    map[string]string{},
		stored:   map[string]string{},
		released: map[string]int{},
	}
}

func (f *fakeIdem) Acquire(_ context.Context, key string) (cache.AcquireStatus, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return cache.StatusUnknown, "", f.acquireErr
	}
	if v, ok := f.state[key]; ok {
		if v == cache.ProcessingMarker {
			return cache.StatusPending, "", nil
		}
		return cache.StatusCached, v, nil
	}
	f.state[key] = cache.ProcessingMarker
	return cache.StatusAcquired, "", nil
}

func (f *fakeIdem) Store(_ context.Context, key, payload string) error {
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
	rxSelectForUpdate     = regexp.QuoteMeta("FOR UPDATE")
	rxSelectBalances      = `SELECT gc_balance.*FROM wallets`
	rxUpdateWallet        = `UPDATE wallets`
	rxInsertLedgerTx      = `INSERT INTO ledger_transactions`
	rxInsertLedgerEntry   = `INSERT INTO ledger_entries`
	rxSelectLedgerByOp    = `SELECT id, player_id, transaction_type.*FROM ledger_transactions`
)

func walletRows(gc, scU, scR string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"gc_balance", "sc_unplayed_balance", "sc_redeemable_balance"}).
		AddRow(decimal.RequireFromString(gc), decimal.RequireFromString(scU), decimal.RequireFromString(scR))
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
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-bet-1", playerID, "BET", gameID, roundID, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
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
	// Post-state: 0/0/80 — debit 30 from unplayed, 20 from redeemable.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("80.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-bet-2", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))

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
	// Win 7 → SC_REDEEMABLE 10 -> 17 (NOT split into unplayed).
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("5.0000"), dec("17.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-win-1", playerID, "WIN", gameID, roundID, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
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

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).
		WithArgs(playerID).
		WillReturnRows(walletRows("90.0000", "0.0000", "0.0000")) // post-state of original commit
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("80.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// The INSERT into ledger_transactions hits the operator UNIQUE index.
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-ghost", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnError(&pgconn.PgError{
			Code:           pgerrcode.UniqueViolation,
			ConstraintName: "ledger_tx_operator_unique",
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
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerA).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-reuse", playerA, "BET", nil, nil, nil, json.RawMessage("{}")).
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
	if err != nil { t.Fatalf("pgxmock: %v", err) }
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
	if err != nil { t.Fatalf("ProcessWin: %v", err) }
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
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("15.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-win-ghost", playerID, "WIN", nil, nil, referenceID, json.RawMessage("{}")).
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
	if err != nil { t.Fatalf("ProcessWin: %v", err) }
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
	if err == nil { t.Fatal("expected error on BEGIN failure") }
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
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-cf", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
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
	if err == nil { t.Fatal("expected error on COMMIT failure") }
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
	// UPDATE returns 0 rows: schema corruption / bad routing — engine must reject.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	_, err := e.ProcessBet(context.Background(), BetRequest{
		OperatorCode: operatorCode, OperatorTransactionID: "op-zr",
		PlayerID: playerID, Family: domain.FamilyGC, Amount: mustMoney(t, "10.0000"),
	})
	if err == nil { t.Fatal("expected error when UPDATE affected 0 rows") }
}

func TestProcessBet_GhostSpin_LedgerLookupMisses(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("90.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-gone", playerID, "BET", nil, nil, nil, json.RawMessage("{}")).
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
	if err == nil { t.Fatal("expected error decoding bad JSON") }
}
