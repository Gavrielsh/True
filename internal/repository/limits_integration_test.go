//go:build integration

package repository

// limits_integration_test.go proves the one property a limit cannot have
// without a database: that it holds under concurrency.
//
// The rules themselves — does a stake fit, is a change an increase — are pure
// and covered exhaustively in limits_test.go. What is proven here is that the
// check and the consumption are ONE serialized observation, so twenty
// simultaneous wagers cannot each read the same stale total and each decide they
// fit. That is the failure a limit exists to prevent, and it is invisible to any
// test that runs one request at a time.
//
//	TEST_POSTGRES_URL=postgres://postgres@127.0.0.1:5433/true_engine?sslmode=disable \
//	    go test -tags integration ./internal/repository/ -run Integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// setLimit installs a limit directly, bypassing ProcessSetPlayerLimit.
//
// Used as ARRANGE by the enforcement tests so they exercise one thing each: a
// test about whether a cap holds under load should not also depend on the
// cooling-off classifier agreeing with it. The write path has its own tests
// below.
func setLimitDirect(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID, kind, period, amount string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO player_limits (player_id, limit_kind, period, amount)
		VALUES ($1, $2::limit_kind, $3::limit_period, $4::numeric)
		ON CONFLICT (player_id, limit_kind, period)
		DO UPDATE SET amount = EXCLUDED.amount, pending_amount = NULL, pending_effective_at = NULL`,
		playerID, kind, period, amount); err != nil {
		t.Fatalf("seed limit: %v", err)
	}
}

func limitUsage(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID, kind, period string) decimal.Decimal {
	t.Helper()
	var used decimal.Decimal
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(used), 0) FROM player_limit_usage
		WHERE player_id = $1 AND limit_kind = $2::limit_kind AND period = $3::limit_period`,
		playerID, kind, period).Scan(&used)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return used
}

