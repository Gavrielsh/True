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

const rxInsertPromoGrant = `INSERT INTO promo_grants`

// A daily-bonus grant of SC only: one SC_UNPLAYED player CREDIT balanced by a
// HOUSE_PROMO_POOL DEBIT, booked as PROMO_CREDIT (never DEPOSIT), with the
// typed promo_grants row written in the same transaction.
func TestProcessPromoGrant_BonusSCOnly(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("10.0000", "1.0000", "2.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	expectNoExclusion(mock, playerID)
	// Post: SC_UNPLAYED +0.2; GC and SC_REDEEMABLE untouched.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("10.0000"), dec("1.2000"), dec("2.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "bonus:daily:1", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}"), nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "bonus:daily:1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertPromoGrant).
		WithArgs(ledgerTxID, operatorCode, "bonus:daily:1", playerID, "BONUS", nil, dec("0"), dec("0.2000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_UNPLAYED", "CREDIT", dec("0.2000"), dec("1.2000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", "SC_UNPLAYED", "DEBIT", dec("0.2000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "bonus:daily:1",
		PlayerID:              playerID,
		GCAmount:              domain.ZeroMoney(),
		SCAmount:              mustMoney(t, "0.2000"),
		Channel:               PromoChannelBonus,
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
	if got.TransactionType != "PROMO_CREDIT" {
		t.Errorf("Type: got %s want PROMO_CREDIT", got.TransactionType)
	}
	if got.PostBalances.SCUnplayed.String() != "1.2000" || got.PostBalances.SCRedeemable.String() != "2.0000" {
		t.Errorf("post balances: SCU=%s SCR=%s", got.PostBalances.SCUnplayed, got.PostBalances.SCRedeemable)
	}
	if got.Amount.String() != "0.2000" {
		t.Errorf("headline amount: got %s want the SC amount when no GC was granted", got.Amount)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "bonus:daily:1")]; !ok {
		t.Error("grant response must be cached for idempotent replay")
	}
}

// AMOE carries its channel reference into the typed row.
func TestProcessPromoGrant_AMOEWithReference(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	expectNoExclusion(mock, playerID)
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("100.0000"), dec("1.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "amoe:g1", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}"), nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "amoe:g1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertPromoGrant).
		WithArgs(ledgerTxID, operatorCode, "amoe:g1", playerID, "AMOE", "AMOE-g1", dec("100.0000"), dec("1.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	for _, cur := range []struct{ c, amt string }{{"GC", "100.0000"}, {"SC_UNPLAYED", "1.0000"}} {
		mock.ExpectExec(rxInsertLedgerEntry).
			WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", cur.c, "CREDIT", dec(cur.amt), dec(cur.amt)).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectExec(rxInsertLedgerEntry).
			WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", cur.c, "DEBIT", dec(cur.amt), nil).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectCommit()

	got, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "amoe:g1",
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "100.0000"),
		SCAmount:              mustMoney(t, "1.0000"),
		Channel:               PromoChannelAMOE,
		ChannelReference:      "AMOE-g1",
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
	if got.Amount.String() != "100.0000" {
		t.Errorf("headline amount: got %s want the GC amount", got.Amount)
	}
}

// A retry whose response was lost replays the ORIGINAL grant — the unique
// dedup anchor turns the second attempt into a recovery, never a second credit.
func TestProcessPromoGrant_GhostSpinRecovery(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	committedID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	expectNoExclusion(mock, playerID)
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.5000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "bonus:ghost", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}"), nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "bonus:ghost", pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	mock.ExpectQuery(rxSelectLedgerByOp).WithArgs(operatorCode, "bonus:ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(committedID, playerID, "PROMO_CREDIT"))
	mock.ExpectQuery(rxSelectBalances).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.5000", "0.0000"))

	got, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "bonus:ghost",
		PlayerID:              playerID,
		SCAmount:              mustMoney(t, "0.5000"),
		Channel:               PromoChannelBonus,
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
	if got.Status != StatusGhostRecovered || got.LedgerTransactionID != committedID {
		t.Errorf("got status=%v ledger=%v, want ghost recovery of %v", got.Status, got.LedgerTransactionID, committedID)
	}
}

// An operator id already used for a PURCHASE cannot be replayed as a grant:
// the stored type differs, so recovery refuses instead of passing it off.
func TestProcessPromoGrant_GhostSpin_RejectsTypeMismatch(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	expectNoExclusion(mock, playerID)
	mock.ExpectExec(rxUpdateWallet).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectExec(rxInsertDedup).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	mock.ExpectQuery(rxSelectLedgerByOp).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "player_id", "transaction_type"}).
			AddRow(uuid.New(), playerID, "DEPOSIT"))

	_, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "reused",
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "5.0000"),
		Channel:               PromoChannelCompensation,
	})
	if !errors.Is(err, errs.ErrTransactionConflict) {
		t.Fatalf("err: got %v want ErrTransactionConflict", err)
	}
}

