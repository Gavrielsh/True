//go:build integration

package repository

// rg_integration_test.go proves the concurrency property PLAN.md item 6
// requires: limits are checked inside the SAME transaction that locks the
// wallet, so a concurrent spin cannot slip past a loss limit. That guarantee
// comes from Postgres row-level locking (the wallets FOR UPDATE every
// money-moving flow already takes — see rg.go's file header), which pgxmock
// cannot simulate, so this runs against a real database like the rest of
// this package's *_integration_test.go suite.
//
// Run with:
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

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// TestIntegration_ConcurrentSpinsCannotExceedLossLimit fires 100 concurrent,
// distinct spins for one player against a LOSS_LIMIT that admits exactly one
// of them. Each spin uses a private losingRNG (see settle_integration_test.go
// for why a shared one would manufacture false wins), so every successful
// spin loses its stake in full — the counter therefore advances by exactly
// one bet's worth per success, never less.
//
// Exactly one spin must succeed. The rest must be refused as RG_RESTRICTED,
// and the accumulated LOSS counter must never exceed the limit.
func TestIntegration_ConcurrentSpinsCannotExceedLossLimit(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()

	const (
		goroutines = 100
		betAmount  = "10.0000"
		lossLimit  = "10.0000" // admits exactly one 10.0000 bet per DAY window
	)

	playerID := seedPlayer(t, pool, "100000.0000")

	rg := NewResponsibleGaming(pool, openIdem{}, discardLoggerRepo())
	if _, err := rg.SetLimit(ctx, SetLimitRequest{
		PlayerID:  playerID,
		LimitType: "LOSS_LIMIT",
		Active:    true,
		Period:    "DAY",
		Amount:    moneyPtr(t, lossLimit),
		EventID:   "rg-concurrency-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}

	// One engine per goroutine, each with a private losingRNG — see
	// newIntegrationGame's doc for why sharing one across concurrent spins
	// would let interleaved draws land a winning combination.
	engines := make([]GameEngine, goroutines)
	for i := range engines {
		engines[i] = newIntegrationGame(pool)
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
		other     []error
	)

	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int, eng GameEngine) {
			defer wg.Done()
			<-start
			_, err := eng.ProcessSpin(ctx, SpinRequest{
				OperatorCode:          "OP1",
				OperatorTransactionID: fmt.Sprintf("rg-loss-limit-%s-%d", uuid.NewString(), n),
				PlayerID:              playerID,
				Family:                domain.FamilySC,
				BetAmount:             mustMoney(t, betAmount),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, errs.ErrResponsibleGamingRestricted):
				refused++
			default:
				other = append(other, err)
			}
		}(i, engines[i])
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Errorf("%d attempts failed unexpectedly; first: %v", len(other), other[0])
	}
	if succeeded != 1 {
		t.Errorf("LOSS LIMIT BREACHED: exactly one spin must succeed, got %d (refused=%d)", succeeded, refused)
	}
	if refused != goroutines-1 {
		t.Errorf("expected %d RG_RESTRICTED refusals, got %d", goroutines-1, refused)
	}

	summary, err := rg.QueryLimits(ctx, playerID)
	if err != nil {
		t.Fatalf("QueryLimits: %v", err)
	}
	if summary.LossLimit == nil {
		t.Fatal("LossLimit: got nil, want the limit set above")
	}
	if summary.LossLimit.Used.Decimal().GreaterThan(mustMoney(t, lossLimit).Decimal()) {
		t.Errorf("LOSS LIMIT BREACHED: accumulated %s exceeds limit %s", summary.LossLimit.Used, lossLimit)
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}

// TestIntegration_GCExemptFromSCLossLimitAndSelfExclusion proves the two
// halves of the SC-only LOSS_LIMIT fix that pgxmock cannot validate on its
// own (it never runs the real SQL currency filter — see sqlRGLossAggregate):
//
//  1. A GC spin is allowed even after an SC LOSS_LIMIT is fully exhausted —
//     GC has no real value, so it must never be gated by, or count toward,
//     a real-money loss limit.
//  2. A GC spin is still refused once the player is self-excluded — that
//     guard is currency-family agnostic and must never be skipped.
func TestIntegration_GCExemptFromSCLossLimitAndSelfExclusion(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()

	const (
		scBetAmount = "10.0000"
		lossLimit   = "10.0000" // admits exactly one 10.0000 SC bet per DAY window
		gcBetAmount = "500.0000"
	)

	playerID := seedPlayer(t, pool, scBetAmount)
	creditOpeningBalance(t, pool, playerID, "GC", "10000.0000")

	rg := NewResponsibleGaming(pool, openIdem{}, discardLoggerRepo())
	if _, err := rg.SetLimit(ctx, SetLimitRequest{
		PlayerID:  playerID,
		LimitType: "LOSS_LIMIT",
		Active:    true,
		Period:    "DAY",
		Amount:    moneyPtr(t, lossLimit),
		EventID:   "rg-gc-exempt-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("SetLimit LOSS_LIMIT: %v", err)
	}

	eng := newIntegrationGame(pool)

	// Exhaust the SC loss limit with one losing SC spin.
	if _, err := eng.ProcessSpin(ctx, SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "rg-gc-exempt-sc-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             mustMoney(t, scBetAmount),
	}); err != nil {
		t.Fatalf("initial SC spin: %v", err)
	}

	// A second SC spin must now be refused: the limit is exhausted.
	_, err := eng.ProcessSpin(ctx, SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "rg-gc-exempt-sc2-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilySC,
		BetAmount:             mustMoney(t, scBetAmount),
	})
	if !errors.Is(err, errs.ErrResponsibleGamingRestricted) {
		t.Fatalf("SC spin after exhausting LOSS_LIMIT: got %v, want ErrResponsibleGamingRestricted", err)
	}

	// A GC spin, in contrast, must succeed — GC never counts toward, or is
	// gated by, an SC LOSS_LIMIT, however exhausted.
	if _, err := eng.ProcessSpin(ctx, SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "rg-gc-exempt-gc-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		BetAmount:             mustMoney(t, gcBetAmount),
	}); err != nil {
		t.Fatalf("GC spin must be exempt from the SC LOSS_LIMIT: %v", err)
	}

	// Now self-exclude the player — a currency-agnostic guard that must
	// still catch a GC spin even though the LOSS_LIMIT guard skips it.
	if _, err := rg.SetLimit(ctx, SetLimitRequest{
		PlayerID:  playerID,
		LimitType: "SELF_EXCLUSION",
		Active:    true,
		EndsAt:    ptrTime(time.Now().Add(24 * time.Hour)),
		EventID:   "rg-gc-exempt-excl-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("SetLimit SELF_EXCLUSION: %v", err)
	}

	_, err = eng.ProcessSpin(ctx, SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "rg-gc-exempt-gc2-" + uuid.NewString(),
		PlayerID:              playerID,
		Family:                domain.FamilyGC,
		BetAmount:             mustMoney(t, gcBetAmount),
	})
	if !errors.Is(err, errs.ErrResponsibleGamingRestricted) {
		t.Fatalf("GC spin during self-exclusion: got %v, want ErrResponsibleGamingRestricted", err)
	}

	assertLedgerReconciles(t, pool, playerID)
	assertDoubleEntryBalanced(t, pool)
}