func limitRowOf(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID, kind, period string) (amount decimal.Decimal, pending *decimal.Decimal, at *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT amount, pending_amount, pending_effective_at FROM player_limits
		WHERE player_id = $1 AND limit_kind = $2::limit_kind AND period = $3::limit_period`,
		playerID, kind, period).Scan(&amount, &pending, &at)
	if err != nil {
		t.Fatalf("read limit: %v", err)
	}
	return amount, pending, at
}

// ----------------------------------------------------------------------------
// The concurrency proof
// ----------------------------------------------------------------------------

// TestIntegration_ConcurrentSpinsCannotRacePastALimit is the reason this file
// exists.
//
// A player with a 50.0000 daily WAGER limit fires twenty simultaneous 10.0000
// spins. Exactly five may settle. Nineteen of the twenty are competing for a
// decision that depends on what the others have already committed, so if the
// check and the consumption were separate observations — a read on one
// connection, a write on another — every one of them would read 0 consumed, and
// all twenty would settle 200.0000 of turnover against a 50.0000 cap.
//
// The Redis idempotency barrier is disabled (openIdem) precisely so this reaches
// Postgres: what is under test is whether the DATABASE serializes the decision,
// not whether a cache happened to deduplicate it.
func TestIntegration_ConcurrentSpinsCannotRacePastALimit(t *testing.T) {
	pool := integrationPool(t)
	game := newIntegrationGame(pool)

	const (
		concurrency = 20
		stake       = "10.0000"
		limit       = "50.0000"
		wantSettled = 5 // 50 / 10
	)

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, limit)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		settled  int
		refused  int
		otherErr []error
	)
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, so they genuinely contend
			_, err := game.ProcessSpin(context.Background(), SpinRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: fmt.Sprintf("limit-race-%s-%d", uuid.NewString(), i),
				PlayerID:              playerID,
				Family:                domain.FamilySC,
				BetAmount:             mustMoney(t, stake),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				settled++
			case errors.Is(err, errs.ErrLimitExceeded):
				refused++
			default:
				otherErr = append(otherErr, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(otherErr) > 0 {
		t.Fatalf("unexpected errors: %v", otherErr)
	}
	if settled != wantSettled {
		t.Errorf("settled %d spins, want exactly %d — the limit did not serialize", settled, wantSettled)
	}
	if refused != concurrency-wantSettled {
		t.Errorf("refused %d, want %d", refused, concurrency-wantSettled)
	}

	// The invariant that matters more than the count: consumption never exceeded
	// the cap, whatever the interleaving happened to be.
	used := limitUsage(t, pool, playerID, LimitWager, PeriodDaily)
	if used.GreaterThan(decimal.RequireFromString(limit)) {
		t.Errorf("usage %s exceeds the %s limit", used, limit)
	}

	// And the ledger agrees with the counter — the wagers that were refused left
	// no trace on the balance.
	_, scU, _ := walletBalances(t, pool, playerID)
	spent := decimal.RequireFromString("1000.0000").Sub(scU)
	if !spent.Equal(used) {
		t.Errorf("wallet says %s was staked, the usage counter says %s", spent, used)
	}
	assertLedgerReconciles(t, pool, playerID)
}

// TestIntegration_ConcurrentLimitChangeAndSpinsSerialize covers the other race:
// a limit being LOWERED while wagers are in flight.
//
// The decrease must not land halfway through a settling spin, and every spin
// that acquires the lock after it must see the new cap. Both follow from the
// limit write taking the same wallet lock the wager does — which is why
// ProcessSetPlayerLimit takes it despite touching no balance.
func TestIntegration_ConcurrentLimitChangeAndSpinsSerialize(t *testing.T) {
	pool := integrationPool(t)
	game := newIntegrationGame(pool)
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "100.0000")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Drop the cap to zero while spins are running.
		_, _ = casino.ProcessSetPlayerLimit(context.Background(), SetPlayerLimitRequest{
			OperatorCode: "OP1", PlayerID: playerID,
			Kind: LimitWager, Period: PeriodDaily,
			Amount: mustMoney(t, "0.0000"), ActorType: "PLAYER",
		})
	}()

	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = game.ProcessSpin(context.Background(), SpinRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: fmt.Sprintf("limit-change-race-%s-%d", uuid.NewString(), i),
				PlayerID:              playerID,
				Family:                domain.FamilySC,
				BetAmount:             mustMoney(t, "10.0000"),
			})
		}(i)
	}
	wg.Wait()

	// Whatever order they interleaved in, the total staked cannot exceed the
	// HIGHER of the two caps — a decrease can only ever tighten. The precise
	// count is genuinely nondeterministic here and asserting one would be
	// asserting a scheduling accident.
	used := limitUsage(t, pool, playerID, LimitWager, PeriodDaily)
	if used.GreaterThan(decimal.RequireFromString("100.0000")) {
		t.Errorf("usage %s exceeds even the pre-decrease cap of 100.0000", used)
	}
	if used.IsNegative() {
		t.Errorf("usage went negative: %s", used)
	}
	assertLedgerReconciles(t, pool, playerID)

	// The decrease is in force afterwards, so the next wager is refused outright.
	_, err := game.ProcessSpin(context.Background(), SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "after-decrease-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             mustMoney(t, "1.0000"),
	})
	if !errors.Is(err, errs.ErrLimitExceeded) {
		t.Errorf("after a decrease to zero: got %v, want ErrLimitExceeded", err)
	}
}

// ----------------------------------------------------------------------------
// Net-loss accounting
// ----------------------------------------------------------------------------

// TestIntegration_LossLimitCountsNetNotTurnover proves LOSS measures what it
// claims to.
//
// A losing spin consumes its full stake. A WIN gives the headroom back, because
// a player who is level has lost nothing — if LOSS counted turnover it would be
// a second wager limit under a misleading name, and a player on a break-even
// streak would be locked out having lost nothing at all.
func TestIntegration_LossLimitCountsNetNotTurnover(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitLoss, PeriodDaily, "100.0000")

	bet := func(amount string) error {
		_, err := eng.ProcessBet(ctx, BetRequest{
			OperatorCode:          "OP1",
			OperatorTransactionID: "loss-bet-" + uuid.NewString(),
			PlayerID:              playerID,
			Family:                domain.FamilySC,
			Amount:                mustMoney(t, amount),
		})
		return err
	}
	win := func(amount string) error {
		_, err := eng.ProcessWin(ctx, WinRequest{
			OperatorCode:          "OP1",
			OperatorTransactionID: "loss-win-" + uuid.NewString(),
			PlayerID:              playerID,
			Family:                domain.FamilySC,
			Amount:                mustMoney(t, amount),
		})
		return err
	}

	if err := bet("60.0000"); err != nil {
		t.Fatalf("first bet: %v", err)
	}
	if got := limitUsage(t, pool, playerID, LimitLoss, PeriodDaily); !got.Equal(decimal.RequireFromString("60")) {
		t.Fatalf("after a 60 stake, loss usage = %s, want 60", got)
	}

	// A 60 win returns the player to level: nothing has been lost.
	if err := win("60.0000"); err != nil {
		t.Fatalf("win: %v", err)
	}
	if got := limitUsage(t, pool, playerID, LimitLoss, PeriodDaily); !got.IsZero() {
		t.Errorf("after winning the stake back, loss usage = %s, want 0", got)
	}

	// Turnover is now 60 and would have consumed 60 of a wager limit — but the
	// loss limit is untouched, so a further 100 stake still fits exactly.
	if err := bet("100.0000"); err != nil {
		t.Errorf("a break-even player must still have their full loss limit: %v", err)
	}
	if err := bet("0.0001"); !errors.Is(err, errs.ErrLimitExceeded) {
		t.Errorf("having now lost 100, the next stake must be refused: got %v", err)
	}

	// WAGER is untouched by the win: a win is not a wager.
	if got := limitUsage(t, pool, playerID, LimitWager, PeriodDaily); !got.IsZero() {
		t.Errorf("no WAGER limit is set, so no WAGER usage should be recorded: %s", got)
	}
	assertLedgerReconciles(t, pool, playerID)
}

// TestIntegration_WinIsNeverRefusedByALimit: limits bind what a player may
// STAKE. Refusing to credit a win they have already earned because it would
// breach a cap would be taking their money, not protecting them.
func TestIntegration_WinIsNeverRefusedByALimit(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "10.0000")
	setLimitDirect(t, pool, playerID, LimitLoss, PeriodDaily, "10.0000")

	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "win-allowed-bet-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("bet: %v", err)
	}

	// Far larger than either cap, and it must still be credited.
	if _, err := eng.ProcessWin(ctx, WinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "win-allowed-win-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "500.0000"),
	}); err != nil {
		t.Fatalf("a win must never be refused by a limit: %v", err)
	}
	assertLedgerReconciles(t, pool, playerID)
}

// TestIntegration_LimitsAreScopedToTheirPeriod: a daily bucket must not consume
// the weekly one, and usage recorded under one period must not be visible to
// another. Both follow from limit_period_start being the single definition of a
// bucket boundary, shared by the read and the write.
func TestIntegration_LimitsAreScopedToTheirPeriod(t *testing.T) {
	pool := integrationPool(t)
	game := newIntegrationGame(pool)

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "20.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodWeekly, "1000.0000")

	for i := 0; i < 2; i++ {
		if _, err := game.ProcessSpin(context.Background(), SpinRequest{
			OperatorCode:          "OP1",
			OperatorTransactionID: fmt.Sprintf("period-%s-%d", uuid.NewString(), i),
			PlayerID:              playerID,
			Family:                domain.FamilySC,
			BetAmount:             mustMoney(t, "10.0000"),
		}); err != nil {
			t.Fatalf("spin %d: %v", i, err)
		}
	}

	// Both buckets moved by the same amount, independently.
	for _, period := range []string{PeriodDaily, PeriodWeekly} {
		if got := limitUsage(t, pool, playerID, LimitWager, period); !got.Equal(decimal.RequireFromString("20")) {
			t.Errorf("%s usage = %s, want 20", period, got)
		}
	}

	// The daily cap is spent; the weekly one is nowhere near. The tighter one
	// must be what refuses the next wager.
	_, err := game.ProcessSpin(context.Background(), SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "period-over-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             mustMoney(t, "1.0000"),
	})
	if !errors.Is(err, errs.ErrLimitExceeded) {
		t.Errorf("the daily cap must bind even with weekly headroom: got %v", err)
	}
}

// TestIntegration_NoLimitsMeansNoUsageRows: the cost of the control for a player
// who has not set one is a single SELECT that returns nothing — no rows written,
// no buckets created.
func TestIntegration_NoLimitsMeansNoUsageRows(t *testing.T) {
	pool := integrationPool(t)
	game := newIntegrationGame(pool)

	playerID := seedPlayer(t, pool, "100.0000")
	if _, err := game.ProcessSpin(context.Background(), SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "no-limits-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("spin: %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM player_limit_usage WHERE player_id = $1`, playerID).Scan(&n); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	if n != 0 {
		t.Errorf("%d usage rows written for a player with no limits, want 0", n)
	}
}

