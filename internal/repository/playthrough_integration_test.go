//go:build integration

package repository

// playthrough_integration_test.go proves the 1× sweepstakes wagering
// requirement end to end: a grant creates an obligation, wagering the granted
// coins discharges it, and redemption is refused until it is discharged.
//
// The properties worth testing here are the ones the SQL decides — the FIFO
// waterfall across multiple grants, the fact that SC_REDEEMABLE wagering does
// NOT count, and that concurrent wagers cannot discharge the same obligation
// twice.
//
//	TEST_POSTGRES_URL=postgres://postgres@127.0.0.1:5433/true_engine?sslmode=disable \
//	    go test -tags integration ./internal/repository/ -run Integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// grantPromoSC issues promotional SC through the real purchase path, so the
// obligation is created the way production creates it rather than by a fixture
// that could drift from it.
func grantPromoSC(t *testing.T, casino CasinoEngine, playerID uuid.UUID, gc, scPromo string) {
	t.Helper()
	if _, err := casino.ProcessPurchase(context.Background(), PurchaseRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "promo-" + uuid.NewString(),
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, gc),
		SCPromoAmount:         mustMoney(t, scPromo),
	}); err != nil {
		t.Fatalf("grant promo SC: %v", err)
	}
}

func outstandingPlaythrough(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) decimal.Decimal {
	t.Helper()
	var out decimal.Decimal
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(required_amount - wagered_amount), 0)
		FROM sc_playthrough WHERE player_id = $1 AND status = 'OUTSTANDING'`,
		playerID).Scan(&out); err != nil {
		t.Fatalf("read outstanding: %v", err)
	}
	return out
}

func playthroughRows(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) []struct {
	Granted, Required, Wagered decimal.Decimal
	Status                     string
} {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT granted_amount, required_amount, wagered_amount, status
		FROM sc_playthrough WHERE player_id = $1 ORDER BY created_at, id`, playerID)
	if err != nil {
		t.Fatalf("read playthrough rows: %v", err)
	}
	defer rows.Close()

	var out []struct {
		Granted, Required, Wagered decimal.Decimal
		Status                     string
	}
	for rows.Next() {
		var r struct {
			Granted, Required, Wagered decimal.Decimal
			Status                     string
		}
		if err := rows.Scan(&r.Granted, &r.Required, &r.Wagered, &r.Status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestIntegration_PromoGrantCreatesAnObligation: the record exists because the
// grant happened, in the same transaction, so a grant can never exist without
// the wagering requirement attached to it.
func TestIntegration_PromoGrantCreatesAnObligation(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "0.0001") // a token opening balance; the promo is what matters
	grantPromoSC(t, casino, playerID, "100.0000", "25.0000")

	rows := playthroughRows(t, pool, playerID)
	if len(rows) != 1 {
		t.Fatalf("%d playthrough rows, want 1", len(rows))
	}
	r := rows[0]
	if !r.Granted.Equal(decimal.RequireFromString("25")) {
		t.Errorf("granted = %s, want 25", r.Granted)
	}
	// 1x: required equals granted. Stored rather than derived, so a later change
	// to the multiplier cannot retroactively alter this obligation.
	if !r.Required.Equal(r.Granted.Mul(decimal.NewFromInt(PlaythroughMultiplier))) {
		t.Errorf("required = %s, want %dx granted", r.Required, PlaythroughMultiplier)
	}
	if !r.Wagered.IsZero() || r.Status != "OUTSTANDING" {
		t.Errorf("a fresh grant should be OUTSTANDING with 0 wagered, got %s / %s", r.Status, r.Wagered)
	}
}

// TestIntegration_PurchaseWithoutPromoCreatesNoObligation: a GC-only purchase
// grants no sweeps coins, so it must not saddle the player with wagering they
// never accepted.
func TestIntegration_PurchaseWithoutPromoCreatesNoObligation(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "100.0000", "0.0000")

	if rows := playthroughRows(t, pool, playerID); len(rows) != 0 {
		t.Errorf("%d playthrough rows for a GC-only purchase, want 0", len(rows))
	}
	if got := outstandingPlaythrough(t, pool, playerID); !got.IsZero() {
		t.Errorf("outstanding = %s, want 0", got)
	}
}

