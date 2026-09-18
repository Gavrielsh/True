package repository

// status.go implements the ONLY write path for users.status.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS IS THE ONLY WRITE PATH
// ─────────────────────────────────────────────────────────────────────────────
// requirePlayerActive refuses every money path for a player who is not ACTIVE,
// and has done so since long before this file existed. What was missing was any
// way to REACH a non-ACTIVE status: nothing in the repository wrote users.status
// at all. Self-exclusion, operator suspension and regulator-mandated closure
// were unimplementable — the enforcement was a door with no handle.
//
// Every transition therefore goes through ProcessStatusTransition, and
// ProcessStatusTransition writes the users UPDATE and its audit row in ONE
// transaction. A status that moved without a record of who moved it and why is
// not a compliance record, so the schema and this function make the two
// inseparable: migration 000010's grant gives the application role UPDATE on
// users and INSERT-only on player_status_transitions, and the audit table's
// trigger refuses UPDATE and DELETE for every role including the owner.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THE WALLET ROW IS LOCKED FOR A CHANGE THAT TOUCHES NO BALANCE
// ─────────────────────────────────────────────────────────────────────────────
// Because that is the lock the money paths take, and serialization only happens
// on a SHARED lock.
//
// Every money path runs selectWalletForUpdate (SELECT ... FOR UPDATE on wallets)
// and then reads users.status on the same tx handle. If a transition locked only
// the users row, the two would not contend at all: under READ COMMITTED a plain
// SELECT does not block on a FOR UPDATE lock, so a wager could read
// status = 'ACTIVE' while an exclusion committed alongside it, and settle a bet
// for a player who was excluded a millisecond earlier.
//
// Taking the wallet lock — in the same order as every money path, so no deadlock
// is introduced — makes the interleaving impossible. A transition waits for an
// in-flight spin to finish, and a spin that starts after the transition commits
// reads the new status. There is no half state where a player is excluded and a
// wager is mid-settlement.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THERE IS NO REDIS IDEMPOTENCY BARRIER HERE
// ─────────────────────────────────────────────────────────────────────────────
// The money paths key their barrier on operator_transaction_id and cache a
// TxResult tied to a ledger row. A status transition writes no ledger row, so
// there is nothing to anchor and nothing to replay. Duplicate protection comes
// from two places instead:
//
//   - The perimeter: X-Nonce is single-use inside ReplayGuard's window, so a
//     byte-identical resend is rejected before it reaches this code.
//   - This function: a transition to the status the player already holds is
//     refused with ErrStatusUnchanged rather than written, because the audit
//     trail records changes. Zone 2 treats that error as an idempotent success
//     when replaying a transition it may already have applied — which is the
//     right layering, since Zone 2 already owns dedup through the unique key on
//     EngineRequestLog.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Player statuses, mirroring the user_status ENUM (000001 + 000009). Declared
// as constants so the transition rules below read as rules rather than as
// string comparisons.
const (
	StatusKYCPending   = "KYC_PENDING"
	StatusActive       = "ACTIVE"
	StatusSuspended    = "SUSPENDED"
	StatusSelfExcluded = "SELF_EXCLUDED"
	StatusClosed       = "CLOSED"
)

// MinSelfExclusionDays is the regulatory floor on a self-exclusion.
//
// 180 days, not a shorter convenience option. Hopfgartner 2023 (n = 3,203)
// found 75.3% of players returned after exclusions of 38 days or less, against
// 0.9% for exclusions between 90 days and a year: a short exclusion is not a
// weaker version of the control, it is a different and largely ineffective one.
// Offering a 7-day option would let the product advertise a protection that the
// evidence says does not work.
const MinSelfExclusionDays = 180

// maxSelfExclusionDays is a sanity bound, NOT a policy limit. It catches a
// caller that has confused units — days for hours, or a sentinel value — before
// it becomes an exclusion nobody can explain. A genuinely permanent decision is
// expressed as StatusClosed, which is terminal.
const maxSelfExclusionDays = 100 * 365

// MinSelfExclusionTerm is MinSelfExclusionDays as a duration, for callers and
// tests that reason in absolute time.
const MinSelfExclusionTerm = MinSelfExclusionDays * 24 * time.Hour