// ----------------------------------------------------------------------------
// The cooling-off asymmetry
// ----------------------------------------------------------------------------

// TestIntegration_LimitDecreaseIsImmediate: the protective direction never
// waits. A player lowering their cap mid-session is protected by the very next
// wager, not tomorrow.
func TestIntegration_LimitDecreaseIsImmediate(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "100.0000")

	res, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID,
		Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "5.0000"), ActorType: "PLAYER", ActorRef: "rg-settings",
	})
	if err != nil {
		t.Fatalf("decrease: %v", err)
	}
	if res.Direction != string(limitDirectionDecrease) {
		t.Errorf("direction = %s, want DECREASE", res.Direction)
	}
	if res.EffectiveAt != nil || res.PendingAmount != nil {
		t.Errorf("a decrease must not be deferred: effective_at=%v pending=%v", res.EffectiveAt, res.PendingAmount)
	}
	if res.Amount.String() != "5.0000" {
		t.Errorf("amount in force = %s, want 5.0000", res.Amount)
	}

	amount, pending, at := limitRowOf(t, pool, playerID, LimitWager, PeriodDaily)
	if !amount.Equal(decimal.RequireFromString("5")) {
		t.Errorf("stored amount = %s, want 5", amount)
	}
	if pending != nil || at != nil {
		t.Errorf("a decrease left a pending value: %v @ %v", pending, at)
	}

	// The next wager is bound by the new cap immediately.
	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "dec-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "10.0000"),
	}); !errors.Is(err, errs.ErrLimitExceeded) {
		t.Errorf("a 10 stake against a just-lowered 5 cap: got %v, want ErrLimitExceeded", err)
	}
}

