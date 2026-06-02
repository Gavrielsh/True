package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/shopspring/decimal"

	errs "github.com/Gavrielsh/TransactionMechanism/pkg/errors"
)

// Escrow-specific SQL matchers (engine_test.go owns the shared ones).
var (
	rxLedgerForUpdate = `SELECT player_id, transaction_type, status.*FOR UPDATE`
	rxEntriesByTx     = `SELECT currency, direction, amount.*FROM ledger_entries`
	rxMarkCommitted   = `UPDATE ledger_transactions.*SET status = 'COMPLETED'`
	rxMarkReleased    = `UPDATE ledger_transactions.*SET status = 'ROLLED_BACK'`
)

// reserveLockRows returns the (player_id, transaction_type, status) row that
// lockEscrowReserve reads under FOR UPDATE.
func reserveLockRows(playerID uuid.UUID, txType, status string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"player_id", "transaction_type", "status"}).
		AddRow(playerID, txType, status)
}

// reserveEntryRows returns the single PLAYER_WALLET debit row the reserve wrote.
func reserveEntryRows(amount string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"currency", "direction", "amount"}).
		AddRow("SC_REDEEMABLE", "DEBIT", decimal.RequireFromString(amount))
}

// ----------------------------------------------------------------------------
// Reserve — DEBIT player SC_REDEEMABLE / CREDIT HOUSE_ESCROW_POOL, PENDING.
// ----------------------------------------------------------------------------

func TestProcessEscrowReserve_HappyPath(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	escrowTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "20.0000", "100.0000"))
	// Reserve 30 → SC_REDEEMABLE 100 → 70 (SC_UNPLAYED untouched).
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("20.0000"), dec("70.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-resv-1", playerID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(escrowTxID))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(escrowTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "DEBIT", dec("30.0000"), dec("70.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(escrowTxID, nil, "HOUSE_ESCROW_POOL", "SC_REDEEMABLE", "CREDIT", dec("30.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessEscrowReserve(context.Background(), EscrowReserveRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-resv-1",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "30.0000"),
		RequestHash:           "h-resv",
	})
	if err != nil {
		t.Fatalf("ProcessEscrowReserve: %v", err)
	}
	if got.TransactionType != "ESCROW_RESERVE" {
		t.Errorf("type: got %s want ESCROW_RESERVE", got.TransactionType)
	}
	if got.LedgerTransactionID != escrowTxID {
		t.Errorf("escrow id: got %v want %v", got.LedgerTransactionID, escrowTxID)
	}
	if got.PostBalances.SCRedeemable.String() != "70.0000" {
		t.Errorf("post SC_REDEEMABLE: got %s want 70.0000", got.PostBalances.SCRedeemable)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-resv-1")]; !ok {
		t.Error("reserve must cache its response")
	}
}

func TestProcessEscrowReserve_InsufficientRedeemable(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	// Plenty of SC_UNPLAYED, but only 5 SC_REDEEMABLE — reserve of 10 must fail.
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "500.0000", "5.0000"))
	mock.ExpectRollback()

	_, err := e.ProcessEscrowReserve(context.Background(), EscrowReserveRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-resv-2",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "10.0000"),
	})
	if !errors.Is(err, errs.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
	if cnt := idem.released[idempotencyKey(operatorCode, "op-resv-2")]; cnt != 1 {
		t.Errorf("Release count: got %d want 1", cnt)
	}
}

// ----------------------------------------------------------------------------
// Commit — burns escrow: HOUSE_ESCROW_POOL DEBIT / HOUSE_WITHDRAWAL_POOL CREDIT.
// ----------------------------------------------------------------------------

