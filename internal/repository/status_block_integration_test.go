//go:build integration

package repository

// status_block_integration_test.go is the Phase A closing argument: SUSPENDED
// and SELF_EXCLUDED block EVERY money path, immediately, under concurrent load.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS SUITE EXISTS SEPARATELY FROM THE PER-PATH TESTS
// ─────────────────────────────────────────────────────────────────────────────
// requirePlayerActive is called from five places, and each call site has its own
// coverage. What none of those prove is the property that actually matters at
// the perimeter: that there is no path — purchase, wager, spin or redeem — a
// blocked player can still reach, and that the block takes hold at the moment it
// commits rather than at the next convenient boundary.
//
// The dangerous failure is not "the guard is missing from route X". It is "the
// guard is present on every route, and a request already in flight settled
// anyway" — which is invisible to a sequential test, because sequentially there
// is no in-flight request to lose. So the tests below suspend or exclude a
// player WHILE traffic is running at them, and assert on what the ledger and the
// wallet say afterwards.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE GUARANTEE BEING ASSERTED
// ─────────────────────────────────────────────────────────────────────────────
// Not "no request settles after the transition is requested" — a wager that
// acquired the wallet lock first is entitled to finish, and refusing it would
// mean rolling back committed money. The guarantee is stronger and more useful:
//
//	every request that settles was serialized BEFORE the block committed, and
//	every request that starts after it is refused.
//
// which is exactly what the shared wallet lock buys, and what makes the wallet
// balance afterwards equal to the value of the settled requests and nothing
// else.
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

// blockedStatuses are the two the whole of Phase A exists to make enforceable.
// They are tested together, and identically, because a player protected by one
// must be exactly as protected by the other — a gap in either is a gap in the
// control.
var blockedStatuses = []string{StatusSuspended, StatusSelfExcluded}

// applyBlock puts a player into a blocking status through the REAL write path
// wherever it can.
//
// SUSPENDED goes through ProcessStatusTransition, so this suite exercises A2's
// code rather than a fixture. SELF_EXCLUDED is applied directly ONLY where a
// test needs the player already excluded at t=0, because the real path is also
// used in the concurrent tests below — see the note there.
func applyBlock(t *testing.T, pool *pgxpool.Pool, casino CasinoEngine, playerID uuid.UUID, status string) {
	t.Helper()
	if status == StatusSelfExcluded {
		forceSelfExclusion(t, pool, playerID, time.Now().UTC().Add(MinSelfExclusionTerm))
		return
	}
	if _, err := casino.ProcessStatusTransition(context.Background(), StatusTransitionRequest{
		OperatorCode: "OP1", PlayerID: playerID, ToStatus: status,
		ActorType: "OPERATOR", Reason: "regression coverage: blocking status",
	}); err != nil {
		t.Fatalf("apply %s: %v", status, err)
	}
}

// moneyPath is one way a player can move money. Every one of them must refuse a
// blocked player, so they are driven from one table rather than one test each —
// a new path added to the engine and not added here is the gap this shape is
// meant to make obvious.
type moneyPath struct {
	name string
	call func(t *testing.T, pool *pgxpool.Pool, eng Engine, casino CasinoEngine, game GameEngine, playerID uuid.UUID) error
}

func allMoneyPaths() []moneyPath {
	return []moneyPath{
		{
			name: "purchase",
			call: func(t *testing.T, _ *pgxpool.Pool, _ Engine, casino CasinoEngine, _ GameEngine, playerID uuid.UUID) error {
				_, err := casino.ProcessPurchase(context.Background(), PurchaseRequest{
					OperatorCode: "OP1", OperatorTransactionID: "blk-pur-" + uuid.NewString(),
					PlayerID: playerID, GCAmount: mustMoney(t, "10.0000"), SCPromoAmount: mustMoney(t, "5.0000"),
				})
				return err
			},
		},
		{
			name: "bet",
			call: func(t *testing.T, _ *pgxpool.Pool, eng Engine, _ CasinoEngine, _ GameEngine, playerID uuid.UUID) error {
				_, err := eng.ProcessBet(context.Background(), BetRequest{
					OperatorCode: "OP1", OperatorTransactionID: "blk-bet-" + uuid.NewString(),
					PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "1.0000"),
				})
				return err
			},
		},
		{
			name: "win",
			call: func(t *testing.T, _ *pgxpool.Pool, eng Engine, _ CasinoEngine, _ GameEngine, playerID uuid.UUID) error {
				_, err := eng.ProcessWin(context.Background(), WinRequest{
					OperatorCode: "OP1", OperatorTransactionID: "blk-win-" + uuid.NewString(),
					PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "1.0000"),
				})
				return err
			},
		},
		{
			name: "spin",
			call: func(t *testing.T, _ *pgxpool.Pool, _ Engine, _ CasinoEngine, game GameEngine, playerID uuid.UUID) error {
				_, err := game.ProcessSpin(context.Background(), SpinRequest{
					OperatorCode: "OP1", OperatorTransactionID: "blk-spin-" + uuid.NewString(),
					PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, "1.0000"),
				})
				return err
			},
		},
		{
			name: "redeem",
			call: func(t *testing.T, _ *pgxpool.Pool, _ Engine, casino CasinoEngine, _ GameEngine, playerID uuid.UUID) error {
				_, err := casino.ProcessRedeem(context.Background(), RedeemRequest{
					OperatorCode: "OP1", OperatorTransactionID: "blk-red-" + uuid.NewString(),
					PlayerID: playerID, Amount: mustMoney(t, "1.0000"),
				})
				return err
			},
		},
	}
}

