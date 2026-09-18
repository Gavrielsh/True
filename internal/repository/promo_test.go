package repository

// promo_test.go — unit coverage for the AMOE promo-grant path.
//
// The mocks are deliberately written to be read against casino_test.go's
// ProcessPurchase expectations: the two paths must post the SAME player credits
// in the SAME buckets and create the SAME playthrough obligation, differing only
// in transaction type (PROMO_CREDIT vs DEPOSIT) and counterparty account
// (HOUSE_PROMO_POOL vs HOUSE_ISSUANCE_POOL). That pair of differences is the
// whole of the "no purchase necessary" ledger claim; everything else being
// identical is the equal-dignity guarantee.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

const rxInsertPromoGrant = `INSERT INTO promo_grants`

// expectPromoGrantRecord registers the promo_grants insert that records WHICH
// free route issued the coins.
func expectPromoGrantRecord(mock pgxmock.PgxPoolIface, playerID uuid.UUID, channel, reference string) {
	var ref any = reference
	if reference == "" {
		ref = nil
	}
	mock.ExpectExec(rxInsertPromoGrant).
		WithArgs(playerID, pgxmock.AnyArg(), channel, ref, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

// The canonical AMOE grant: Sweeps Coins for a mail-in entry, no GC, no payment.
func TestProcessPromoGrant_AMOECreditsUnplayedAgainstThePromoPool(t *testing.T) {
	t.Parallel()
	e, mock, idem := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	// The status guard IS expected here — the contrast with refund_test.go,
	// where its ABSENCE is the assertion. A grant offers new coins, so a
	// self-excluded player must not receive one.
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("5.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-amoe-1", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-amoe-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectPromoGrantRecord(mock, playerID, "AMOE", "MAIL-2026-000123")
	// The SAME obligation a purchaser's promotional SC carries.
	expectPlaythroughGrant(mock, playerID)
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_UNPLAYED", "CREDIT", dec("5.0000"), dec("5.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", "SC_UNPLAYED", "DEBIT", dec("5.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-amoe-1",
		PlayerID:              playerID,
		SCAmount:              mustMoney(t, "5.0000"),
		Channel:               PromoChannelAMOE,
		ChannelReference:      "MAIL-2026-000123",
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
	if got.TransactionType != "PROMO_CREDIT" {
		t.Errorf("type: got %s want PROMO_CREDIT", got.TransactionType)
	}
	if got.PostBalances.SCUnplayed.String() != "5.0000" {
		t.Errorf("SCUnplayed: got %s want 5.0000", got.PostBalances.SCUnplayed)
	}
	// The line the whole sweepstakes model rests on: a free entry must not
	// produce cashable tokens.
	if got.PostBalances.SCRedeemable.String() != "0.0000" {
		t.Errorf("SCRedeemable: got %s — a promo grant must never credit it", got.PostBalances.SCRedeemable)
	}
	if _, ok := idem.stored[idempotencyKey(operatorCode, "op-amoe-1")]; !ok {
		t.Error("promo grant response must be cached for idempotent replay")
	}
}

// A grant issuing both currencies posts four entries, two per currency, with the
// promo pool on the house side of each.
func TestProcessPromoGrant_BothCurrenciesBalanceAgainstThePromoPool(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("10.0000", "1.0000", "7.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("1010.0000"), dec("3.0000"), dec("7.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-bonus-1", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-bonus-1", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectPromoGrantRecord(mock, playerID, "BONUS", "CAMPAIGN-7")
	expectPlaythroughGrant(mock, playerID)
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "GC", "CREDIT", dec("1000.0000"), dec("1010.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", "GC", "DEBIT", dec("1000.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_UNPLAYED", "CREDIT", dec("2.0000"), dec("3.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", "SC_UNPLAYED", "DEBIT", dec("2.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-bonus-1",
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "1000.0000"),
		SCAmount:              mustMoney(t, "2.0000"),
		Channel:               PromoChannelBonus,
		ChannelReference:      "CAMPAIGN-7",
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
	// The SC leg is what the receipt reports, not the (much larger) GC leg.
	if got.Amount.String() != "2.0000" {
		t.Errorf("Amount: got %s want the SC leg 2.0000", got.Amount)
	}
	if got.PostBalances.SCRedeemable.String() != "7.0000" {
		t.Errorf("SCRedeemable moved to %s; a promo grant must leave it alone", got.PostBalances.SCRedeemable)
	}
}

// A GC-only grant creates NO playthrough obligation — there is no SC to play
// through — and reports the GC leg so the receipt's Amount is never zero.
func TestProcessPromoGrant_GCOnlyCreatesNoObligation(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("500.0000"), dec("0.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-gc-only", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-gc-only", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// channel_reference omitted → NULL. Permitted for every channel but AMOE.
	expectPromoGrantRecord(mock, playerID, "COMPENSATION", "")
	// NO expectPlaythroughGrant here. Strict ordering turns an unexpected
	// sc_playthrough insert into a failure, which is the assertion.
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "GC", "CREDIT", dec("500.0000"), dec("500.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", "GC", "DEBIT", dec("500.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-gc-only",
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "500.0000"),
		Channel:               PromoChannelCompensation,
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
	if got.Amount.String() != "500.0000" {
		t.Errorf("Amount: got %s want the GC leg 500.0000 when no SC was issued", got.Amount)
	}
}

// A blocked player is refused, and the refusal happens INSIDE the wallet lock,
// before any write. The contrast with ProcessRedemptionRefund — which must reach
// a blocked player — is the pair of rules that together protect the same person:
// give back what was taken, never offer anything new.
func TestProcessPromoGrant_RefusesBlockedPlayers(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"SELF_EXCLUDED", "SUSPENDED", "CLOSED", "KYC_PENDING"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			e, mock, _ := newEngine(t)
			playerID := uuid.New()

			mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
				WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
			expectPlayerStatus(mock, playerID, status)
			// No wallet UPDATE, no ledger insert: strict ordering fails the test
			// if the path writes anything after the guard.
			mock.ExpectRollback()

			_, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
				OperatorCode:          operatorCode,
				OperatorTransactionID: "op-blocked-" + status,
				PlayerID:              playerID,
				SCAmount:              mustMoney(t, "5.0000"),
				Channel:               PromoChannelAMOE,
				ChannelReference:      "MAIL-1",
			})
			if !errors.Is(err, errs.ErrPlayerNotActive) {
				t.Fatalf("status %s: got %v, want ErrPlayerNotActive", status, err)
			}
		})
	}
}

// The channel is normalized before validation, so an operator sending a
// lowercase channel is accepted rather than silently rejected — but the value
// that reaches the database is always the canonical enum literal.
func TestProcessPromoGrant_NormalizesChannelAndReference(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	ledgerTxID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxSelectForUpdate).WithArgs(playerID).
		WillReturnRows(walletRows("0.0000", "0.0000", "0.0000"))
	expectPlayerStatus(mock, playerID, "ACTIVE")
	mock.ExpectExec(rxUpdateWallet).
		WithArgs(dec("0.0000"), dec("1.0000"), dec("0.0000"), playerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(rxInsertLedgerTx).
		WithArgs(operatorCode, "op-norm", playerID, "PROMO_CREDIT", nil, nil, nil, json.RawMessage("{}")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(ledgerTxID))
	mock.ExpectExec(rxInsertDedup).
		WithArgs(operatorCode, "op-norm", ledgerTxID).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Uppercased channel, trimmed reference.
	expectPromoGrantRecord(mock, playerID, "AMOE", "MAIL-9")
	expectPlaythroughGrant(mock, playerID)
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, playerID, "PLAYER_WALLET", "SC_UNPLAYED", "CREDIT", dec("1.0000"), dec("1.0000")).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(rxInsertLedgerEntry).
		WithArgs(ledgerTxID, nil, "HOUSE_PROMO_POOL", "SC_UNPLAYED", "DEBIT", dec("1.0000"), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	if _, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: "op-norm",
		PlayerID:              playerID,
		SCAmount:              mustMoney(t, "1.0000"),
		Channel:               PromoChannel("  amoe  "),
		ChannelReference:      "  MAIL-9  ",
	}); err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}
}

func TestProcessPromoGrant_Validation(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	sc := mustMoney(t, "5.0000")

	cases := []struct {
		name string
		req  PromoGrantRequest
		want error
	}{
		{"empty operator code", PromoGrantRequest{
			OperatorTransactionID: "x", PlayerID: playerID, SCAmount: sc, Channel: PromoChannelBonus,
		}, errs.ErrInvalidAmount},
		{"empty transaction id", PromoGrantRequest{
			OperatorCode: operatorCode, PlayerID: playerID, SCAmount: sc, Channel: PromoChannelBonus,
		}, errs.ErrInvalidAmount},
		{"nil player", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", SCAmount: sc, Channel: PromoChannelBonus,
		}, errs.ErrPlayerNotFound},
		{"grant of nothing", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID, Channel: PromoChannelBonus,
		}, errs.ErrInvalidAmount},
		{"unknown channel", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID, SCAmount: sc,
			Channel: PromoChannel("FREEBIE"),
		}, errs.ErrInvalidAmount},
		{"missing channel", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID, SCAmount: sc,
		}, errs.ErrInvalidAmount},
		// The AMOE-specific rule: a free-entry record that cannot be tied back
		// to a received entry is an assertion, not evidence.
		{"AMOE without a reference", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID, SCAmount: sc,
			Channel: PromoChannelAMOE,
		}, errs.ErrInvalidAmount},
		{"AMOE with a whitespace-only reference", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID, SCAmount: sc,
			Channel: PromoChannelAMOE, ChannelReference: "   ",
		}, errs.ErrInvalidAmount},
		{"over-long reference", PromoGrantRequest{
			OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID, SCAmount: sc,
			Channel: PromoChannelBonus, ChannelReference: strings.Repeat("x", maxChannelReferenceLen+1),
		}, errs.ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t)
			_, err := e.ProcessPromoGrant(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}

	// A negative leg is refused before any database work: it would post a ledger
	// entry the amount > 0 CHECK rejects, surfacing as a 500 rather than a 400.
	t.Run("negative leg", func(t *testing.T) {
		t.Parallel()
		e, _, _ := newEngine(t)
		_, err := e.ProcessPromoGrant(context.Background(), PromoGrantRequest{
			OperatorCode:          operatorCode,
			OperatorTransactionID: "op-neg",
			PlayerID:              playerID,
			GCAmount:              mustMoney(t, "0.0000").Sub(mustMoney(t, "1.0000")),
			SCAmount:              sc,
			Channel:               PromoChannelBonus,
		})
		if !errors.Is(err, errs.ErrInvalidAmount) {
			t.Errorf("got %v, want ErrInvalidAmount", err)
		}
	})

	// BONUS and COMPENSATION may omit the reference; only AMOE must carry one.
	for _, channel := range []PromoChannel{PromoChannelBonus, PromoChannelCompensation} {
		t.Run(string(channel)+" needs no reference", func(t *testing.T) {
			t.Parallel()
			req := PromoGrantRequest{
				OperatorCode: operatorCode, OperatorTransactionID: "x", PlayerID: playerID,
				SCAmount: sc, Channel: channel,
			}
			req.normalize()
			if err := req.validate(); err != nil {
				t.Errorf("%s: got %v, want nil", channel, err)
			}
		})
	}
}