func TestProcessEscrowCommit_HappyPath_Burns(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	escrowTxID := uuid.New()
	commitTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "70.0000")) // post-reserve
	mock.ExpectQuery(rxLedgerForUpdate).WithArgs(escrowTxID).
		WillReturnRows(reserveLockRows(playerID, "ESCROW_RESERVE", "PENDING"))
	mock.ExpectQuery(rxEntriesByTx).WithArgs(escrowTxID).
		WillReturnRows(reserveEntryRows("30.0000"))
	mock.ExpectExec(rxMarkCommitted).WithArgs(escrowTxID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-commit-1", playerID, "ESCROW_COMMIT", nil, nil, escrowTxID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(commitTxID))
	// Burn: escrow pool debited, withdrawal pool credited; no player entry.
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(commitTxID, nil, "HOUSE_ESCROW_POOL", "SC_REDEEMABLE", "DEBIT", dec("30.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(commitTxID, nil, "HOUSE_WITHDRAWAL_POOL", "SC_REDEEMABLE", "CREDIT", dec("30.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessEscrowCommit(context.Background(), EscrowCommitRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-commit-1",
		PlayerID:              playerID,
		EscrowTransactionID:   escrowTxID,
	})
	if err != nil {
		t.Fatalf("ProcessEscrowCommit: %v", err)
	}
	if got.TransactionType != "ESCROW_COMMIT" {
		t.Errorf("type: got %s want ESCROW_COMMIT", got.TransactionType)
	}
	if got.Amount.String() != "30.0000" {
		t.Errorf("amount: got %s want 30.0000", got.Amount)
	}
	// A commit does not touch the wallet.
	if got.PostBalances.SCRedeemable.String() != "70.0000" {
		t.Errorf("post SC_REDEEMABLE: got %s want 70.0000 (commit must not move the wallet)", got.PostBalances.SCRedeemable)
	}
}

// ----------------------------------------------------------------------------
// Release — returns escrow to the player: player CREDIT / escrow pool DEBIT.
// ----------------------------------------------------------------------------

func TestProcessEscrowRelease_HappyPath_Refunds(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	escrowTxID := uuid.New()
	releaseTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "70.0000")) // post-reserve
	mock.ExpectQuery(rxLedgerForUpdate).WithArgs(escrowTxID).
		WillReturnRows(reserveLockRows(playerID, "ESCROW_RESERVE", "PENDING"))
	mock.ExpectQuery(rxEntriesByTx).WithArgs(escrowTxID).
		WillReturnRows(reserveEntryRows("30.0000"))
	// Funds returned: SC_REDEEMABLE 70 → 100.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("100.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(rxMarkReleased).WithArgs(escrowTxID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-rel-1", playerID, "ESCROW_RELEASE", nil, nil, escrowTxID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(releaseTxID))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(releaseTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "CREDIT", dec("30.0000"), dec("100.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(releaseTxID, nil, "HOUSE_ESCROW_POOL", "SC_REDEEMABLE", "DEBIT", dec("30.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessEscrowRelease(context.Background(), EscrowReleaseRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-rel-1",
		PlayerID:              playerID,
		EscrowTransactionID:   escrowTxID,
	})
	if err != nil {
		t.Fatalf("ProcessEscrowRelease: %v", err)
	}
	if got.PostBalances.SCRedeemable.String() != "100.0000" {
		t.Errorf("post SC_REDEEMABLE: got %s want 100.0000", got.PostBalances.SCRedeemable)
	}
}

// ----------------------------------------------------------------------------
// Double-spend guard: an already-released reserve cannot be committed.
// ----------------------------------------------------------------------------

func TestProcessEscrowCommit_AfterRelease_Conflicts(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	escrowTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "100.0000"))
	// The reserve was already released → status ROLLED_BACK.
	mock.ExpectQuery(rxLedgerForUpdate).WithArgs(escrowTxID).
		WillReturnRows(reserveLockRows(playerID, "ESCROW_RESERVE", "ROLLED_BACK"))
	mock.ExpectRollback()

	_, err := e.ProcessEscrowCommit(context.Background(), EscrowCommitRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-commit-dbl",
		PlayerID:              playerID,
		EscrowTransactionID:   escrowTxID,
	})
	if !errors.Is(err, errs.ErrEscrowConflict) {
		t.Fatalf("got %v, want ErrEscrowConflict (cannot commit a released reserve)", err)
	}
}

