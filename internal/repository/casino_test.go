package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Regexes specific to the casino flows (the shared ledger/wallet regexes live
// in engine_test.go, same package).
const (
	rxSelectUserByExternal = `SELECT id FROM users WHERE external_id`
	rxInsertUser           = `INSERT INTO users`
	rxInsertWallet         = `INSERT INTO wallets`
)

// ----------------------------------------------------------------------------
// CreatePlayer
// ----------------------------------------------------------------------------

func TestCreatePlayer_NewPlayer(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	// Not found by external_id → insert.
	mock.ExpectQuery(rxSelectUserByExternal).WithArgs("ext-1").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxInsertUser).
		WithArgs("ext-1", "ext-1", nil, "US", "ACTIVE"). // username→external_id, email NULL, defaults applied
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(playerID))
	mock.ExpectExec(rxInsertWallet).WithArgs(playerID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery(rxSelectBalances).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	mock.ExpectCommit()

	got, err := e.CreatePlayer(context.Background(), CreatePlayerRequest{ExternalID: "ext-1"})
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}
	if !got.Created {
		t.Error("Created: got false want true")
	}
	if got.PlayerID != playerID {
		t.Errorf("PlayerID: got %v want %v", got.PlayerID, playerID)
	}
	if got.Balances.GC.String() != "0.0000" {
		t.Errorf("GC: got %s want 0.0000", got.Balances.GC)
	}
}

func TestCreatePlayer_Idempotent_AlreadyExists(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	// Found by external_id → no insert; ensure wallet, read balances.
	mock.ExpectQuery(rxSelectUserByExternal).WithArgs("ext-dup").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(playerID))
	mock.ExpectExec(rxInsertWallet).WithArgs(playerID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0)) // wallet already there
	mock.ExpectQuery(rxSelectBalances).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "5.0000", "10.0000"))
	mock.ExpectCommit()

	got, err := e.CreatePlayer(context.Background(), CreatePlayerRequest{ExternalID: "ext-dup"})
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}
	if got.Created {
		t.Error("Created: got true want false (idempotent replay)")
	}
	if got.Balances.SCRedeemable.String() != "10.0000" {
		t.Errorf("SCRedeemable: got %s want 10.0000", got.Balances.SCRedeemable)
	}
}

func TestCreatePlayer_UniqueViolation_OnInsert(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectUserByExternal).WithArgs("ext-race").WillReturnError(pgx.ErrNoRows)
	// Username/email collision (or a racing create) → unique violation.
	mock.ExpectQuery(rxInsertUser).
		WithArgs("ext-race", "taken", nil, "US", "ACTIVE").
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()

	_, err := e.CreatePlayer(context.Background(), CreatePlayerRequest{ExternalID: "ext-race", Username: "taken"})
	if !errors.Is(err, errs.ErrTransactionConflict) {
		t.Fatalf("got %v, want wrapping ErrTransactionConflict", err)
	}
}