// TestIntegration_WageringGrantedCoinsDischargesTheObligation is the core loop.
func TestIntegration_WageringGrantedCoinsDischargesTheObligation(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "10.0000", "20.0000")

	if got := outstandingPlaythrough(t, pool, playerID); !got.Equal(decimal.RequireFromString("20")) {
		t.Fatalf("outstanding = %s, want 20", got)
	}

	// Halfway.
	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "pt-1-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("spin: %v", err)
	}
	if got := outstandingPlaythrough(t, pool, playerID); !got.Equal(decimal.RequireFromString("10")) {
		t.Errorf("after wagering 10 of 20, outstanding = %s, want 10", got)
	}

	// And done.
	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "pt-2-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("spin: %v", err)
	}
	if got := outstandingPlaythrough(t, pool, playerID); !got.IsZero() {
		t.Errorf("after wagering the full grant, outstanding = %s, want 0", got)
	}

	rows := playthroughRows(t, pool, playerID)
	if len(rows) != 1 || rows[0].Status != "SATISFIED" {
		t.Errorf("grant should be SATISFIED, got %+v", rows)
	}
}

// TestIntegration_PlaythroughWaterfallsAcrossGrantsOldestFirst pins the FIFO
// allocation the window function implements.
//
// A single wager larger than the oldest grant must fill it completely and spill
// the remainder onto the next — not split evenly, not apply to only one. Getting
// this wrong leaves a player with two half-satisfied grants and no way to redeem
// despite having wagered the full amount.
func TestIntegration_PlaythroughWaterfallsAcrossGrantsOldestFirst(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "10.0000", "10.0000") // grant 1
	grantPromoSC(t, casino, playerID, "10.0000", "30.0000") // grant 2

	if got := outstandingPlaythrough(t, pool, playerID); !got.Equal(decimal.RequireFromString("40")) {
		t.Fatalf("outstanding = %s, want 40", got)
	}

	// 25 in one wager: fills grant 1 (10) and puts 15 against grant 2.
	if _, err := game.ProcessSpin(context.Background(), SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "waterfall-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "25.0000"),
	}); err != nil {
		t.Fatalf("spin: %v", err)
	}

	rows := playthroughRows(t, pool, playerID)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[0].Status != "SATISFIED" || !rows[0].Wagered.Equal(rows[0].Required) {
		t.Errorf("the OLDEST grant must be filled first: %+v", rows[0])
	}
	if rows[1].Status != "OUTSTANDING" || !rows[1].Wagered.Equal(decimal.RequireFromString("15")) {
		t.Errorf("the remainder must spill onto the next grant: %+v", rows[1])
	}
	if got := outstandingPlaythrough(t, pool, playerID); !got.Equal(decimal.RequireFromString("15")) {
		t.Errorf("outstanding = %s, want 15", got)
	}
}