func TestProcessEscrowCommit_NotFound(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	escrowTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	mock.ExpectQuery(rxLedgerForUpdate).WithArgs(escrowTxID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := e.ProcessEscrowCommit(context.Background(), EscrowCommitRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-commit-nf",
		PlayerID:              playerID,
		EscrowTransactionID:   escrowTxID,
	})
	if !errors.Is(err, errs.ErrEscrowNotFound) {
		t.Fatalf("got %v, want ErrEscrowNotFound", err)
	}
}

func TestProcessEscrowCommit_PlayerMismatch(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerA := uuid.New()
	playerB := uuid.New()
	escrowTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerA).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	mock.ExpectQuery(rxLedgerForUpdate).WithArgs(escrowTxID).
		WillReturnRows(reserveLockRows(playerB, "ESCROW_RESERVE", "PENDING"))
	mock.ExpectRollback()

	_, err := e.ProcessEscrowCommit(context.Background(), EscrowCommitRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-commit-mismatch",
		PlayerID:              playerA,
		EscrowTransactionID:   escrowTxID,
	})
	if !errors.Is(err, errs.ErrTransactionConflict) {
		t.Fatalf("got %v, want ErrTransactionConflict", err)
	}
}

func TestProcessEscrow_Validation(t *testing.T) {
	t.Parallel()
	e, _, _ := newEngine(t)
	// Commit with a nil escrow id is rejected before any Redis/DB activity.
	_, err := e.ProcessEscrowCommit(context.Background(), EscrowCommitRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "x",
		PlayerID:              uuid.New(),
		EscrowTransactionID:   uuid.Nil,
	})
	if !errors.Is(err, errs.ErrEscrowNotFound) {
		t.Fatalf("got %v, want ErrEscrowNotFound for nil escrow id", err)
	}
}

// ----------------------------------------------------------------------------
// Cryptographic idempotency: reused id + changed body → mismatch, no DB hit.
// ----------------------------------------------------------------------------

func TestProcessEscrowReserve_IdempotencyMismatch(t *testing.T) {
	t.Parallel()
	e, _, idem := newEngine(t)
	playerID := uuid.New()
	key := idempotencyKey(operatorCode, "op-resv-replay")

	// The original attempt (hash "h-original") is already cached.
	prior := TxResult{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-resv-replay",
		PlayerID:              playerID,
		TransactionType:       "ESCROW_RESERVE",
		Amount:                mustMoney(t, "30.0000"),
		Status:                StatusProcessed,
	}
	b, _ := json.Marshal(prior)
	idem.state[key] = string(b)
	idem.hashes[key] = "h-original"

	// A retry reusing the id but with a DIFFERENT body (amount 999) must NOT
	// replay the cached success — no DB expectations are set, so any DB touch
	// would fail the test.
	_, err := e.ProcessEscrowReserve(context.Background(), EscrowReserveRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-resv-replay",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "999.0000"),
		RequestHash:           "h-tampered",
	})
	if !errors.Is(err, errs.ErrIdempotencyMismatch) {
		t.Fatalf("got %v, want ErrIdempotencyMismatch", err)
	}
}

// Sanity: domain reserve math is wired through (no float drift on .0001 edges).
func TestProcessEscrowReserve_PrecisionEdge(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	escrowTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "10.0001"))
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("0.0001"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-resv-prec", playerID, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(escrowTxID))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(escrowTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "DEBIT", dec("10.0000"), dec("0.0001")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(escrowTxID, nil, "HOUSE_ESCROW_POOL", "SC_REDEEMABLE", "CREDIT", dec("10.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessEscrowReserve(context.Background(), EscrowReserveRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-resv-prec",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "10.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessEscrowReserve: %v", err)
	}
	if got.PostBalances.SCRedeemable.String() != "0.0001" {
		t.Errorf("post SC_REDEEMABLE: got %s want 0.0001", got.PostBalances.SCRedeemable)
	}
}
