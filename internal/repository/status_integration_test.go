//go:build integration

package repository

// status_integration_test.go proves the properties of ProcessStatusTransition
// that only a real PostgreSQL can demonstrate:
//
//   - the users UPDATE and the audit INSERT are ONE transaction (fault-injected,
//     not merely asserted from reading the code),
//   - a transition contends on the same wallet lock as a settling wager, so the
//     two cannot interleave,
//   - the schema refuses a self-exclusion without a term for EVERY writer, not
//     just for callers who go through the Go validation.
//
// The rule logic itself — which moves are legal, when a term has expired, the
// 180-day floor — is covered exhaustively in status_test.go without a database.
// Here the same rules are re-checked end to end, so a refusal is proven to leave
// nothing behind rather than just to return the right error.
//
//	TEST_POSTGRES_URL=postgres://postgres@127.0.0.1:5433/true_engine?sslmode=disable \
//	    go test -tags integration ./internal/repository/ -run Integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

// sqlStateRepo returns the Postgres SQLSTATE of err, or "" if it is not a
// PgError. Asserting the code rather than the message keeps these tests stable
// across PostgreSQL releases.
func sqlStateRepo(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// seedBalance is what these tests fund a player with. The amount is irrelevant
// to a status transition — none of this moves money — but it cannot be zero:
// seedPlayer posts a REAL ledger credit to open the balance, and ledger_entries
// enforces a positive amount. A status test that appeared to work with a
// zero-amount ledger entry would be testing a row the ledger would never hold.
const seedBalance = "10.0000"

func newIntegrationCasino(pool *pgxpool.Pool) CasinoEngine {
	return NewCasino(pool, openIdem{}, discardLoggerRepo())
}

// transitionCount returns how many audit rows exist for a player. The audit
// table is append-only, so this only ever grows — which makes it a reliable
// probe for "did a refused transition leave anything behind".
func transitionCount(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM player_status_transitions WHERE player_id = $1`, playerID).Scan(&n); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	return n
}

func playerStatus(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID) (string, *time.Time) {
	t.Helper()
	var (
		status string
		until  *time.Time
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT status, self_exclusion_until FROM users WHERE id = $1`, playerID).Scan(&status, &until); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status, until
}

// forceSelfExclusion puts a player into SELF_EXCLUDED with an arbitrary term,
// bypassing ProcessStatusTransition.
//
// Needed because the API deliberately cannot produce the state some of these
// tests need to start from: a term already in the past. That is the point of
// the 180-day floor, so the setup goes around it rather than weakening it.
func forceSelfExclusion(t *testing.T, pool *pgxpool.Pool, playerID uuid.UUID, until time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET status = 'SELF_EXCLUDED', self_exclusion_until = $2 WHERE id = $1`,
		playerID, until); err != nil {
		t.Fatalf("force self-exclusion: %v", err)
	}
}

// ----------------------------------------------------------------------------
// Atomicity
// ----------------------------------------------------------------------------

// TestIntegration_StatusTransitionWritesBothOrNeither is the ACID requirement,
// proven by fault injection rather than by inspection.
//
// A trigger is installed that makes the audit INSERT fail. The status UPDATE has
// already executed by that point, so if the two were not in one transaction the
// player would end up suspended with no record of why — a status change with no
// audit trail, which is precisely the failure the whole design exists to
// prevent. The rollback must discard both.
func TestIntegration_StatusTransitionWritesBothOrNeither(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	before, _ := playerStatus(t, pool, playerID)
	countBefore := transitionCount(t, pool, playerID)

	// Fault injection: refuse the audit INSERT, and only the audit INSERT.
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION it_fail_transition_insert()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected audit failure' USING ERRCODE = 'P0001';
		END;
		$$;
		CREATE TRIGGER it_fail_transition_insert
			BEFORE INSERT ON player_status_transitions
			FOR EACH ROW EXECUTE FUNCTION it_fail_transition_insert();`); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS it_fail_transition_insert ON player_status_transitions;
			 DROP FUNCTION IF EXISTS it_fail_transition_insert();`)
	})

	_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusSuspended,
		ActorType:    "OPERATOR",
		Reason:       "fraud review — audit insert will be forced to fail",
	})
	if err == nil {
		t.Fatal("transition succeeded despite a failing audit insert")
	}

	// The whole point: the UPDATE must have been rolled back with it.
	after, _ := playerStatus(t, pool, playerID)
	if after != before {
		t.Errorf("status moved to %s with no audit row; the two writes are not atomic", after)
	}
	if got := transitionCount(t, pool, playerID); got != countBefore {
		t.Errorf("audit rows = %d, want %d", got, countBefore)
	}
}

// TestIntegration_StatusTransitionRecordsTheAuditRow is the happy path, checked
// field by field. The audit row IS the compliance deliverable, so its contents
// are asserted rather than its existence.
func TestIntegration_StatusTransitionRecordsTheAuditRow(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)

	result, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusSuspended,
		ActorType:    "OPERATOR",
		ActorRef:     "admin-7",
		Reason:       "chargeback investigation",
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}

	if result.FromStatus != StatusActive || result.ToStatus != StatusSuspended {
		t.Errorf("result reports %s → %s, want ACTIVE → SUSPENDED", result.FromStatus, result.ToStatus)
	}
	if result.TransitionID == uuid.Nil {
		t.Error("result carries no transition id")
	}

	status, _ := playerStatus(t, pool, playerID)
	if status != StatusSuspended {
		t.Errorf("users.status = %s, want SUSPENDED", status)
	}

	var (
		from, to, actorType, reason string
		actorRef                    *string
		until                       *time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT from_status, to_status, actor_type, actor_ref, reason, self_exclusion_until
		FROM player_status_transitions WHERE id = $1`, result.TransitionID,
	).Scan(&from, &to, &actorType, &actorRef, &reason, &until); err != nil {
		t.Fatalf("read audit row: %v", err)
	}

	if from != StatusActive || to != StatusSuspended {
		t.Errorf("audit row records %s → %s", from, to)
	}
	if actorType != "OPERATOR" {
		t.Errorf("actor_type = %s, want OPERATOR", actorType)
	}
	if actorRef == nil || *actorRef != "admin-7" {
		t.Errorf("actor_ref = %v, want admin-7", actorRef)
	}
	if reason != "chargeback investigation" {
		t.Errorf("reason = %q", reason)
	}
	if until != nil {
		t.Errorf("self_exclusion_until = %v on a suspension, want NULL", until)
	}
}