// TestIntegration_WageringRedeemableDoesNotDischargeAGrant is the rule that
// makes the requirement mean anything.
//
// SC_REDEEMABLE is money already won and already played through once. If
// wagering it counted, a player with a redeemable balance could discharge every
// new grant without ever staking the granted coins — the requirement would be
// satisfied by history rather than by play.
func TestIntegration_WageringRedeemableDoesNotDischargeAGrant(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "0.0001")

	// Give the player a redeemable balance the honest way: grant, wager it
	// through, win it back as SC_REDEEMABLE.
	grantPromoSC(t, casino, playerID, "10.0000", "10.0000")
	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "seed-bet-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("seed bet: %v", err)
	}
	if _, err := eng.ProcessWin(ctx, WinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "seed-win-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "50.0000"),
	}); err != nil {
		t.Fatalf("seed win: %v", err)
	}
	if got := outstandingPlaythrough(t, pool, playerID); !got.IsZero() {
		t.Fatalf("the first grant should be satisfied, outstanding = %s", got)
	}

	// New grant, then wager entirely out of the redeemable balance by staking
	// more than the new grant provides — SC_UNPLAYED drains first, so only that
	// portion may count.
	grantPromoSC(t, casino, playerID, "10.0000", "5.0000")
	if got := outstandingPlaythrough(t, pool, playerID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("outstanding after the new grant = %s, want 5", got)
	}

	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "mixed-bet-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "30.0000"),
	}); err != nil {
		t.Fatalf("mixed bet: %v", err)
	}

	// 5 of the 30 came from SC_UNPLAYED and discharged the grant; the other 25
	// came from SC_REDEEMABLE and discharged nothing. The grant is satisfied by
	// exactly its own 5 — not over-credited by the redeemable portion.
	rows := playthroughRows(t, pool, playerID)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[1].Status != "SATISFIED" {
		t.Errorf("the SC_UNPLAYED portion should have discharged the grant: %+v", rows[1])
	}
	if !rows[1].Wagered.Equal(decimal.RequireFromString("5")) {
		t.Errorf("wagered = %s, want exactly the granted 5 — the redeemable "+
			"portion of the stake must not be credited", rows[1].Wagered)
	}
	assertLedgerReconciles(t, pool, playerID)
}

// TestIntegration_RedemptionBlockedWhilePlaythroughOutstanding is the gate.
func TestIntegration_RedemptionBlockedWhilePlaythroughOutstanding(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "10.0000", "10.0000")

	// Give them a redeemable balance WITHOUT discharging the grant, by winning
	// on a wager funded from the grant but smaller than it.
	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "gate-bet-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "4.0000"),
	}); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := eng.ProcessWin(ctx, WinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "gate-win-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "20.0000"),
	}); err != nil {
		t.Fatalf("win: %v", err)
	}

	_, _, scR := walletBalances(t, pool, playerID)
	if !scR.GreaterThan(decimal.RequireFromString("5")) {
		t.Fatalf("setup: expected a redeemable balance, got %s", scR)
	}

	// The money is there and the redemption is still refused — and the error
	// must say WHY, not blame the balance.
	_, err := casino.ProcessRedeem(ctx, RedeemRequest{
		OperatorCode: "OP1", OperatorTransactionID: "gate-redeem-" + uuid.NewString(),
		PlayerID: playerID, Amount: mustMoney(t, "5.0000"),
	})
	if !errors.Is(err, errs.ErrPlaythroughOutstanding) {
		t.Fatalf("got %v, want ErrPlaythroughOutstanding", err)
	}
	if errors.Is(err, errs.ErrInsufficientFunds) {
		t.Fatal("reported as insufficient funds; the balance is present and that would misdirect the player")
	}

	// Nothing moved.
	_, _, scRAfter := walletBalances(t, pool, playerID)
	if !scRAfter.Equal(scR) {
		t.Errorf("a refused redemption moved money: %s → %s", scR, scRAfter)
	}
}

// TestIntegration_RedemptionAllowedOncePlaythroughSatisfied closes the loop: the
// gate is a requirement, not a wall.
func TestIntegration_RedemptionAllowedOncePlaythroughSatisfied(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "10.0000", "10.0000")

	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "ok-bet-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := eng.ProcessWin(ctx, WinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "ok-win-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "30.0000"),
	}); err != nil {
		t.Fatalf("win: %v", err)
	}
	if got := outstandingPlaythrough(t, pool, playerID); !got.IsZero() {
		t.Fatalf("outstanding = %s, want 0", got)
	}

	if _, err := casino.ProcessRedeem(ctx, RedeemRequest{
		OperatorCode: "OP1", OperatorTransactionID: "ok-redeem-" + uuid.NewString(),
		PlayerID: playerID, Amount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("redemption must be permitted once playthrough is satisfied: %v", err)
	}
	assertLedgerReconciles(t, pool, playerID)
}