func TestCreatePlayer_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  CreatePlayerRequest
	}{
		{"empty_external_id", CreatePlayerRequest{ExternalID: ""}},
		{"bad_country_code", CreatePlayerRequest{ExternalID: "x", CountryCode: "USA"}},
		{"bad_status", CreatePlayerRequest{ExternalID: "x", Status: "VIP"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t) // no DB expectations: must short-circuit
			_, err := e.CreatePlayer(context.Background(), tc.req)
			if !errors.Is(err, errs.ErrInvalidAmount) {
				t.Errorf("got %v, want wrapping ErrInvalidAmount", err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// ProcessPurchase
// ----------------------------------------------------------------------------

func TestProcessPurchase_GCWithPromo(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	// Post: GC 100, SC_UNPLAYED 5, SC_REDEEMABLE untouched.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("100.0000"), dec("5.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-pur-1", playerID, "DEPOSIT", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-pur-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// GC: player CREDIT + HOUSE_ISSUANCE_POOL DEBIT.
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "GC", "CREDIT", dec("100.0000"), dec("100.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_ISSUANCE_POOL", "GC", "DEBIT", dec("100.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// SC_UNPLAYED promo: player CREDIT + HOUSE_ISSUANCE_POOL DEBIT.
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_UNPLAYED", "CREDIT", dec("5.0000"), dec("5.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_ISSUANCE_POOL", "SC_UNPLAYED", "DEBIT", dec("5.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessPurchase(context.Background(), PurchaseRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-pur-1",
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "100.0000"),
		SCPromoAmount:         mustMoney(t, "5.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessPurchase: %v", err)
	}
	if got.TransactionType != "DEPOSIT" {
		t.Errorf("Type: got %s want DEPOSIT", got.TransactionType)
	}
	if got.PostBalances.GC.String() != "100.0000" || got.PostBalances.SCUnplayed.String() != "5.0000" {
		t.Errorf("post balances: got GC=%s SCU=%s", got.PostBalances.GC, got.PostBalances.SCUnplayed)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-pur-1")]; !ok {
		t.Error("purchase response must be cached for idempotent replay")
	}
}

func TestProcessPurchase_GhostSpinRecovery(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	committedID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("100.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-pur-ghost", playerID, "DEPOSIT", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-pur-ghost", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	mock.ExpectQuery(rxSelectLedgerByOp).WithArgs(operatorCode, "op-pur-ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(committedID, playerID, "DEPOSIT"))
	mock.ExpectQuery(rxSelectBalances).WithArgs(playerID).
		WillReturnRows(walletRows("100.0000", "0.0000", "0.0000"))

	got, err := e.ProcessPurchase(context.Background(), PurchaseRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-pur-ghost",
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "100.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessPurchase: %v", err)
	}
	if got.Status != StatusGhostRecovered {
		t.Errorf("Status: got %v want %v", got.Status, StatusGhostRecovered)
	}
	if got.LedgerTransactionID != committedID {
		t.Errorf("LedgerTransactionID: got %v want %v", got.LedgerTransactionID, committedID)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-pur-ghost")]; !ok {
		t.Error("ghost recovery must cache the response")
	}
}

func TestProcessPurchase_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  PurchaseRequest
		err  error
	}{
		{"no_op_code", PurchaseRequest{OperatorTransactionID: "x", PlayerID: uuid.New(), GCAmount: mustMoney(t, "1.0000")}, errs.ErrInvalidAmount},
		{"nil_player", PurchaseRequest{OperatorCode: "X", OperatorTransactionID: "x", GCAmount: mustMoney(t, "1.0000")}, errs.ErrPlayerNotFound},
		{"zero_purchase", PurchaseRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(), GCAmount: domain.ZeroMoney(), SCPromoAmount: domain.ZeroMoney()}, errs.ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, idem := newEngine(t)
			_, err := e.ProcessPurchase(context.Background(), tc.req)
			if !errors.Is(err, tc.err) {
				t.Errorf("got %v, want wrapping %v", err, tc.err)
			}
			if len(idem.state) != 0 {
				t.Errorf("validation must short-circuit before the idempotency lock; state=%v", idem.state)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// ProcessRedeem
// ----------------------------------------------------------------------------

func TestProcessRedeem_HappyPath_DrawsRedeemableOnly(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "50.0000"))
	// Redeem 20 → SC_REDEEMABLE 50 → 30. GC/SC_UNPLAYED untouched.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("30.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-red-1", playerID, "WITHDRAWAL", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-red-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "DEBIT", dec("20.0000"), dec("30.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_REDEMPTION_POOL", "SC_REDEEMABLE", "CREDIT", dec("20.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessRedeem(context.Background(), RedeemRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-red-1",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "20.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessRedeem: %v", err)
	}
	if got.TransactionType != "WITHDRAWAL" || got.Family != "SC" {
		t.Errorf("type/family: got %s/%s want WITHDRAWAL/SC", got.TransactionType, got.Family)
	}
	if got.PostBalances.SCRedeemable.String() != "30.0000" {
		t.Errorf("SCRedeemable: got %s want 30.0000", got.PostBalances.SCRedeemable)
	}
}

func TestProcessRedeem_InsufficientRedeemable_RollsBack(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	// Lots of SC_UNPLAYED, little SC_REDEEMABLE: redeem must still fail.
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "1000.0000", "5.0000"))
	mock.ExpectRollback() // allocator rejects before any mutating SQL

	_, err := e.ProcessRedeem(context.Background(), RedeemRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-red-bad",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "10.0000"),
	})
	if !errors.Is(err, errs.ErrInsufficientFunds) {
		t.Fatalf("got %v, want wrapping ErrInsufficientFunds", err)
	}
	if cnt := idem.released[idempotencyKey(operatorCode, "op-red-bad")]; cnt != 1 {
		t.Errorf("Release count: got %d want 1", cnt)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-red-bad")]; ok {
		t.Error("must NOT cache a failed redemption")
	}
}

func TestProcessRedeem_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  RedeemRequest
		err  error
	}{
		{"no_op_tx_id", RedeemRequest{OperatorCode: "X", PlayerID: uuid.New(), Amount: mustMoney(t, "1.0000")}, errs.ErrInvalidAmount},
		{"nil_player", RedeemRequest{OperatorCode: "X", OperatorTransactionID: "x", Amount: mustMoney(t, "1.0000")}, errs.ErrPlayerNotFound},
		{"zero_amount", RedeemRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(), Amount: domain.ZeroMoney()}, errs.ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t)
			_, err := e.ProcessRedeem(context.Background(), tc.req)
			if !errors.Is(err, tc.err) {
				t.Errorf("got %v, want wrapping %v", err, tc.err)
			}
		})
	}
}