// TestIntegration_LimitIncreaseServesTheCoolingOffPeriod is the other half, and
// the one a player will try to get around.
//
// The requested value is parked; the OLD cap keeps binding. A player mid-session
// who raises their limit gets nothing today — which is the entire point of the
// control, and the thing that would be silently lost if the increase were
// written straight to `amount`.
func TestIntegration_LimitIncreaseServesTheCoolingOffPeriod(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "10.0000")

	before := time.Now().UTC()
	res, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID,
		Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "500.0000"), ActorType: "PLAYER",
	})
	if err != nil {
		t.Fatalf("increase: %v", err)
	}

	if res.Direction != string(limitDirectionIncrease) {
		t.Fatalf("direction = %s, want INCREASE", res.Direction)
	}
	// The response must report the cap ACTUALLY in force, not the one requested.
	// A client that rendered the requested value would tell the player they had
	// 500 to spend when they have 10.
	if res.Amount.String() != "10.0000" {
		t.Errorf("amount in force = %s, want the OLD cap 10.0000", res.Amount)
	}
	if res.PendingAmount == nil || res.PendingAmount.String() != "500.0000" {
		t.Errorf("pending = %v, want 500.0000", res.PendingAmount)
	}
	if res.EffectiveAt == nil {
		t.Fatal("an increase must carry an effective time")
	}
	if delta := res.EffectiveAt.Sub(before.Add(LimitIncreaseCoolOff)); delta > time.Minute || delta < -time.Minute {
		t.Errorf("effective_at %v is not ~%v from now (delta %v)", res.EffectiveAt, LimitIncreaseCoolOff, delta)
	}

	// The old cap still binds.
	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "inc-blocked-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "50.0000"),
	}); !errors.Is(err, errs.ErrLimitExceeded) {
		t.Errorf("a 50 stake during the cooling-off period: got %v, want ErrLimitExceeded", err)
	}

	// And a stake inside the OLD cap still works — the increase request must not
	// have disturbed the limit that is actually in force.
	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "inc-allowed-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Errorf("a stake within the old cap must still settle: %v", err)
	}
}