// TestIntegration_NewGrantReimposesPlaythrough states the strict reading
// explicitly, because it is a product decision someone will question.
//
// Accepting new promotional coins accepts new wagering with them, and that holds
// back redemption even of a balance earned under an already-satisfied grant.
// Without it, repeated small purchases would wash promotional value straight
// back out as cash.
func TestIntegration_NewGrantReimposesPlaythrough(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "10.0000", "10.0000")
	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "re-bet-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := eng.ProcessWin(ctx, WinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "re-win-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "40.0000"),
	}); err != nil {
		t.Fatalf("win: %v", err)
	}

	// Redemption is open at this point.
	if _, err := casino.ProcessRedeem(ctx, RedeemRequest{
		OperatorCode: "OP1", OperatorTransactionID: "re-redeem-1-" + uuid.NewString(),
		PlayerID: playerID, Amount: mustMoney(t, "5.0000"),
	}); err != nil {
		t.Fatalf("first redemption: %v", err)
	}

	// A new grant closes it again.
	grantPromoSC(t, casino, playerID, "10.0000", "15.0000")
	_, err := casino.ProcessRedeem(ctx, RedeemRequest{
		OperatorCode: "OP1", OperatorTransactionID: "re-redeem-2-" + uuid.NewString(),
		PlayerID: playerID, Amount: mustMoney(t, "5.0000"),
	})
	if !errors.Is(err, errs.ErrPlaythroughOutstanding) {
		t.Errorf("a fresh grant must re-impose the requirement: got %v", err)
	}
}

// TestIntegration_ConcurrentWagersCannotOverDischargeAGrant: the waterfall runs
// under the wallet lock, so simultaneous wagers each see the progress the last
// one committed rather than all crediting against the same remaining balance.
//
// Over-discharge would be invisible without this test — the counter is capped by
// a CHECK constraint, so the symptom of a race is not a wrong number but a
// failed transaction under load.
func TestIntegration_ConcurrentWagersCannotOverDischargeAGrant(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)

	playerID := seedPlayer(t, pool, "0.0001")
	grantPromoSC(t, casino, playerID, "10.0000", "50.0000")

	const concurrency = 20
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := game.ProcessSpin(context.Background(), SpinRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: fmt.Sprintf("pt-race-%s-%d", uuid.NewString(), i),
				PlayerID:              playerID,
				Family:                domain.FamilySC,
				BetAmount:             mustMoney(t, "5.0000"),
			})
			if err != nil && !errors.Is(err, errs.ErrInsufficientFunds) {
				errCh <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("unexpected error under concurrency: %v", err)
	}

	rows := playthroughRows(t, pool, playerID)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	// The constraint caps wagered at required; the point is that it was never
	// VIOLATED, which is what a race would have done.
	if rows[0].Wagered.GreaterThan(rows[0].Required) {
		t.Errorf("wagered %s exceeds required %s", rows[0].Wagered, rows[0].Required)
	}
	if got := outstandingPlaythrough(t, pool, playerID); got.IsNegative() {
		t.Errorf("outstanding went negative: %s", got)
	}
	assertLedgerReconciles(t, pool, playerID)
}

// TestIntegration_PlaythroughGrantIsIdempotentPerLedgerTransaction: one grant,
// one obligation. The purchase path has its own Ghost-Spin recovery, so a
// retried purchase can re-enter with the same ledger transaction — and must not
// leave the player owing twice the wagering off a single grant.
func TestIntegration_PlaythroughGrantIsIdempotentPerLedgerTransaction(t *testing.T) {
	pool := integrationPool(t)
	playerID := seedPlayer(t, pool, "0.0001")
	ledgerTxID := uuid.New()

	insert := func() error {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO sc_playthrough (player_id, ledger_transaction_id, granted_amount, required_amount)
			VALUES ($1, $2, 10.0000, 10.0000)
			ON CONFLICT (ledger_transaction_id) DO NOTHING`, playerID, ledgerTxID)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err != nil {
		t.Fatalf("replay must be a no-op, not an error: %v", err)
	}

	if rows := playthroughRows(t, pool, playerID); len(rows) != 1 {
		t.Errorf("%d rows for one ledger transaction, want 1", len(rows))
	}
}
