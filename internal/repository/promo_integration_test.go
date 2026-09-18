//go:build integration

package repository

// promo_integration_test.go proves the AMOE free-entry path against a real
// database: that the ledger carries the no-purchase claim structurally, that a
// free entrant gets exactly what a purchaser gets, that a grant cannot be
// credited twice under concurrency, and that a free entry is refused to the
// players every other grant would be refused to.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

/** Issue a promo grant with sensible AMOE defaults, failing the test on error. */
func grantAMOE(t *testing.T, casino CasinoEngine, playerID uuid.UUID, sc, opTxID string) TxResult {
	t.Helper()
	res, err := casino.ProcessPromoGrant(context.Background(), PromoGrantRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: opTxID,
		PlayerID:              playerID,
		SCAmount:              mustMoney(t, sc),
		Channel:               PromoChannelAMOE,
		ChannelReference:      "MAIL-" + opTxID,
	})
	if err != nil {
		t.Fatalf("promo grant: %v", err)
	}
	return res
}

/** The promo_grants row for a ledger transaction, or a failure if absent. */
func promoGrantRow(t *testing.T, pool *pgxpool.Pool, ledgerTxID uuid.UUID) (channel, reference string, gc, sc decimal.Decimal) {
	t.Helper()
	var ref *string
	err := pool.QueryRow(context.Background(), `
		SELECT channel::text, channel_reference, gc_amount, sc_amount
		FROM promo_grants WHERE ledger_transaction_id = $1`, ledgerTxID).
		Scan(&channel, &ref, &gc, &sc)
	if err != nil {
		t.Fatalf("read promo_grants for %s: %v", ledgerTxID, err)
	}
	if ref != nil {
		reference = *ref
	}
	return channel, reference, gc, sc
}

/** Count of promo_grants rows for a player. */
func promoGrantCount(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM promo_grants WHERE player_id = $1`, playerID).Scan(&n); err != nil {
		t.Fatalf("count promo_grants: %v", err)
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// The ledger facts that carry the "no purchase necessary" claim
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_PromoGrantWritesPromoCreditAgainstThePromoPool(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "0.0001")
	before, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}

	res := grantAMOE(t, casino, playerID, "5.0000", "amoe-"+uuid.NewString())

	// 1. The transaction type. Distinct from DEPOSIT, so the two issuance
	//    routes can never be confused in a query.
	var txType string
	if err := pool.QueryRow(context.Background(),
		`SELECT transaction_type::text FROM ledger_transactions WHERE id = $1`,
		res.LedgerTransactionID).Scan(&txType); err != nil {
		t.Fatalf("read ledger tx: %v", err)
	}
	if txType != "PROMO_CREDIT" {
		t.Errorf("transaction_type = %q, want PROMO_CREDIT", txType)
	}

	// 2. The counterparty. A promo grant has NO fiat leg, and the double-entry
	//    shape says so on its own: HOUSE_PROMO_POOL, never HOUSE_ISSUANCE_POOL.
	rows, err := pool.Query(context.Background(), `
		SELECT account_type::text, currency::text, direction::text, amount
		FROM ledger_entries WHERE ledger_transaction_id = $1
		ORDER BY account_type, currency`, res.LedgerTransactionID)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	defer rows.Close()

	type entry struct {
		account, currency, direction string
		amount                       decimal.Decimal
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.account, &e.currency, &e.direction, &e.amount); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("entries: got %d want 2 (%+v)", len(entries), entries)
	}
	// Ordered by account_type: HOUSE_PROMO_POOL before PLAYER_WALLET.
	house, player := entries[0], entries[1]
	if house.account != "HOUSE_PROMO_POOL" || house.direction != "DEBIT" {
		t.Errorf("house leg: got %s/%s want HOUSE_PROMO_POOL/DEBIT", house.account, house.direction)
	}
	if player.account != "PLAYER_WALLET" || player.direction != "CREDIT" {
		t.Errorf("player leg: got %s/%s want PLAYER_WALLET/CREDIT", player.account, player.direction)
	}
	if player.currency != "SC_UNPLAYED" {
		t.Errorf("player currency: got %s want SC_UNPLAYED", player.currency)
	}
	if !house.amount.Equal(player.amount) {
		t.Errorf("legs do not balance: house %s vs player %s", house.amount, player.amount)
	}

	// 3. No HOUSE_ISSUANCE_POOL entry anywhere in this transaction — there was
	//    no purchase, so there must be nothing that looks like one.
	var issuanceLegs int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ledger_entries
		WHERE ledger_transaction_id = $1 AND account_type = 'HOUSE_ISSUANCE_POOL'`,
		res.LedgerTransactionID).Scan(&issuanceLegs); err != nil {
		t.Fatalf("count issuance legs: %v", err)
	}
	if issuanceLegs != 0 {
		t.Errorf("a free grant posted %d HOUSE_ISSUANCE_POOL entries; it must post none", issuanceLegs)
	}

	// 4. The coins actually landed, in the promotional bucket only.
	after, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances after: %v", err)
	}
	if got := after.SCUnplayed.Decimal().Sub(before.SCUnplayed.Decimal()); !got.Equal(decimal.RequireFromString("5")) {
		t.Errorf("SC_UNPLAYED moved %s, want 5", got)
	}
	if !after.SCRedeemable.Decimal().Equal(before.SCRedeemable.Decimal()) {
		t.Errorf("SC_REDEEMABLE moved from %s to %s; a free entry must never mint cashable tokens",
			before.SCRedeemable, after.SCRedeemable)
	}
}

