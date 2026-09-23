//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

// A BONUS grant against a real ledger: SC lands in SC_UNPLAYED only, the ledger
// row is PROMO_CREDIT against HOUSE_PROMO_POOL, the typed promo_grants row
// agrees with it, and the wallet still reconciles to the ledger exactly.
func TestIntegration_PromoGrantCreditsUnplayedAndReconciles(t *testing.T) {
	pool := integrationPool(t)
	casino := NewCasino(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1.0000")
	opTxID := "bonus:daily:" + uuid.NewString()

	res, err := casino.ProcessPromoGrant(ctx, PromoGrantRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: opTxID,
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "5000.0000"),
		SCAmount:              mustMoney(t, "0.2000"),
		Channel:               PromoChannelBonus,
	})
	if err != nil {
		t.Fatalf("ProcessPromoGrant: %v", err)
	}

	gc, scU, scR := walletBalances(t, pool, playerID)
	if !gc.Equal(decimal.RequireFromString("5000")) || !scU.Equal(decimal.RequireFromString("1.2")) || !scR.IsZero() {
		t.Errorf("wallet: gc=%s scU=%s scR=%s, want 5000 / 1.2 / 0 (a grant never mints redeemable SC)", gc, scU, scR)
	}
	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)

	var txType, channel string
	var ref *string
	if err := pool.QueryRow(ctx, `
		SELECT lt.transaction_type::text, pg.channel::text, pg.channel_reference
		FROM promo_grants pg
		JOIN ledger_transactions lt ON lt.id = pg.ledger_transaction_id
		WHERE pg.ledger_transaction_id = $1`, res.LedgerTransactionID).Scan(&txType, &channel, &ref); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if txType != "PROMO_CREDIT" || channel != "BONUS" || ref != nil {
		t.Errorf("grant row: type=%s channel=%s ref=%v", txType, channel, ref)
	}

	var housePool int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		WHERE ledger_transaction_id = $1 AND account_type = 'HOUSE_PROMO_POOL' AND direction = 'DEBIT'`,
		res.LedgerTransactionID).Scan(&housePool); err != nil {
		t.Fatalf("read house entries: %v", err)
	}
	if housePool != 2 {
		t.Errorf("HOUSE_PROMO_POOL debits: got %d want 2 (one per issued currency)", housePool)
	}
}

// The same grant sent twice (response lost, Redis barrier absent) credits ONCE:
// the second attempt collides on the global dedup anchor and replays the first.
func TestIntegration_PromoGrantRetryCreditsOnce(t *testing.T) {
	pool := integrationPool(t)
	casino := NewCasino(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1.0000")
	req := PromoGrantRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "amoe:" + uuid.NewString(),
		PlayerID:              playerID,
		SCAmount:              mustMoney(t, "1.0000"),
		Channel:               PromoChannelAMOE,
		ChannelReference:      "AMOE-" + uuid.NewString(),
	}

	first, err := casino.ProcessPromoGrant(ctx, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := casino.ProcessPromoGrant(ctx, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Status != StatusGhostRecovered || second.LedgerTransactionID != first.LedgerTransactionID {
		t.Errorf("retry: status=%s ledger=%s, want GHOST_RECOVERED of %s", second.Status, second.LedgerTransactionID, first.LedgerTransactionID)
	}
	_, scU, _ := walletBalances(t, pool, playerID)
	if !scU.Equal(decimal.RequireFromString("2")) { // 1 seeded + 1 granted once
		t.Errorf("SC_UNPLAYED: got %s want 2 (1 seeded + 1 granted) — a retried grant must credit exactly once", scU)
	}
	assertLedgerReconciles(t, pool, playerID)
}

func TestIntegration_PromoGrantRefusesInactivePlayer(t *testing.T) {
	pool := integrationPool(t)
	casino := NewCasino(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1.0000")
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'SUSPENDED' WHERE id = $1`, playerID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	_, err := casino.ProcessPromoGrant(ctx, PromoGrantRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "bonus:" + uuid.NewString(),
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "100.0000"),
		Channel:               PromoChannelBonus,
	})
	if !errors.Is(err, errs.ErrPlayerNotActive) {
		t.Fatalf("got %v want ErrPlayerNotActive", err)
	}
	gc, _, _ := walletBalances(t, pool, playerID)
	if !gc.IsZero() {
		t.Errorf("GC changed for a suspended player: %s", gc)
	}
}

// Defence in depth below the Go validation: the database itself refuses an
// untraceable AMOE row, and refuses to rewrite or erase any grant.
func TestIntegration_PromoGrantsTableGuards(t *testing.T) {
	pool := integrationPool(t)
	casino := NewCasino(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()
	playerID := seedPlayer(t, pool, "1.0000")

	if _, err := pool.Exec(ctx, `
		INSERT INTO promo_grants (ledger_transaction_id, operator_code, operator_transaction_id,
		                          player_id, channel, gc_amount, sc_amount)
		VALUES ($1, 'OP1', $2, $3, 'AMOE', 0, 1)`, uuid.New(), "raw-"+uuid.NewString(), playerID); err == nil {
		t.Error("an AMOE row without channel_reference must be rejected by the CHECK constraint")
	}

	res, err := casino.ProcessPromoGrant(ctx, PromoGrantRequest{
		OperatorCode: "OP1", OperatorTransactionID: "comp:" + uuid.NewString(), PlayerID: playerID,
		GCAmount: mustMoney(t, "10.0000"), Channel: PromoChannelCompensation,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE promo_grants SET gc_amount = 999 WHERE ledger_transaction_id = $1`,
		res.LedgerTransactionID); err == nil {
		t.Error("promo_grants must be append-only: UPDATE succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM promo_grants WHERE ledger_transaction_id = $1`,
		res.LedgerTransactionID); err == nil {
		t.Error("promo_grants must be append-only: DELETE succeeded")
	}
}