// A suspended / self-excluded / closed account receives nothing — not even a
// free entry — and the wallet row is never written.
func TestProcessPromoGrant_InactivePlayerRefused(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "SELF_EXCLUDED")
	mock.ExpectRollback()

	_, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "amoe:blocked",
		PlayerID:              playerID,
		SCAmount:              mustMoney(t, "1.0000"),
		Channel:               PromoChannelAMOE,
		ChannelReference:      "AMOE-blocked",
	})
	if !errors.Is(err, errs.ErrPlayerNotActive) {
		t.Fatalf("err: got %v want ErrPlayerNotActive", err)
	}
	if _, cached := idem.stored[idempotencyKey(operatorCode, "amoe:blocked")]; cached {
		t.Error("a refused grant must release, not cache, the idempotency key")
	}
}

func TestProcessPromoGrant_Validation(t *testing.T) {
	t.Parallel()
	one := func() domain.Money { return mustMoney(t, "1.0000") }
	base := func() PromoGrantRequest {
		return PromoGrantRequest{OperatorCode: "X", OperatorTransactionID: "x", PlayerID: uuid.New(),
			SCAmount: one(), Channel: PromoChannelBonus}
	}
	cases := []struct {
		name   string
		mutate func(*PromoGrantRequest)
		want   error
	}{
		{"no_op_code", func(r *PromoGrantRequest) { r.OperatorCode = "" }, errs.ErrInvalidAmount},
		{"no_op_tx", func(r *PromoGrantRequest) { r.OperatorTransactionID = "" }, errs.ErrInvalidAmount},
		{"nil_player", func(r *PromoGrantRequest) { r.PlayerID = uuid.Nil }, errs.ErrPlayerNotFound},
		{"unknown_channel", func(r *PromoGrantRequest) { r.Channel = "FREE_MONEY" }, errs.ErrInvalidAmount},
		{"amoe_without_reference", func(r *PromoGrantRequest) { r.Channel = PromoChannelAMOE }, errs.ErrInvalidAmount},
		{"amoe_blank_reference", func(r *PromoGrantRequest) { r.Channel = PromoChannelAMOE; r.ChannelReference = "  " }, errs.ErrInvalidAmount},
		{"reference_too_long", func(r *PromoGrantRequest) { r.ChannelReference = string(make([]byte, 129)) }, errs.ErrInvalidAmount},
		{"zero_grant", func(r *PromoGrantRequest) { r.SCAmount = domain.ZeroMoney() }, errs.ErrInvalidAmount},
		{"negative_gc", func(r *PromoGrantRequest) { r.GCAmount = mustMoney(t, "-1.0000") }, errs.ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t) // no DB expectations: validation fails before any I/O
			req := base()
			tc.mutate(&req)
			if _, err := e.ProcessPromoGrant(context.Background(), req); !errors.Is(err, tc.want) {
				t.Fatalf("err: got %v want %v", err, tc.want)
			}
		})
	}
}