// TestIntegration_PendingIncreaseAppliesOnceItsDeadlinePasses closes the loop:
// the cooling-off period is a delay, not a refusal.
//
// The deadline is moved into the past rather than waiting 24 hours. That is
// legitimate here because the mechanism under test is the CASE expression in
// sqlSelectEffectiveLimits — "does a pending value become effective when its
// time comes" — and the clock it reads is the database's.
func TestIntegration_PendingIncreaseAppliesOnceItsDeadlinePasses(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "10.0000")

	if _, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID,
		Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "500.0000"), ActorType: "PLAYER",
	}); err != nil {
		t.Fatalf("increase: %v", err)
	}

	// Fast-forward past the deadline.
	if _, err := pool.Exec(ctx, `
		UPDATE player_limits SET pending_effective_at = now() - interval '1 second'
		WHERE player_id = $1 AND limit_kind = 'WAGER' AND period = 'DAILY'`, playerID); err != nil {
		t.Fatalf("advance the deadline: %v", err)
	}

	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "pending-applied-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "50.0000"),
	}); err != nil {
		t.Errorf("once the cooling-off period has passed the new cap must apply: %v", err)
	}
}

// TestIntegration_DecreaseClearsAPendingIncrease is the interaction that is easy
// to miss and bad to get wrong.
//
// A player who requests an increase and then thinks better of it — lowering
// their cap the same evening — must not have the increase they abandoned land on
// them a day later. The decrease clears the pending value on its way past.
func TestIntegration_DecreaseClearsAPendingIncrease(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")
	setLimitDirect(t, pool, playerID, LimitWager, PeriodDaily, "50.0000")

	if _, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID, Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "500.0000"), ActorType: "PLAYER",
	}); err != nil {
		t.Fatalf("increase: %v", err)
	}
	if _, _, at := limitRowOf(t, pool, playerID, LimitWager, PeriodDaily); at == nil {
		t.Fatal("the increase was not parked")
	}

	if _, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID, Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "20.0000"), ActorType: "PLAYER",
	}); err != nil {
		t.Fatalf("decrease: %v", err)
	}

	amount, pending, at := limitRowOf(t, pool, playerID, LimitWager, PeriodDaily)
	if !amount.Equal(decimal.RequireFromString("20")) {
		t.Errorf("amount = %s, want 20", amount)
	}
	if pending != nil || at != nil {
		t.Errorf("the abandoned increase survived the decrease: %v @ %v — it would land tomorrow", pending, at)
	}
}