// TestIntegration_BlankActorRefIsStoredAsNull: an attribution that carries
// nothing should not masquerade as one. The Go layer normalizes it away before
// the DB constraint would reject it.
func TestIntegration_BlankActorRefIsStoredAsNull(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	result, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusSuspended,
		ActorType:    "SYSTEM",
		ActorRef:     "   ",
		Reason:       "automated velocity rule",
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}

	var actorRef *string
	if err := pool.QueryRow(ctx,
		`SELECT actor_ref FROM player_status_transitions WHERE id = $1`, result.TransitionID).Scan(&actorRef); err != nil {
		t.Fatalf("read actor_ref: %v", err)
	}
	if actorRef != nil {
		t.Errorf("actor_ref = %q, want NULL", *actorRef)
	}
}

// ----------------------------------------------------------------------------
// Irrevocability
// ----------------------------------------------------------------------------

// TestIntegration_SelfExclusionCannotBeLiftedBeforeExpiry is the guardrail,
// end to end and including by an operator or a regulator.
//
// Each refusal is checked for its side effects as well as its error: a rejection
// that still wrote an audit row, or still moved the status, would be worse than
// no rejection at all because it would look handled.
func TestIntegration_SelfExclusionCannotBeLiftedBeforeExpiry(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)

	// Exactly the regulatory minimum. Under the earlier timestamp API this very
	// request failed, because the milliseconds it spent reaching the database
	// put it a hair under 180 days — the defect that motivated the day count.
	if _, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode:      "OP1",
		PlayerID:          playerID,
		ToStatus:          StatusSelfExcluded,
		ActorType:         "PLAYER",
		Reason:            "player requested a 180-day exclusion",
		SelfExclusionDays: MinSelfExclusionDays,
	}); err != nil {
		t.Fatalf("a request for exactly the minimum term must succeed: %v", err)
	}

	countAfterExclusion := transitionCount(t, pool, playerID)

	// SUSPENDED and KYC_PENDING are the subtle ones: neither returns the player
	// to play directly, but both are operator-liftable, so allowing them would
	// launder an irrevocable exclusion into a reversible hold.
	for _, attempt := range []struct {
		to    string
		actor string
	}{
		{StatusActive, "OPERATOR"},
		{StatusActive, "PLAYER"},
		{StatusActive, "REGULATOR"},
		{StatusSuspended, "OPERATOR"},
		{StatusKYCPending, "SYSTEM"},
	} {
		t.Run(attempt.to+"_by_"+attempt.actor, func(t *testing.T) {
			_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
				OperatorCode: "OP1",
				PlayerID:     playerID,
				ToStatus:     attempt.to,
				ActorType:    attempt.actor,
				Reason:       "attempting to lift an active exclusion",
			})
			if !errors.Is(err, errs.ErrSelfExclusionActive) {
				t.Fatalf("got %v, want ErrSelfExclusionActive", err)
			}

			status, _ := playerStatus(t, pool, playerID)
			if status != StatusSelfExcluded {
				t.Errorf("status moved to %s despite the refusal", status)
			}
			if got := transitionCount(t, pool, playerID); got != countAfterExclusion {
				t.Errorf("a refused transition wrote an audit row: %d, want %d", got, countAfterExclusion)
			}
		})
	}
}