// ─────────────────────────────────────────────────────────────────────────────
// WHY THE TERM IS A DAY COUNT AND NOT AN END TIMESTAMP
// ─────────────────────────────────────────────────────────────────────────────
// The first version of this API took the expiry instant directly. It was wrong,
// and the integration suite proved it: a caller computing `now + 180 days` and
// sending that instant is REJECTED, because the few milliseconds spent in
// transport mean the term measures 180 days minus epsilon by the time the
// database clock evaluates it. The single most common self-exclusion request —
// a player choosing the minimum option — would have failed in production, and
// the only workarounds are for Zone 2 to pad the value (an implicit coupling
// nobody would remember) or for the engine to allow slightly under the
// regulatory floor (a compromise that reads badly in an audit, for a reason
// that has nothing to do with player protection).
//
// A day count has neither problem. The player chooses "180 days"; the engine
// computes the expiry from its OWN clock at the instant the transition commits,
// so the floor comparison is exact, the stored term is exactly what was chosen,
// and Zone 2's clock cannot disagree with Zone 1's about when an exclusion ends.
// It is also how every jurisdiction and every UI expresses the choice.

// Field bounds, mirroring the CHECK constraints on player_status_transitions.
// Duplicated here on purpose: the database is the backstop that binds every
// writer, and this is the layer that can tell the caller which field was wrong.
const (
	maxTransitionReasonLen   = 500
	maxTransitionActorRefLen = 200
)

// validActorTypes mirrors the status_actor ENUM (000009).
//
// Note what is deliberately NOT enforced: the actor is recorded, not restricted
// against the destination. An OPERATOR may legitimately record a self-exclusion
// a player requested by phone, and a REGULATOR may mandate one. Tying actor to
// destination would reject those real cases while preventing nothing — the
// actor is supplied by the same caller either way.
var validActorTypes = map[string]struct{}{
	"PLAYER": {}, "OPERATOR": {}, "SYSTEM": {}, "REGULATOR": {},
}

// validStatuses is every value the user_status ENUM accepts. Whether a
// particular move is LEGAL is decided by assertTransitionAllowed; this only
// rejects a destination the database could not store.
var validStatuses = map[string]struct{}{
	StatusKYCPending: {}, StatusActive: {}, StatusSuspended: {},
	StatusSelfExcluded: {}, StatusClosed: {},
}

// StatusTransitionRequest moves a player from their current status to ToStatus.
//
// The caller does NOT supply the current status: it is read under the wallet
// lock inside the transaction, so a caller cannot act on a stale view and cannot
// assert a "from" that was true a second ago.
type StatusTransitionRequest struct {
	OperatorCode string
	PlayerID     uuid.UUID

	// ToStatus is the destination. Case-insensitive; normalized to upper.
	ToStatus string

	// ActorType is who caused this (PLAYER / OPERATOR / SYSTEM / REGULATOR).
	ActorType string
	// ActorRef is the external reference for the actor — an admin user id, a
	// regulator order number, a KYC provider event id, or the name of an
	// automated sweep. Optional; a blank string is stored as NULL rather than
	// as an attribution that carries nothing.
	ActorRef string
	// Reason is mandatory free text. An unexplained compliance action is not
	// defensible, so there is no default and no empty value.
	Reason string

	// SelfExclusionDays is the term, in whole days. REQUIRED (and at least
	// MinSelfExclusionDays) when ToStatus is SELF_EXCLUDED; must be zero
	// otherwise — a term supplied alongside a suspension means the caller has
	// confused two operations, and is rejected rather than silently dropped.
	//
	// The engine converts this to an expiry instant using the DATABASE clock
	// inside the transaction. See the note above on why this is a duration
	// rather than a timestamp.
	SelfExclusionDays int
}

// StatusTransitionResult is the committed outcome, including the audit row's id
// so the caller can cite the exact record this action produced.
type StatusTransitionResult struct {
	PlayerID           uuid.UUID  `json:"player_id"`
	TransitionID       uuid.UUID  `json:"transition_id"`
	FromStatus         string     `json:"from_status"`
	ToStatus           string     `json:"to_status"`
	SelfExclusionUntil *time.Time `json:"self_exclusion_until,omitempty"`
	OccurredAt         time.Time  `json:"occurred_at"`
}