// The evidence half: WHICH free route this was, in a typed column rather than a
// metadata key.
func TestIntegration_PromoGrantRecordsTheChannelAsAColumn(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "0.0001")
	res := grantAMOE(t, casino, playerID, "5.0000", "amoe-"+uuid.NewString())

	channel, reference, gc, sc := promoGrantRow(t, pool, res.LedgerTransactionID)
	if channel != "AMOE" {
		t.Errorf("channel = %q, want AMOE", channel)
	}
	if reference == "" {
		t.Error("channel_reference is empty; an AMOE grant must name the entry it answers")
	}
	if !sc.Equal(decimal.RequireFromString("5")) {
		t.Errorf("sc_amount = %s, want 5", sc)
	}
	if !gc.IsZero() {
		t.Errorf("gc_amount = %s, want 0", gc)
	}

	// The regulator-facing query this table exists to make possible.
	var amoeCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM promo_grants WHERE channel = 'AMOE' AND player_id = $1`, playerID).
		Scan(&amoeCount); err != nil {
		t.Fatalf("count AMOE grants: %v", err)
	}
	if amoeCount != 1 {
		t.Errorf("AMOE grants for this player: got %d want 1", amoeCount)
	}
}

// The database refuses an AMOE row with no reference even if a future writer
// bypasses the engine's validation. Proven by writing directly.
func TestIntegration_PromoGrantsRefusesAMOEWithoutAReference(t *testing.T) {
	pool := integrationPool(t)
	playerID := seedPlayer(t, pool, "0.0001")

	_, err := pool.Exec(context.Background(), `
		INSERT INTO promo_grants (player_id, ledger_transaction_id, channel, sc_amount)
		VALUES ($1, $2, 'AMOE', 5)`, playerID, uuid.New())
	if err == nil {
		t.Fatal("the database accepted an AMOE grant with no channel_reference; " +
			"the constraint must hold for every writer, not only the engine")
	}

	// A non-AMOE channel may legitimately omit it.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO promo_grants (player_id, ledger_transaction_id, channel, sc_amount)
		VALUES ($1, $2, 'BONUS', 5)`, playerID, uuid.New()); err != nil {
		t.Errorf("BONUS with no reference was refused: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Equal dignity
// ─────────────────────────────────────────────────────────────────────────────

// The condition that keeps a sweepstakes from being a lottery: the free route
// must offer materially the same thing as the paid one. Asserted end to end
// against a real database rather than at the allocator, because "the same" has
// to include the wagering requirement, not just the balance.
func TestIntegration_AMOEGrantGetsTheSameCoinsAndTheSameObligationAsAPurchase(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	buyer := seedPlayer(t, pool, "0.0001")
	entrant := seedPlayer(t, pool, "0.0001")

	// The buyer pays for a package carrying 5 promotional SC.
	if _, err := casino.ProcessPurchase(context.Background(), PurchaseRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "pur-" + uuid.NewString(),
		PlayerID:              buyer,
		GCAmount:              mustMoney(t, "100.0000"),
		SCPromoAmount:         mustMoney(t, "5.0000"),
	}); err != nil {
		t.Fatalf("purchase: %v", err)
	}

	// The entrant mails in and receives the same 5 SC, paying nothing.
	grantAMOE(t, casino, entrant, "5.0000", "amoe-"+uuid.NewString())

	buyerBal, err := eng.GetBalances(context.Background(), buyer)
	if err != nil {
		t.Fatalf("buyer balances: %v", err)
	}
	entrantBal, err := eng.GetBalances(context.Background(), entrant)
	if err != nil {
		t.Fatalf("entrant balances: %v", err)
	}

	// Same sweepstakes coins, in the same bucket.
	if !buyerBal.SCUnplayed.Decimal().Equal(entrantBal.SCUnplayed.Decimal()) {
		t.Errorf("SC_UNPLAYED: buyer has %s, AMOE entrant has %s — the free route must not "+
			"deliver a different coin", buyerBal.SCUnplayed, entrantBal.SCUnplayed)
	}
	if !buyerBal.SCRedeemable.Decimal().Equal(entrantBal.SCRedeemable.Decimal()) {
		t.Errorf("SC_REDEEMABLE: buyer %s, entrant %s", buyerBal.SCRedeemable, entrantBal.SCRedeemable)
	}

	// And the same wagering requirement. A free entry under a HEAVIER
	// requirement would be equal in balance and unequal in substance.
	outstanding := func(playerID uuid.UUID) decimal.Decimal {
		t.Helper()
		var d decimal.Decimal
		if err := pool.QueryRow(context.Background(), `
			SELECT COALESCE(SUM(required_amount - wagered_amount), 0)
			FROM sc_playthrough WHERE player_id = $1 AND status = 'OUTSTANDING'`, playerID).Scan(&d); err != nil {
			t.Fatalf("outstanding: %v", err)
		}
		return d
	}
	buyerDue, entrantDue := outstanding(buyer), outstanding(entrant)
	if !buyerDue.Equal(entrantDue) {
		t.Errorf("wagering requirement: buyer owes %s, AMOE entrant owes %s — the free route must "+
			"not carry a heavier obligation", buyerDue, entrantDue)
	}
	if !entrantDue.Equal(decimal.RequireFromString("5")) {
		t.Errorf("AMOE entrant owes %s, want the standard 1x of 5", entrantDue)
	}
}

// A free entry does not become cash without being played. The grant creates the
// obligation, and the redemption gate reads it.
func TestIntegration_AMOEGrantCannotBeRedeemedWithoutPlaying(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "0.0001")
	// Give the player a redeemable balance from prior play, so the refusal is
	// unambiguously the playthrough gate and not insufficient funds.
	creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "50.0000")

	grantAMOE(t, casino, playerID, "5.0000", "amoe-"+uuid.NewString())

	_, err := casino.ProcessRedeem(context.Background(), RedeemRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "redeem-" + uuid.NewString(),
		PlayerID:              playerID,
		Amount:                mustMoney(t, "10.0000"),
	})
	if err == nil {
		t.Fatal("a redemption succeeded while an AMOE grant was outstanding; the free route " +
			"must not be a path from mailed entry to cash without gameplay")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Idempotency
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_PromoGrantReplayIssuesCoinsOnce(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "0.0001")
	opTxID := "amoe-" + uuid.NewString()

	first := grantAMOE(t, casino, playerID, "5.0000", opTxID)
	afterFirst, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}

	// openIdem is a no-op barrier, so this replay reaches the database and is
	// caught by ledger_transaction_dedup — the durable layer, not the Redis one.
	second := grantAMOE(t, casino, playerID, "5.0000", opTxID)

	if second.LedgerTransactionID != first.LedgerTransactionID {
		t.Errorf("replay produced a NEW ledger transaction %s (first was %s)",
			second.LedgerTransactionID, first.LedgerTransactionID)
	}
	afterSecond, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances after replay: %v", err)
	}
	if !afterSecond.SCUnplayed.Decimal().Equal(afterFirst.SCUnplayed.Decimal()) {
		t.Fatalf("a replayed grant issued a SECOND set of coins: %s → %s",
			afterFirst.SCUnplayed, afterSecond.SCUnplayed)
	}
	// And the AMOE record was not duplicated — one mailed entry, one row.
	if n := promoGrantCount(t, pool, playerID); n != 1 {
		t.Errorf("promo_grants rows: got %d want 1; one mailed entry must not appear as two", n)
	}
}

