package repository

// refund_test.go — unit coverage for the compensating credit, mirroring the
// ProcessRedeem mocks so the two can be read side by side and their ENTRIES
// compared directly: the refund's must be the redeem's, reversed.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

func TestProcessRedemptionRefund_CreditsRedeemableAndDebitsThePool(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "30.0000"))
	// NOTE what is NOT expected here: no player-status read and no playthrough
	// read. Their absence is asserted by pgxmock's strict ordering — an
	// accidental guard would show up as an unexpected query, which is exactly
	// how this test defends the "a refund must reach a blocked player" rule.
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("50.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-refund-1", playerID, "REDEMPTION_REFUND", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-refund-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// The mirror of ProcessRedeem: player CREDIT, house DEBIT.
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "CREDIT", dec("20.0000"), dec("50.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_REDEMPTION_POOL", "SC_REDEEMABLE", "DEBIT", dec("20.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-refund-1",
		PlayerID:              playerID,
		Amount:                mustMoney(t, "20.0000"),
	})
	if err != nil {
		t.Fatalf("ProcessRedemptionRefund: %v", err)
	}
	if got.TransactionType != "REDEMPTION_REFUND" || got.Family != "SC" {
		t.Errorf("type/family: got %s/%s want REDEMPTION_REFUND/SC", got.TransactionType, got.Family)
	}
	if got.PostBalances.SCRedeemable.String() != "50.0000" {
		t.Errorf("SCRedeemable: got %s want 50.0000", got.PostBalances.SCRedeemable)
	}
	// The refund must not touch the promotional bucket: returning it as
	// SC_UNPLAYED would impose a fresh playthrough on money already earned out.
	if got.PostBalances.SCUnplayed.String() != "0.0000" {
		t.Errorf("SCUnplayed: got %s want 0.0000", got.PostBalances.SCUnplayed)
	}
}

func TestProcessRedemptionRefund_RecordsTheOriginalDebitAsReference(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()
	originalDebit := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("0.0000"), dec("5.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// The reference is carried through so the debit and its refund read as one story.
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-refund-ref", playerID, "REDEMPTION_REFUND", nil, nil, originalDebit, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-refund-ref", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_REDEEMABLE", "CREDIT", dec("5.0000"), dec("5.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_REDEMPTION_POOL", "SC_REDEEMABLE", "DEBIT", dec("5.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	if _, err := e.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
		OperatorCode:           operatorCode,
		OperatorTransactionID:  "op-refund-ref",
		PlayerID:               playerID,
		Amount:                 mustMoney(t, "5.0000"),
		ReferenceTransactionID: originalDebit,
	}); err != nil {
		t.Fatalf("ProcessRedemptionRefund: %v", err)
	}
}

func TestProcessRedemptionRefund_Validation(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()

	cases := []struct {
		name string
		req  RedemptionRefundRequest
		want error
	}{
		{"empty operator code", RedemptionRefundRequest{OperatorTransactionID: "x", PlayerID: playerID}, errs.ErrInvalidAmount},
		{"empty transaction id", RedemptionRefundRequest{OperatorCode: operatorCode, PlayerID: playerID}, errs.ErrInvalidAmount},
		{"nil player", RedemptionRefundRequest{OperatorCode: operatorCode, OperatorTransactionID: "x"}, errs.ErrPlayerNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t)
			_, err := e.ProcessRedemptionRefund(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}

	// A zero amount is refused before any database work: refunding nothing is a
	// caller bug, and writing a zero-value ledger transaction would hide it.
	t.Run("zero amount", func(t *testing.T) {
		t.Parallel()
		e, _, _ := newEngine(t)
		_, err := e.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
			OperatorCode:          operatorCode,
			OperatorTransactionID: "op-zero",
			PlayerID:              playerID,
			Amount:                mustMoney(t, "0"),
		})
		if !errors.Is(err, errs.ErrInvalidAmount) {
			t.Errorf("got %v, want ErrInvalidAmount", err)
		}
	})
}