const (
	sqlSelectPlayerStatusForTransition = `
		SELECT status, self_exclusion_until, now()
		FROM users
		WHERE id = $1`

	// COALESCE, not a plain assignment: a term is written only when one is
	// supplied, so moving a player OUT of SELF_EXCLUDED preserves the record
	// that they were once excluded. 000009 defines the column as surviving the
	// exclusion that set it, and 000010's constraint is one-directional so that
	// survival is legal.
	sqlUpdatePlayerStatus = `
		UPDATE users
		SET status = $2::user_status,
		    self_exclusion_until = COALESCE($3, self_exclusion_until)
		WHERE id = $1`

	sqlInsertStatusTransition = `
		INSERT INTO player_status_transitions
			(player_id, from_status, to_status, actor_type, actor_ref, reason, self_exclusion_until)
		VALUES ($1, $2::user_status, $3::user_status, $4::status_actor, $5, $6, $7)
		RETURNING id, created_at`
)

// ProcessStatusTransition is the single entry point for changing a player's
// status. The users UPDATE and the audit INSERT commit together or not at all.
func (e *engine) ProcessStatusTransition(ctx context.Context, req StatusTransitionRequest) (StatusTransitionResult, error) {
	req.normalize()
	if err := req.validate(); err != nil {
		return StatusTransitionResult{}, err
	}
	return e.processStatusTransitionTx(ctx, req)
}

