//go:build integration

package repository

// session_integration_test.go proves GetSessionSnapshot reports the two facts
// the gateway could not previously see, against a real database.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIntegration_SessionSnapshotReportsStatusAndPlaythrough(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())
	casino := newIntegrationCasino(pool)

	playerID := seedPlayer(t, pool, "250.0000")

	// A fresh player: ACTIVE, and nothing owed.
	snap, err := eng.GetSessionSnapshot(context.Background(), playerID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Status != StatusActive {
		t.Errorf("status = %q, want %q", snap.Status, StatusActive)
	}
	if !snap.PlaythroughOutstanding.Decimal().IsZero() {
		t.Errorf("playthrough outstanding = %s, want 0", snap.PlaythroughOutstanding)
	}
	if got := snap.Wallet.SCUnplayed.String(); got != "250.0000" {
		t.Errorf("sc_unplayed = %s, want 250.0000", got)
	}

	// A promotional grant creates an obligation, and the snapshot reports its size
	// — not merely that one exists, which is what makes it useful to a caller
	// deciding whether to show "wager 40 more".
	if _, err := casino.ProcessPurchase(context.Background(), PurchaseRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "snap-purchase-" + uuid.NewString(),
		PlayerID:              playerID,
		GCAmount:              mustMoney(t, "0"),
		SCPromoAmount:         mustMoney(t, "40.0000"),
	}); err != nil {
		t.Fatalf("purchase: %v", err)
	}

	snap, err = eng.GetSessionSnapshot(context.Background(), playerID)
	if err != nil {
		t.Fatalf("snapshot after grant: %v", err)
	}
	if got := snap.PlaythroughOutstanding.String(); got != "40.0000" {
		t.Errorf("playthrough outstanding = %s, want 40.0000", got)
	}

	// A blocking transition is visible immediately, which is the whole point:
	// the gateway can refuse a redemption without spending a signed round trip
	// discovering what the engine already knows.
	if _, err := casino.ProcessStatusTransition(context.Background(), StatusTransitionRequest{
		OperatorCode: "OP1", PlayerID: playerID, ToStatus: StatusSuspended,
		ActorType: "OPERATOR", Reason: "session snapshot coverage",
	}); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	snap, err = eng.GetSessionSnapshot(context.Background(), playerID)
	if err != nil {
		t.Fatalf("snapshot after suspension: %v", err)
	}
	if snap.Status != StatusSuspended {
		t.Errorf("status = %q, want %q", snap.Status, StatusSuspended)
	}
	// The balance is still reported: a suspended player may still be shown what
	// they hold. Only the ability to MOVE it is withdrawn.
	if got := snap.Wallet.SCUnplayed.String(); got == "0.0000" {
		t.Errorf("wallet was blanked by the suspension; want the balance still reported")
	}
}

func TestIntegration_SessionSnapshotSeesSelfExclusion(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	playerID := seedPlayer(t, pool, "10.0000")
	forceSelfExclusion(t, pool, playerID, time.Now().UTC().Add(MinSelfExclusionTerm))

	snap, err := eng.GetSessionSnapshot(context.Background(), playerID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Status != StatusSelfExcluded {
		t.Errorf("status = %q, want %q", snap.Status, StatusSelfExcluded)
	}
}

func TestIntegration_SessionSnapshotUnknownPlayer(t *testing.T) {
	pool := integrationPool(t)
	eng := New(pool, openIdem{}, discardLoggerRepo())

	// Fails closed with a domain error rather than an empty snapshot that would
	// read as "ACTIVE with nothing owed".
	if _, err := eng.GetSessionSnapshot(context.Background(), uuid.New()); err == nil {
		t.Fatal("unknown player: got nil error, want ErrPlayerNotFound")
	}
}