// TestIntegration_FirstLimitAppliesImmediately: a player who has never set a
// limit is protected by nothing, so making their first one wait would leave them
// unprotected for a day as a direct consequence of choosing to be protected.
func TestIntegration_FirstLimitAppliesImmediately(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "1000.0000")

	res, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID, Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "5.0000"), ActorType: "PLAYER",
	})
	if err != nil {
		t.Fatalf("first limit: %v", err)
	}
	if res.Direction != string(limitDirectionSet) {
		t.Errorf("direction = %s, want SET", res.Direction)
	}
	if res.EffectiveAt != nil {
		t.Errorf("a first limit must not be deferred: %v", res.EffectiveAt)
	}

	if _, err := game.ProcessSpin(ctx, SpinRequest{
		OperatorCode: "OP1", OperatorTransactionID: "first-limit-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "10.0000"),
	}); !errors.Is(err, errs.ErrLimitExceeded) {
		t.Errorf("the first limit must bind at once: got %v", err)
	}
}

// TestIntegration_RestatingTheSameLimitIsRefused: re-submitting the value
// already in force is not a decision and must not be recorded as one, or the
// audit trail fills with events that never happened.
func TestIntegration_RestatingTheSameLimitIsRefused(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "100.0000")
	setLimitDirect(t, pool, playerID, LimitLoss, PeriodWeekly, "50.0000")

	var before int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM player_limit_changes WHERE player_id = $1`, playerID).Scan(&before); err != nil {
		t.Fatalf("count: %v", err)
	}

	_, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID, Kind: LimitLoss, Period: PeriodWeekly,
		Amount: mustMoney(t, "50.0000"), ActorType: "PLAYER",
	})
	if !errors.Is(err, errs.ErrStatusUnchanged) {
		t.Fatalf("got %v, want ErrStatusUnchanged", err)
	}

	var after int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM player_limit_changes WHERE player_id = $1`, playerID).Scan(&after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("a no-op wrote %d audit rows", after-before)
	}
}

// TestIntegration_LimitChangeIsAudited: the change row IS the evidence that the
// cooling-off period was served, so its contents are asserted rather than its
// existence.
func TestIntegration_LimitChangeIsAudited(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "100.0000")
	setLimitDirect(t, pool, playerID, LimitLoss, PeriodMonthly, "200.0000")

	res, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID, Kind: LimitLoss, Period: PeriodMonthly,
		Amount: mustMoney(t, "900.0000"), ActorType: "PLAYER", ActorRef: "rg-page",
	})
	if err != nil {
		t.Fatalf("increase: %v", err)
	}

	var (
		prev, requested decimal.Decimal
		direction       string
		effectiveAt     *time.Time
		actorType       string
		actorRef        *string
	)
	if err := pool.QueryRow(ctx, `
		SELECT previous_amount, requested_amount, direction, effective_at, actor_type, actor_ref
		FROM player_limit_changes WHERE id = $1`, res.ChangeID,
	).Scan(&prev, &requested, &direction, &effectiveAt, &actorType, &actorRef); err != nil {
		t.Fatalf("read audit row: %v", err)
	}

	if !prev.Equal(decimal.RequireFromString("200")) || !requested.Equal(decimal.RequireFromString("900")) {
		t.Errorf("amounts: previous=%s requested=%s", prev, requested)
	}
	if direction != "INCREASE" {
		t.Errorf("direction = %s", direction)
	}
	if effectiveAt == nil {
		t.Error("an INCREASE must record when it becomes effective — that is the proof the delay was served")
	}
	if actorType != "PLAYER" || actorRef == nil || *actorRef != "rg-page" {
		t.Errorf("actor: %s / %v", actorType, actorRef)
	}
}

