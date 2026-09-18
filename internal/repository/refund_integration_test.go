//go:build integration

package repository

// refund_integration_test.go proves the compensating credit against a real
// database: that it restores exactly what the redemption took, that it balances
// the house pool back to zero, that it cannot be credited twice, and — the one
// that matters most — that it reaches a player the other money paths refuse.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

/** Redeem `amount`, then return the redemption's ledger transaction id. */
func redeemOnce(t *testing.T, casino CasinoEngine, playerID uuid.UUID, amount string) TxResult {
	t.Helper()
	res, err := casino.ProcessRedeem(context.Background(), RedeemRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "redeem:" + uuid.NewString(),
		PlayerID:              playerID,
		Amount:                mustMoney(t, amount),
	})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return res
}

/**
 * Net CREDIT minus DEBIT for a house account, in one currency family. The house
 * pool is a running total the double-entry ledger maintains; reading it here is
 * how a test can assert that a redemption and its refund cancel.
 */
func houseAccountBalance(t *testing.T, pool *pgxpool.Pool, accountType string) decimal.Decimal {
	t.Helper()
	var net decimal.Decimal
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE -amount END), 0)
		FROM ledger_entries
		WHERE account_type = $1`, accountType).Scan(&net)
	if err != nil {
		t.Fatalf("house balance %s: %v", accountType, err)
	}
	return net
}

func TestIntegration_RedemptionRefundRestoresExactlyWhatWasTaken(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "10.0000")
	creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "500.0000")

	before, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}

	redeem := redeemOnce(t, casino, playerID, "125.5000")

	mid, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances after redeem: %v", err)
	}
	if got := before.SCRedeemable.Decimal().Sub(mid.SCRedeemable.Decimal()); !got.Equal(decimal.RequireFromString("125.5")) {
		t.Fatalf("redeem moved %s, want 125.5", got)
	}

	refund, err := casino.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
		OperatorCode:           "OP1",
		OperatorTransactionID:  "refund:" + redeem.OperatorTransactionID,
		PlayerID:               playerID,
		Amount:                 mustMoney(t, "125.5000"),
		ReferenceTransactionID: redeem.LedgerTransactionID,
	})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refund.TransactionType != "REDEMPTION_REFUND" {
		t.Errorf("transaction type = %q, want REDEMPTION_REFUND", refund.TransactionType)
	}

	after, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances after refund: %v", err)
	}
	// Exactly restored — not approximately, and not into a different bucket.
	if !after.SCRedeemable.Decimal().Equal(before.SCRedeemable.Decimal()) {
		t.Errorf("SC_REDEEMABLE = %s, want the pre-redemption %s", after.SCRedeemable, before.SCRedeemable)
	}
	if !after.SCUnplayed.Decimal().Equal(before.SCUnplayed.Decimal()) {
		t.Errorf("SC_UNPLAYED = %s, want unchanged %s — a refund must not impose a fresh playthrough",
			after.SCUnplayed, before.SCUnplayed)
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

func TestIntegration_RedemptionRefundNetsTheHousePoolToZero(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "10.0000")
	creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "300.0000")

	poolBefore := houseAccountBalance(t, pool, accountHouseRedemptionPool)
	redeem := redeemOnce(t, casino, playerID, "80.0000")
	poolMid := houseAccountBalance(t, pool, accountHouseRedemptionPool)

	if got := poolMid.Sub(poolBefore); !got.Equal(decimal.RequireFromString("80")) {
		t.Fatalf("redeem moved the pool by %s, want 80", got)
	}

	if _, err := casino.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
		OperatorCode:           "OP1",
		OperatorTransactionID:  "refund:" + redeem.OperatorTransactionID,
		PlayerID:               playerID,
		Amount:                 mustMoney(t, "80.0000"),
		ReferenceTransactionID: redeem.LedgerTransactionID,
	}); err != nil {
		t.Fatalf("refund: %v", err)
	}

	// Taken and given back nets to nothing: the pool is exactly where it started.
	poolAfter := houseAccountBalance(t, pool, accountHouseRedemptionPool)
	if !poolAfter.Equal(poolBefore) {
		t.Errorf("house pool = %s, want the original %s", poolAfter, poolBefore)
	}
	assertDoubleEntryBalanced(t, pool)
}

func TestIntegration_RedemptionRefundIsIdempotent(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "10.0000")
	creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "200.0000")
	redeem := redeemOnce(t, casino, playerID, "50.0000")

	anchor := "refund:" + redeem.OperatorTransactionID
	req := RedemptionRefundRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: anchor,
		PlayerID:              playerID,
		Amount:                mustMoney(t, "50.0000"),
	}

	first, err := casino.ProcessRedemptionRefund(context.Background(), req)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	afterFirst, _ := eng.GetBalances(context.Background(), playerID)

	// The same anchor again — a retried admin rejection, or a redelivered worker
	// message. The Redis barrier is disabled here (openIdem), so this is the
	// DATABASE proving it, which is the layer that has to hold.
	second, err := casino.ProcessRedemptionRefund(context.Background(), req)
	if err != nil {
		t.Fatalf("second refund: %v", err)
	}
	if second.LedgerTransactionID != first.LedgerTransactionID {
		t.Errorf("second refund got ledger tx %s, want the original %s — money was invented",
			second.LedgerTransactionID, first.LedgerTransactionID)
	}

	afterSecond, _ := eng.GetBalances(context.Background(), playerID)
	if !afterSecond.SCRedeemable.Decimal().Equal(afterFirst.SCRedeemable.Decimal()) {
		t.Errorf("balance moved on the replay: %s → %s", afterFirst.SCRedeemable, afterSecond.SCRedeemable)
	}
	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

func TestIntegration_RedemptionRefundConcurrentReplaysCreditOnce(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "10.0000")
	creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "400.0000")
	before, _ := eng.GetBalances(context.Background(), playerID)

	const concurrency = 25
	anchor := "refund:concurrent:" + uuid.NewString()

	done := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			_, err := casino.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: anchor,
				PlayerID:              playerID,
				Amount:                mustMoney(t, "60.0000"),
			})
			done <- err
		}()
	}
	for i := 0; i < concurrency; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent refund %d: %v", i, err)
		}
	}

	// 25 simultaneous attempts, ONE credit. This is the defect the anchor exists
	// to make impossible: a refund credited 25 times is 1,500 SC invented.
	after, _ := eng.GetBalances(context.Background(), playerID)
	moved := after.SCRedeemable.Decimal().Sub(before.SCRedeemable.Decimal())
	if !moved.Equal(decimal.RequireFromString("60")) {
		t.Errorf("balance moved by %s across %d replays, want exactly 60", moved, concurrency)
	}
	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

func TestIntegration_RedemptionRefundReachesABlockedPlayer(t *testing.T) {
	// THE point of this primitive. Every other money path refuses these players,
	// and must; this one must not, or a player protection mechanism becomes a
	// confiscation.
	for _, status := range blockedStatuses {
		t.Run(status, func(t *testing.T) {
			pool := integrationPool(t)
			casino := newIntegrationCasino(pool)
			eng := New(pool, openIdem{}, discardLoggerRepo())

			playerID := seedPlayer(t, pool, "10.0000")
			creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "150.0000")
			redeem := redeemOnce(t, casino, playerID, "100.0000")

			// The player is blocked AFTER asking to redeem — the ordering that makes
			// this the realistic case rather than a contrived one.
			applyBlock(t, pool, casino, playerID, status)

			before, _ := eng.GetBalances(context.Background(), playerID)
			if _, err := casino.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
				OperatorCode:           "OP1",
				OperatorTransactionID:  "refund:" + redeem.OperatorTransactionID,
				PlayerID:               playerID,
				Amount:                 mustMoney(t, "100.0000"),
				ReferenceTransactionID: redeem.LedgerTransactionID,
			}); err != nil {
				t.Fatalf("refund to a %s player was refused: %v — their money is stranded", status, err)
			}

			after, _ := eng.GetBalances(context.Background(), playerID)
			moved := after.SCRedeemable.Decimal().Sub(before.SCRedeemable.Decimal())
			if !moved.Equal(decimal.RequireFromString("100")) {
				t.Errorf("refund credited %s, want 100", moved)
			}

			// And the block still holds for everything that moves money OUT.
			if _, err := casino.ProcessRedeem(context.Background(), RedeemRequest{
				OperatorCode: "OP1", OperatorTransactionID: "redeem-after-" + uuid.NewString(),
				PlayerID: playerID, Amount: mustMoney(t, "10.0000"),
			}); err == nil {
				t.Error("a redemption by a blocked player succeeded — the refund must not have unblocked them")
			}
			assertDoubleEntryBalanced(t, pool)
		})
	}
}

func TestIntegration_RedemptionRefundRejectsAnInvalidAmount(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	playerID := seedPlayer(t, pool, "10.0000")

	for _, amount := range []string{"0", "0.0000"} {
		if _, err := casino.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
			OperatorCode: "OP1", OperatorTransactionID: "refund-bad-" + uuid.NewString(),
			PlayerID: playerID, Amount: mustMoney(t, amount),
		}); err == nil {
			t.Errorf("amount %q was accepted; want ErrInvalidAmount", amount)
		}
	}

	// An unknown player cannot be refunded into existence.
	if _, err := casino.ProcessRedemptionRefund(context.Background(), RedemptionRefundRequest{
		OperatorCode: "OP1", OperatorTransactionID: "refund-ghost-" + uuid.NewString(),
		PlayerID: uuid.New(), Amount: mustMoney(t, "10.0000"),
	}); err == nil {
		t.Error("refund to an unknown player succeeded")
	}
}