// TestIntegration_SelfExcludedPlayerMayStillClose: closure is strictly more
// restrictive and terminal, so it is permitted mid-term. A player who wants out
// entirely must not be told to wait out the exclusion first.
func TestIntegration_SelfExcludedPlayerMayStillClose(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	forceSelfExclusion(t, pool, playerID, time.Now().UTC().Add(365*24*time.Hour))

	if _, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusClosed,
		ActorType:    "PLAYER",
		Reason:       "player closed the account during an exclusion",
	}); err != nil {
		t.Fatalf("SELF_EXCLUDED → CLOSED must be permitted mid-term: %v", err)
	}

	status, _ := playerStatus(t, pool, playerID)
	if status != StatusClosed {
		t.Errorf("status = %s, want CLOSED", status)
	}

	// And CLOSED is terminal, which is what makes the above safe: if closure
	// were reversible it would be a two-step route out of an active exclusion.
	_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusActive,
		ActorType:    "OPERATOR",
		Reason:       "attempting to reopen a closed account",
	})
	if !errors.Is(err, errs.ErrStatusTransitionInvalid) {
		t.Errorf("CLOSED → ACTIVE: got %v, want ErrStatusTransitionInvalid", err)
	}
}

// TestIntegration_SelfExclusionLiftsAfterExpiry closes the loop: the control is
// a term, not a life sentence.
//
// It also pins the COALESCE behaviour — self_exclusion_until SURVIVES the return
// to ACTIVE. 000009 defines the column that way deliberately: the fact that a
// player has previously excluded themselves is exactly the signal a later
// responsible-gaming or fraud rule needs, and clearing it would destroy it.
func TestIntegration_SelfExclusionLiftsAfterExpiry(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	served := time.Now().UTC().Add(-time.Hour)
	forceSelfExclusion(t, pool, playerID, served)

	if _, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusActive,
		ActorType:    "SYSTEM",
		ActorRef:     "exclusion-expiry-sweep",
		Reason:       "self-exclusion term served",
	}); err != nil {
		t.Fatalf("an expired exclusion must be liftable: %v", err)
	}

	status, until := playerStatus(t, pool, playerID)
	if status != StatusActive {
		t.Errorf("status = %s, want ACTIVE", status)
	}
	if until == nil {
		t.Fatal("self_exclusion_until was cleared; the record of a prior exclusion must survive")
	}
	if !until.UTC().Truncate(time.Millisecond).Equal(served.Truncate(time.Millisecond)) {
		t.Errorf("self_exclusion_until = %v, want the served term %v", until.UTC(), served)
	}
}

// ----------------------------------------------------------------------------
// Minimum term
// ----------------------------------------------------------------------------

func TestIntegration_SelfExclusionMinimumTermEnforced(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	cases := []struct {
		name    string
		days    int
		wantErr error
	}{
		{"30 days", 30, errs.ErrSelfExclusionTooShort},
		{"179 days", MinSelfExclusionDays - 1, errs.ErrSelfExclusionTooShort},
		// The boundary itself, end to end. A day count makes this exact: there
		// is no round trip for the term to lose time in.
		{"exactly 180 days", MinSelfExclusionDays, nil},
		{"one year", 365, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			playerID := seedPlayer(t, pool, seedBalance)

			_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
				OperatorCode:      "OP1",
				PlayerID:          playerID,
				ToStatus:          StatusSelfExcluded,
				ActorType:         "PLAYER",
				Reason:            "player requested an exclusion",
				SelfExclusionDays: c.days,
			})

			switch {
			case c.wantErr == nil && err != nil:
				t.Fatalf("got %v, want success", err)
			case c.wantErr != nil && !errors.Is(err, c.wantErr):
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}

			status, until := playerStatus(t, pool, playerID)
			if c.wantErr != nil {
				if status != StatusActive {
					t.Errorf("status = %s, want ACTIVE after a refusal", status)
				}
				if until != nil {
					t.Errorf("a refused exclusion still wrote a term: %v", until)
				}
				return
			}

			if status != StatusSelfExcluded {
				t.Errorf("status = %s, want SELF_EXCLUDED", status)
			}
			if until == nil {
				t.Fatal("SELF_EXCLUDED with no term; 000010's constraint should have made this impossible")
			}
			// The stored term must be the term that was CHOSEN, measured from
			// the engine's own clock — not the caller's, and not shortened by
			// however long the request was in flight.
			wantUntil := time.Now().UTC().Add(time.Duration(c.days) * 24 * time.Hour)
			if drift := until.UTC().Sub(wantUntil); drift > time.Minute || drift < -time.Minute {
				t.Errorf("term ends %v, want within a minute of %v (drift %v)", until.UTC(), wantUntil, drift)
			}
		})
	}
}