// The same grant replayed from many goroutines at once. A promo grant is a pure
// CREDIT bounded by nothing the player owns, so a duplicate here is not a
// mis-posting — it is coins invented.
func TestIntegration_PromoGrantConcurrentReplaysCreditOnce(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "0.0001")
	before, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}

	const replays = 25
	opTxID := "amoe-race-" + uuid.NewString()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ledgers = map[uuid.UUID]int{}
		failed  int
	)
	for i := 0; i < replays; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := casino.ProcessPromoGrant(context.Background(), PromoGrantRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: opTxID,
				PlayerID:              playerID,
				SCAmount:              mustMoney(t, "5.0000"),
				Channel:               PromoChannelAMOE,
				ChannelReference:      "MAIL-RACE",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// A conflicting concurrent replay may legitimately be refused;
				// what must never happen is a second CREDIT.
				failed++
				return
			}
			ledgers[res.LedgerTransactionID]++
		}()
	}
	wg.Wait()

	if len(ledgers) == 0 {
		t.Fatalf("every one of %d concurrent grants failed (%d refusals)", replays, failed)
	}
	if len(ledgers) != 1 {
		t.Errorf("%d DISTINCT ledger transactions for one grant: %v", len(ledgers), ledgers)
	}

	after, err := eng.GetBalances(context.Background(), playerID)
	if err != nil {
		t.Fatalf("balances after: %v", err)
	}
	moved := after.SCUnplayed.Decimal().Sub(before.SCUnplayed.Decimal())
	if !moved.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("%d concurrent replays credited %s SC; exactly 5 must have been issued",
			replays, moved)
	}
	if n := promoGrantCount(t, pool, playerID); n != 1 {
		t.Errorf("promo_grants rows: got %d want 1", n)
	}

	// The wagering obligation must not have been doubled either: a player who
	// received one grant must owe one grant's worth of play.
	var due decimal.Decimal
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(required_amount), 0) FROM sc_playthrough WHERE player_id = $1`,
		playerID).Scan(&due); err != nil {
		t.Fatalf("read playthrough: %v", err)
	}
	if !due.Equal(decimal.RequireFromString("5")) {
		t.Errorf("playthrough required_amount total = %s, want 5", due)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The status guard — the deliberate contrast with the refund path
// ─────────────────────────────────────────────────────────────────────────────

// ProcessRedemptionRefund must REACH a blocked player; ProcessPromoGrant must
// not. Both rules protect the same person: give back what was taken, never offer
// anything new. Asserted against a live database because the guard's value comes
// from running inside the wallet lock.
func TestIntegration_PromoGrantIsRefusedToBlockedPlayers(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	for _, status := range []string{"SELF_EXCLUDED", "SUSPENDED", "CLOSED"} {
		t.Run(status, func(t *testing.T) {
			playerID := seedPlayer(t, pool, "0.0001")
			// SELF_EXCLUDED carries a mandatory term (users_self_exclusion_has_term),
			// so it cannot be set by status alone — the schema refuses an exclusion
			// with no end date, which is the constraint working.
			var term any
			if status == "SELF_EXCLUDED" {
				term = time.Now().Add(180 * 24 * time.Hour)
			}
			if _, err := pool.Exec(context.Background(), fmt.Sprintf(
				`UPDATE users SET status = '%s', self_exclusion_until = $2 WHERE id = $1`, status),
				playerID, term); err != nil {
				t.Fatalf("block player: %v", err)
			}

			before, err := eng.GetBalances(context.Background(), playerID)
			if err != nil {
				t.Fatalf("balances: %v", err)
			}

			_, err = casino.ProcessPromoGrant(context.Background(), PromoGrantRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: "amoe-blocked-" + uuid.NewString(),
				PlayerID:              playerID,
				SCAmount:              mustMoney(t, "5.0000"),
				Channel:               PromoChannelAMOE,
				ChannelReference:      "MAIL-BLOCKED",
			})
			if err == nil {
				t.Fatalf("a %s player was granted free coins; offering new entries to a blocked "+
					"player is the inducement the status controls exist to prevent", status)
			}

			after, err := eng.GetBalances(context.Background(), playerID)
			if err != nil {
				t.Fatalf("balances after: %v", err)
			}
			if !after.SCUnplayed.Decimal().Equal(before.SCUnplayed.Decimal()) {
				t.Errorf("a refused grant still moved SC_UNPLAYED: %s → %s", before.SCUnplayed, after.SCUnplayed)
			}
			if n := promoGrantCount(t, pool, playerID); n != 0 {
				t.Errorf("a refused grant left %d promo_grants rows behind", n)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The house pool
// ─────────────────────────────────────────────────────────────────────────────

// Every grant's house leg is a DEBIT, so the promo pool's net balance is the
// negative of everything issued for free — "how much have we given away" as a
// single number, which is what makes the AMOE exposure auditable.
func TestIntegration_PromoPoolNetsTheTotalIssuedForFree(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	before := houseAccountBalance(t, pool, "HOUSE_PROMO_POOL")

	playerID := seedPlayer(t, pool, "0.0001")
	grantAMOE(t, casino, playerID, "5.0000", "amoe-"+uuid.NewString())
	grantAMOE(t, casino, playerID, "2.5000", "amoe-"+uuid.NewString())

	after := houseAccountBalance(t, pool, "HOUSE_PROMO_POOL")
	// houseAccountBalance is CREDIT minus DEBIT; the grants are DEBITs.
	if got := before.Sub(after); !got.Equal(decimal.RequireFromString("7.5")) {
		t.Errorf("promo pool moved %s, want 7.5 issued", got)
	}
}