func (e *engine) processStatusTransitionTx(ctx context.Context, req StatusTransitionRequest) (result StatusTransitionResult, err error) {
	ctx, span := telemetry.StartSpan(ctx, "db.status_transition_tx",
		attribute.String("player_id", req.PlayerID.String()),
		attribute.String("to_status", req.ToStatus))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return StatusTransitionResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The wallet lock. See the header: this is what serializes a transition
	// against a settling wager. The wallet value itself is unused — only the
	// lock matters — and the call doubles as the existence check, returning
	// ErrPlayerNotFound for a player with no wallet row.
	if _, err := selectWalletForUpdate(ctx, tx, req.PlayerID); err != nil {
		return StatusTransitionResult{}, err
	}

	// Current state AND the database clock, in one round trip.
	//
	// now() comes from Postgres rather than the Go process for a reason: the
	// irrevocability boundary is a legal one, and it must not be decidable by
	// whichever pod happens to serve the request. A skewed clock on one replica
	// would otherwise let an exclusion be lifted early there and refused
	// everywhere else. The audit row's created_at defaults to the same clock,
	// so the record and the decision agree by construction.
	var (
		fromStatus   string
		currentUntil *time.Time
		now          time.Time
	)
	err = tx.QueryRow(ctx, sqlSelectPlayerStatusForTransition, req.PlayerID).
		Scan(&fromStatus, &currentUntil, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		// The wallet exists but the user row does not — a provisioning bug. To
		// the caller the player simply is not there.
		return StatusTransitionResult{}, errs.ErrPlayerNotFound
	}
	if err != nil {
		return StatusTransitionResult{}, fmt.Errorf("select player status: %w", err)
	}

	if err := assertTransitionAllowed(fromStatus, req.ToStatus, currentUntil, now); err != nil {
		return StatusTransitionResult{}, err
	}

	// Derive the expiry from the SAME database clock that decided the
	// irrevocability boundary above. A term therefore begins at the instant the
	// transition commits, measured by one authoritative clock, and a request
	// cannot satisfy the floor under one clock and the expiry rule under
	// another.
	var newUntil *time.Time
	if req.ToStatus == StatusSelfExcluded {
		u := now.Add(time.Duration(req.SelfExclusionDays) * 24 * time.Hour).UTC()
		newUntil = &u
		if err := assertSelfExclusionExtends(fromStatus, req.ToStatus, currentUntil, u); err != nil {
			return StatusTransitionResult{}, err
		}
	}

	tag, err := tx.Exec(ctx, sqlUpdatePlayerStatus, req.PlayerID, req.ToStatus, newUntil)
	if err != nil {
		return StatusTransitionResult{}, fmt.Errorf("update player status: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// The row was read under the wallet lock a moment ago, so zero rows here
		// means a schema or routing bug rather than a race. Fail closed.
		return StatusTransitionResult{}, fmt.Errorf("update player status: expected 1 row, got %d", tag.RowsAffected())
	}

	var actorRef *string
	if req.ActorRef != "" {
		actorRef = &req.ActorRef
	}

	var (
		transitionID uuid.UUID
		occurredAt   time.Time
	)
	if err := tx.QueryRow(ctx, sqlInsertStatusTransition,
		req.PlayerID, fromStatus, req.ToStatus, req.ActorType, actorRef, req.Reason, newUntil,
	).Scan(&transitionID, &occurredAt); err != nil {
		// Nothing special to do: the deferred Rollback discards the UPDATE too,
		// which is the whole point of doing both in one transaction. A status
		// change whose audit row failed to write must not survive.
		return StatusTransitionResult{}, fmt.Errorf("insert status transition: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return StatusTransitionResult{}, fmt.Errorf("commit: %w", err)
	}

	span.SetAttributes(
		attribute.String("from_status", fromStatus),
		attribute.String("transition_id", transitionID.String()),
	)

	// Report the term now in force: the one just set, or the one already on the
	// row when this transition did not set one.
	effectiveUntil := newUntil
	if effectiveUntil == nil {
		effectiveUntil = currentUntil
	}

	return StatusTransitionResult{
		PlayerID:           req.PlayerID,
		TransitionID:       transitionID,
		FromStatus:         fromStatus,
		ToStatus:           req.ToStatus,
		SelfExclusionUntil: effectiveUntil,
		OccurredAt:         occurredAt,
	}, nil
}

// ----------------------------------------------------------------------------
// The rules — pure, so they are testable without a database
// ----------------------------------------------------------------------------

// assertTransitionAllowed decides whether a player may move from → to, given the
// self-exclusion term currently on their row and the authoritative clock.
//
// The interesting cases are all about SELF_EXCLUDED. Everything else is
// permissive by design: a compliance system that refuses to suspend a player
// because they are pending KYC is a system operators route around.
func assertTransitionAllowed(from, to string, currentUntil *time.Time, now time.Time) error {
	if from == to {
		// The ONE same-status case that is a real event: a self-excluded player
		// asking for LONGER.
		//
		// A2 shipped without this and it was the wrong way round — a player
		// could shorten their protection (by waiting the term out) but never
		// deepen it. Whether the request actually extends anything is decided by
		// assertSelfExclusionExtends, which needs the requested term and so runs
		// after this; here it is enough that the case is not a no-op.
		if to == StatusSelfExcluded {
			return nil
		}
		return fmt.Errorf("%w: already %s", errs.ErrStatusUnchanged, to)
	}

	// CLOSED is terminal. This is load-bearing rather than tidy: the rule below
	// permits SELF_EXCLUDED → CLOSED at any time, which is only safe because
	// CLOSED cannot itself be left. If closure ever becomes reversible, it
	// becomes a laundering route out of an active exclusion, and this rule and
	// that one must be revisited together. Re-admitting a closed player is a new
	// account and fresh KYC, not a status change.
	if from == StatusClosed {
		return fmt.Errorf("%w: %s is terminal", errs.ErrStatusTransitionInvalid, StatusClosed)
	}

	if from != StatusSelfExcluded {
		return nil
	}

	// ── Leaving a self-exclusion ────────────────────────────────────────────
	//
	// CLOSED is always permitted: it is strictly more restrictive than the
	// exclusion and terminal, so it can never be a route back to play. A player
	// who decides mid-exclusion that they want out entirely must not be told to
	// wait 180 days first.
	if to == StatusClosed {
		return nil
	}

	// Every other destination is refused while the term runs — including
	// SUSPENDED and KYC_PENDING, which are not obviously "returning to play".
	// They are refused because they are LIFTABLE: moving an excluded player to
	// SUSPENDED and then clearing the suspension would launder an irrevocable
	// exclusion into an operator-reversible hold, defeating the control in two
	// steps that each look legitimate in isolation.
	//
	// A NULL term fails closed. 000010's constraint makes that row shape
	// impossible going forward, but a rule that protects a player should not
	// depend on a constraint having been applied.
	if currentUntil == nil {
		return fmt.Errorf("%w: no term recorded; refusing to lift", errs.ErrSelfExclusionActive)
	}
	if currentUntil.After(now) {
		return fmt.Errorf("%w: term runs until %s", errs.ErrSelfExclusionActive, currentUntil.UTC().Format(time.RFC3339))
	}
	return nil
}

// assertSelfExclusionTerm enforces the regulatory floor and the required pairing
// between destination and term.
//
// Pure, and needs no clock: because the term is a day count the engine converts
// against its own clock, the floor is a plain integer comparison rather than a
// subtraction between two instants that may have been measured by different
// machines.
func assertSelfExclusionTerm(to string, days int) error {
	if to != StatusSelfExcluded {
		if days != 0 {
			return fmt.Errorf("%w: self_exclusion_days is only valid with %s", errs.ErrInvalidAmount, StatusSelfExcluded)
		}
		return nil
	}

	// The application half of the invariant 000010 also enforces in the schema.
	// Checked here as well so the caller is told which field is missing rather
	// than handed a bare constraint violation.
	if days <= 0 {
		return fmt.Errorf("%w: %s requires a positive self_exclusion_days", errs.ErrInvalidAmount, StatusSelfExcluded)
	}
	if days < MinSelfExclusionDays {
		return fmt.Errorf("%w: %d days requested, minimum is %d",
			errs.ErrSelfExclusionTooShort, days, MinSelfExclusionDays)
	}
	if days > maxSelfExclusionDays {
		return fmt.Errorf("%w: %d days is implausible (maximum %d); a permanent decision is %s",
			errs.ErrInvalidAmount, days, maxSelfExclusionDays, StatusClosed)
	}
	return nil
}

// assertSelfExclusionExtends guards the one same-status transition that is
// permitted: an extension must genuinely extend.
//
// Without this, "extend my exclusion" is a lift with extra steps — a player
// 170 days into a 180-day term could ask for a fresh 180 days and, if the check
// only looked at the floor, a term ENDING SOONER than the one they were serving
// would be accepted whenever the remaining term exceeded the minimum. The new
// expiry must therefore be strictly later than the one in force, compared on the
// same database clock everything else here uses.
func assertSelfExclusionExtends(from, to string, currentUntil *time.Time, newUntil time.Time) error {
	if from != StatusSelfExcluded || to != StatusSelfExcluded {
		return nil
	}
	// No recorded term: fail closed rather than treat the absence as "anything
	// extends it". 000010's constraint makes this row shape impossible going
	// forward; the rule does not depend on that having been applied.
	if currentUntil == nil {
		return fmt.Errorf("%w: no term recorded to extend", errs.ErrSelfExclusionActive)
	}
	if !newUntil.After(*currentUntil) {
		return fmt.Errorf("%w: requested term ends %s, current term runs to %s",
			errs.ErrSelfExclusionNotExtended,
			newUntil.UTC().Format(time.RFC3339), currentUntil.UTC().Format(time.RFC3339))
	}
	return nil
}

// ----------------------------------------------------------------------------
// Normalization / validation
// ----------------------------------------------------------------------------

func (r *StatusTransitionRequest) normalize() {
	r.ToStatus = strings.ToUpper(strings.TrimSpace(r.ToStatus))
	r.ActorType = strings.ToUpper(strings.TrimSpace(r.ActorType))
	r.ActorRef = strings.TrimSpace(r.ActorRef)
	r.Reason = strings.TrimSpace(r.Reason)
}

func (r StatusTransitionRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if _, ok := validStatuses[r.ToStatus]; !ok {
		return fmt.Errorf("%w: unknown to_status %q", errs.ErrInvalidAmount, r.ToStatus)
	}
	if _, ok := validActorTypes[r.ActorType]; !ok {
		return fmt.Errorf("%w: unknown actor_type %q", errs.ErrInvalidAmount, r.ActorType)
	}
	if r.Reason == "" {
		return fmt.Errorf("%w: reason is required", errs.ErrInvalidAmount)
	}
	if len(r.Reason) > maxTransitionReasonLen {
		return fmt.Errorf("%w: reason exceeds %d characters", errs.ErrInvalidAmount, maxTransitionReasonLen)
	}
	if len(r.ActorRef) > maxTransitionActorRefLen {
		return fmt.Errorf("%w: actor_ref exceeds %d characters", errs.ErrInvalidAmount, maxTransitionActorRefLen)
	}
	// Checked here rather than inside the transaction: a malformed term is a
	// property of the request alone, so it is refused before a connection, a
	// wallet lock, or a transaction is spent on it.
	return assertSelfExclusionTerm(r.ToStatus, r.SelfExclusionDays)
}