// TestIntegration_SchemaRefusesSelfExclusionWithoutTerm proves migration
// 000010's constraint binds writers that never touch this Go code — a future
// migration, an admin console, a hand-run UPDATE at 3am. A rule enforced only in
// the application is a rule that holds only where that application is the sole
// writer.
func TestIntegration_SchemaRefusesSelfExclusionWithoutTerm(t *testing.T) {
	pool := integrationPool(t)
	playerID := seedPlayer(t, pool, seedBalance)

	_, err := pool.Exec(context.Background(),
		`UPDATE users SET status = 'SELF_EXCLUDED' WHERE id = $1`, playerID)
	if err == nil {
		t.Fatal("raw UPDATE to SELF_EXCLUDED with no term SUCCEEDED; the constraint is missing")
	}
	if got := sqlStateRepo(err); got != "23514" {
		t.Errorf("SQLSTATE %q, want 23514 check_violation (err: %v)", got, err)
	}
}

// ----------------------------------------------------------------------------
// Serialization against the money paths
// ----------------------------------------------------------------------------

// TestIntegration_StatusTransitionBlocksOnTheWalletLock proves the property the
// whole locking argument rests on: a transition contends on the SAME lock a
// wager takes.
//
// This is not a detail. If a transition locked only the users row, the two would
// never contend — under READ COMMITTED a plain SELECT does not block on a
// FOR UPDATE lock — and a wager could read status = 'ACTIVE' while an exclusion
// committed beside it, settling a bet for a player excluded a millisecond
// earlier. Asserted by holding the wallet lock and showing the transition waits.
func TestIntegration_StatusTransitionBlocksOnTheWalletLock(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)

	// Hold the wallet row exactly as a settling wager does.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	var probe uuid.UUID
	if err := holder.QueryRow(ctx,
		`SELECT player_id FROM wallets WHERE player_id = $1 FOR UPDATE`, playerID).Scan(&probe); err != nil {
		t.Fatalf("lock wallet: %v", err)
	}

	done := make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { _ = holder.Rollback(ctx) }) }
	t.Cleanup(release)

	go func() {
		_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
			OperatorCode: "OP1",
			PlayerID:     playerID,
			ToStatus:     StatusSuspended,
			ActorType:    "OPERATOR",
			Reason:       "must wait for the in-flight wager to settle",
		})
		done <- err
	}()

	// While the lock is held the transition must make no progress.
	select {
	case err := <-done:
		t.Fatalf("transition completed (%v) while the wallet lock was held; "+
			"it is not serialized against the money paths", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Releasing the lock must let it through.
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("transition failed after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("transition did not complete after the wallet lock was released")
	}

	status, _ := playerStatus(t, pool, playerID)
	if status != StatusSuspended {
		t.Errorf("status = %s, want SUSPENDED", status)
	}
}

// TestIntegration_StatusUnchangedIsRefusedWithoutWriting: the audit trail
// records CHANGES. A no-op is refused rather than written, and Zone 2 reads that
// error as "already applied" when replaying a transition.
func TestIntegration_StatusUnchangedIsRefusedWithoutWriting(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)
	ctx := context.Background()

	playerID := seedPlayer(t, pool, seedBalance)
	before := transitionCount(t, pool, playerID)

	_, err := casino.ProcessStatusTransition(ctx, StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     playerID,
		ToStatus:     StatusActive,
		ActorType:    "OPERATOR",
		Reason:       "re-applying a status the player already holds",
	})
	if !errors.Is(err, errs.ErrStatusUnchanged) {
		t.Fatalf("got %v, want ErrStatusUnchanged", err)
	}
	if got := transitionCount(t, pool, playerID); got != before {
		t.Errorf("a no-op wrote an audit row: %d, want %d", got, before)
	}
}

// TestIntegration_UnknownPlayerIsRejected: the wallet lock doubles as the
// existence check, so a transition for a player who does not exist fails before
// anything is read or written.
func TestIntegration_UnknownPlayerIsRejected(t *testing.T) {
	pool := integrationPool(t)
	casino := newIntegrationCasino(pool)

	_, err := casino.ProcessStatusTransition(context.Background(), StatusTransitionRequest{
		OperatorCode: "OP1",
		PlayerID:     uuid.New(),
		ToStatus:     StatusSuspended,
		ActorType:    "OPERATOR",
		Reason:       "no such player",
	})
	if !errors.Is(err, errs.ErrPlayerNotFound) {
		t.Errorf("got %v, want ErrPlayerNotFound", err)
	}
}