// walletTotal is every bucket summed — the single number that must not move for
// a blocked player, whichever path was attempted.
func walletTotal(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) decimal.Decimal {
	t.Helper()
	gc, scU, scR := walletBalances(t, pool, playerID)
	return gc.Add(scU).Add(scR)
}

func ledgerTxCount(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_transactions WHERE player_id = $1`, playerID).Scan(&n); err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	return n
}

// TestIntegration_BlockedStatusRefusesEveryMoneyPath is the coverage matrix:
// two blocking statuses × five money paths, each asserted to refuse AND to leave
// the wallet and the ledger untouched.
//
// The second half is the part worth having. An error return proves the caller
// was told no; it does not prove nothing happened. A guard that rejected AFTER
// debiting would satisfy the first assertion and fail the player.
func TestIntegration_BlockedStatusRefusesEveryMoneyPath(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	casino := newIntegrationCasino(pool)
	game := newIntegrationGame(pool)

	for _, status := range blockedStatuses {
		for _, path := range allMoneyPaths() {
			t.Run(status+"/"+path.name, func(t *testing.T) {
				playerID := seedPlayer(t, pool, "100.0000")
				// A redeemable balance, so the redeem path is refused by the
				// STATUS rather than by an empty bucket — otherwise that case
				// would pass for the wrong reason.
				creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "50.0000")

				before := walletTotal(t, pool, playerID)
				txBefore := ledgerTxCount(t, pool, playerID)

				applyBlock(t, pool, casino, playerID, status)

				err := path.call(t, pool, eng, casino, game, playerID)
				if !errors.Is(err, errs.ErrPlayerNotActive) {
					t.Fatalf("%s while %s: got %v, want ErrPlayerNotActive", path.name, status, err)
				}

				if after := walletTotal(t, pool, playerID); !after.Equal(before) {
					t.Errorf("%s while %s moved money: %s → %s", path.name, status, before, after)
				}
				if after := ledgerTxCount(t, pool, playerID); after != txBefore {
					t.Errorf("%s while %s wrote %d ledger transaction(s)", path.name, status, after-txBefore)
				}
			})
		}
	}
}

// TestIntegration_BlockTakesHoldUnderConcurrentTraffic is the race the
// sequential matrix cannot see.
//
// Thirty spins are fired at a player while a blocking transition commits into
// the middle of them. Both contend for the same wallet row, so every spin lands
// strictly before or strictly after the block — never inside it.
//
// Three things are asserted, and the third is the one that would catch a real
// defect: that the wallet moved by EXACTLY the value of the spins that
// succeeded. A spin that settled money and then reported an error, or one
// refused after its debit, would balance the first two assertions and break this
// one.
func TestIntegration_BlockTakesHoldUnderConcurrentTraffic(t *testing.T) {
	for _, status := range blockedStatuses {
		t.Run(status, func(t *testing.T) {
			pool := integrationPool(t)
			casino := newIntegrationCasino(pool)
			game := newIntegrationGame(pool)

			playerID := seedPlayer(t, pool, "1000.0000")
			before := walletTotal(t, pool, playerID)

			const (
				concurrency = 30
				stake       = "1.0000"
			)

			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				settled  int
				blocked  int
				otherErr []error
			)
			start := make(chan struct{})

			// Spins are STAGGERED rather than released in one burst. A burst does
			// not race the transition at all: thirty goroutines queue on the
			// wallet lock before the block's transaction even begins, so all
			// thirty settle ahead of it and the run proves nothing about
			// ordering. Spreading them over a window wider than the transition
			// takes is what puts the block genuinely mid-stream.
			const stagger = 5 * time.Millisecond

			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					time.Sleep(time.Duration(i) * stagger)
					_, err := game.ProcessSpin(context.Background(), SpinRequest{
						OperatorCode:          "OP1",
						OperatorTransactionID: fmt.Sprintf("blk-race-%s-%d", uuid.NewString(), i),
						PlayerID:              playerID,
						Family:                domain.FamilySC,
						BetAmount:             mustMoney(t, stake),
					})
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						settled++
					case errors.Is(err, errs.ErrPlayerNotActive):
						blocked++
					default:
						otherErr = append(otherErr, err)
					}
				}(i)
			}

			// The block, racing the traffic. Applied through the real write path
			// for SUSPENDED; SELF_EXCLUDED needs a term already in force, and
			// forceSelfExclusion writes the same row the guard reads, so both
			// contend with the spins identically.
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// Land roughly a third of the way through the stream, so some
				// spins are committed before it and the rest arrive after.
				time.Sleep(concurrency / 3 * stagger)
				applyBlock(t, pool, casino, playerID, status)
			}()

			close(start)
			wg.Wait()

			if len(otherErr) > 0 {
				t.Fatalf("unexpected errors: %v", otherErr)
			}
			if settled+blocked != concurrency {
				t.Fatalf("accounted for %d of %d spins", settled+blocked, concurrency)
			}
			// The block must actually have landed mid-flight; if every spin
			// settled, the test proved nothing about ordering and needs a
			// tighter race rather than a passing tick.
			// Both halves must be non-empty, or the run did not exercise a
			// mid-stream block and its pass would be an accident of scheduling
			// rather than evidence.
			if blocked == 0 {
				t.Fatalf("no spin was blocked — the transition never raced the traffic, "+
					"so this run demonstrates nothing (settled %d)", settled)
			}
			if settled == 0 {
				t.Fatalf("no spin settled — the block landed before the traffic started, "+
					"so this run says nothing about an in-flight request (blocked %d)", blocked)
			}

			// THE assertion: the wallet moved by exactly what the settled spins
			// staked, so nothing was half-applied on either side of the block.
			after := walletTotal(t, pool, playerID)
			wantMoved := decimal.RequireFromString(stake).Mul(decimal.NewFromInt(int64(settled)))
			if moved := before.Sub(after); !moved.Equal(wantMoved) {
				t.Errorf("wallet moved %s, want %s (%d settled spins × %s) — a request was "+
					"half-applied across the block", moved, wantMoved, settled, stake)
			}

			assertLedgerReconciles(t, pool, playerID)
			assertDoubleEntryBalanced(t, pool)

			// And the block is total afterwards: no straggler gets through.
			if _, err := game.ProcessSpin(context.Background(), SpinRequest{
				OperatorCode: "OP1", OperatorTransactionID: "blk-after-" + uuid.NewString(),
				PlayerID: playerID, Family: domain.FamilySC, BetAmount: mustMoney(t, stake),
			}); !errors.Is(err, errs.ErrPlayerNotActive) {
				t.Errorf("a spin after the block: got %v, want ErrPlayerNotActive", err)
			}
		})
	}
}

// TestIntegration_BlockedPlayerRefusedAcrossAllPathsConcurrently fires all five
// money paths at a blocked player simultaneously, repeatedly.
//
// The matrix above tests each path in isolation; this asks whether any of them
// finds a way through when they contend with each other for the same wallet
// row. Every single attempt must be refused, and the wallet must be byte-for-
// byte unchanged at the end.
func TestIntegration_BlockedPlayerRefusedAcrossAllPathsConcurrently(t *testing.T) {
	for _, status := range blockedStatuses {
		t.Run(status, func(t *testing.T) {
			pool := integrationPool(t)
			eng := New(pool, openIdem{}, discardLoggerRepo())
			casino := newIntegrationCasino(pool)
			game := newIntegrationGame(pool)

			playerID := seedPlayer(t, pool, "500.0000")
			creditOpeningBalance(t, pool, playerID, "SC_REDEEMABLE", "100.0000")
			applyBlock(t, pool, casino, playerID, status)

			before := walletTotal(t, pool, playerID)
			txBefore := ledgerTxCount(t, pool, playerID)

			const rounds = 8
			paths := allMoneyPaths()

			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				leaked  []string
				unknown []error
			)
			start := make(chan struct{})

			for r := 0; r < rounds; r++ {
				for _, path := range paths {
					wg.Add(1)
					go func(path moneyPath) {
						defer wg.Done()
						<-start
						err := path.call(t, pool, eng, casino, game, playerID)
						mu.Lock()
						defer mu.Unlock()
						switch {
						case err == nil:
							leaked = append(leaked, path.name)
						case errors.Is(err, errs.ErrPlayerNotActive):
							// The only acceptable outcome.
						default:
							unknown = append(unknown, fmt.Errorf("%s: %w", path.name, err))
						}
					}(path)
				}
			}
			close(start)
			wg.Wait()

			if len(leaked) > 0 {
				t.Errorf("%d request(s) SUCCEEDED for a %s player, via: %v", len(leaked), status, leaked)
			}
			for _, err := range unknown {
				t.Errorf("refused for the wrong reason: %v", err)
			}

			if after := walletTotal(t, pool, playerID); !after.Equal(before) {
				t.Errorf("wallet moved under concurrent load against a %s player: %s → %s", status, before, after)
			}
			if after := ledgerTxCount(t, pool, playerID); after != txBefore {
				t.Errorf("%d ledger transaction(s) written for a %s player", after-txBefore, status)
			}
			assertDoubleEntryBalanced(t, pool)
		})
	}
}

// TestIntegration_BlockSurvivesTheIdempotencyBarrier covers the interaction
// between a blocking status and the replay machinery, and records a real
// limitation found while writing it.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE SECURITY PROPERTY (holds)
// ─────────────────────────────────────────────────────────────────────────────
// A blocked player cannot obtain a NEW settlement, whatever they do with
// operator transaction ids. That is the property that matters and it is asserted
// below.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE LIMITATION (found here, deliberately pinned rather than papered over)
// ─────────────────────────────────────────────────────────────────────────────
// Replaying an ALREADY-SETTLED transaction id for a since-blocked player returns
// ErrPlayerNotActive rather than the original receipt. requirePlayerActive runs
// immediately after the wallet lock, which is well before the ledger INSERT
// whose 23505 would trigger Ghost-Spin recovery — so the guard answers first and
// recovery is never reached.
//
// In production this is masked: the Redis idempotency barrier returns the cached
// result before any database work, so a replay inside the cache TTL gets its
// receipt. The gap is a replay AFTER the TTL expires, where an operator retrying
// a timed-out call learns "player not active" instead of "here is what already
// happened" — and cannot tell whether their original request settled.
//
// This suite runs with the barrier DISABLED (openIdem), which is why the
// behaviour is visible here at all. It is pinned as-is rather than fixed in
// passing: moving the status guard after the dedup insert reorders two
// safety-critical checks on every money path, and that is a change to argue for
// on its own, not a tidy-up at the end of a task.
func TestIntegration_BlockSurvivesTheIdempotencyBarrier(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, "100.0000")
	opTxID := "blk-idem-" + uuid.NewString()

	// A successful wager before the block.
	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: opTxID,
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	}); err != nil {
		t.Fatalf("pre-block bet: %v", err)
	}

	applyBlock(t, pool, casino, playerID, StatusSuspended)

	before := walletTotal(t, pool, playerID)
	txBefore := ledgerTxCount(t, pool, playerID)

	// The limitation, pinned. If this ever starts returning the receipt instead,
	// the guard order changed — which may well be the right fix, but it must be
	// a deliberate one, and this assertion is what forces the conversation.
	_, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: opTxID,
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	})
	if !errors.Is(err, errs.ErrPlayerNotActive) {
		t.Errorf("replay of a settled id for a blocked player: got %v.\n"+
			"Documented behaviour is ErrPlayerNotActive (the status guard precedes "+
			"Ghost-Spin recovery). If this now recovers the original receipt, the "+
			"guard order changed and this comment needs updating.", err)
	}

	// Whatever it ANSWERS, it must not have settled anything a second time.
	// That is the half that protects the money.
	if after := walletTotal(t, pool, playerID); !after.Equal(before) {
		t.Errorf("a replay moved money after the block: %s → %s", before, after)
	}
	if after := ledgerTxCount(t, pool, playerID); after != txBefore {
		t.Errorf("a replay wrote %d ledger transaction(s) after the block", after-txBefore)
	}

	// And a genuinely NEW wager is refused — the security property.
	if _, err := eng.ProcessBet(ctx, BetRequest{
		OperatorCode: "OP1", OperatorTransactionID: "blk-idem-fresh-" + uuid.NewString(),
		PlayerID: playerID, Family: domain.FamilySC, Amount: mustMoney(t, "10.0000"),
	}); !errors.Is(err, errs.ErrPlayerNotActive) {
		t.Errorf("a fresh wager while blocked: got %v, want ErrPlayerNotActive", err)
	}
	assertLedgerReconciles(t, pool, playerID)
}