// TestIntegration_LimitAuditLogIsAppendOnly: the evidence cannot be revised
// after the fact, including by the table owner.
func TestIntegration_LimitAuditLogIsAppendOnly(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "100.0000")
	if _, err := casino.ProcessSetPlayerLimit(ctx, SetPlayerLimitRequest{
		OperatorCode: "OP1", PlayerID: playerID, Kind: LimitWager, Period: PeriodDaily,
		Amount: mustMoney(t, "25.0000"), ActorType: "PLAYER",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	for _, m := range []struct{ name, sql string }{
		{"UPDATE", `UPDATE player_limit_changes SET direction = 'DECREASE'`},
		{"DELETE", `DELETE FROM player_limit_changes`},
		{"TRUNCATE", `TRUNCATE player_limit_changes`},
	} {
		t.Run(m.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, m.sql)
			if err == nil {
				t.Fatalf("%s SUCCEEDED; the limit audit log must be append-only", m.name)
			}
			if got := sqlStateRepo(err); got != "0A000" {
				t.Errorf("%s: SQLSTATE %q, want 0A000 (err: %v)", m.name, got, err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Self-exclusion extension (the gap A2 shipped with)
// ----------------------------------------------------------------------------

// TestIntegration_SelfExclusionCanBeExtended closes it. A player serving a term
// may always ask for longer — the direction that deepens protection was, until
// now, the only one they could not take.
func TestIntegration_SelfExclusionCanBeExtended(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	originalEnd := time.Now().UTC().Add(200 * 24 * time.Hour)
	forceSelfExclusion(t, pool, playerID, originalEnd)

	res, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode:      "OP1",
		PlayerID:          playerID,
		ToStatus:          StatusSelfExcluded,
		ActorType:         "PLAYER",
		Reason:            "player asked to extend their exclusion",
		SelfExclusionDays: 365,
	})
	if err != nil {
		t.Fatalf("extension must be permitted: %v", err)
	}
	if res.FromStatus != StatusSelfExcluded || res.ToStatus != StatusSelfExcluded {
		t.Errorf("result reports %s → %s", res.FromStatus, res.ToStatus)
	}

	status, until := playerStatus(t, pool, playerID)
	if status != StatusSelfExcluded {
		t.Errorf("status = %s", status)
	}
	if until == nil || !until.After(originalEnd) {
		t.Errorf("term = %v, want later than the original %v", until, originalEnd)
	}

	// The extension is recorded like any other transition — the audit trail must
	// show the player deepened their own protection.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM player_status_transitions
		WHERE player_id = $1 AND from_status = 'SELF_EXCLUDED' AND to_status = 'SELF_EXCLUDED'`,
		playerID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d extension rows recorded, want 1", n)
	}
}

// TestIntegration_SelfExclusionCannotBeShortenedViaExtension is the guard on the
// guard.
//
// A player 200 days into a long term who requests "180 days" is asking for a
// term that ends SOONER. A floor check alone would admit it — 180 clears the
// 180-day minimum — and an exclusion would have been quietly shortened by a
// request that looks like a renewal.
func TestIntegration_SelfExclusionCannotBeShortenedViaExtension(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	originalEnd := time.Now().UTC().Add(300 * 24 * time.Hour)
	forceSelfExclusion(t, pool, playerID, originalEnd)

	_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode:      "OP1",
		PlayerID:          playerID,
		ToStatus:          StatusSelfExcluded,
		ActorType:         "PLAYER",
		Reason:            "a shorter term dressed as a renewal",
		SelfExclusionDays: MinSelfExclusionDays, // clears the floor, but ends sooner
	})
	if !errors.Is(err, errs.ErrSelfExclusionNotExtended) {
		t.Fatalf("got %v, want ErrSelfExclusionNotExtended", err)
	}

	_, until := playerStatus(t, pool, playerID)
	if until == nil || !until.Equal(originalEnd.Truncate(time.Microsecond)) {
		// Postgres stores microsecond precision; compare at that resolution.
		if until == nil || until.Sub(originalEnd).Abs() > time.Millisecond {
			t.Errorf("term moved to %v, want the original %v", until, originalEnd)
		}
	}
}
