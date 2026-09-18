package repository

// Unit coverage for the status-transition RULES. The rules are deliberately
// pure functions taking an explicit clock, so the regulatory logic — what may
// move where, and when an exclusion may be lifted — is exercised exhaustively
// here without a database, and the integration suite is left to prove the
// properties only Postgres can demonstrate (atomicity, locking, the schema
// backstop).

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

var testNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time {
	t := testNow.Add(d)
	return &t
}

// TestAssertTransitionAllowed_Matrix walks every ordered pair of statuses for a
// player who is NOT under an active exclusion, so the table states the base
// rules in one place.
//
// Note how permissive the non-exclusion rows are, on purpose: a compliance
// system that refuses to suspend a player because they are still pending KYC is
// a system operators learn to route around. The restrictions that matter are
// concentrated on the two statuses that must not be casually left.
func TestAssertTransitionAllowed_Matrix(t *testing.T) {
	t.Parallel()

	all := []string{StatusKYCPending, StatusActive, StatusSuspended, StatusSelfExcluded, StatusClosed}

	// expired: a self-exclusion whose term has run, so SELF_EXCLUDED behaves
	// like any other liftable status. The in-force case has its own test below.
	expired := at(-time.Hour)

	want := map[string]map[string]error{
		StatusKYCPending: {
			StatusKYCPending:   errs.ErrStatusUnchanged,
			StatusActive:       nil, // KYC passed
			StatusSuspended:    nil, // fraud signal before verification
			StatusSelfExcluded: nil, // a player may exclude before KYC completes
			StatusClosed:       nil,
		},
		StatusActive: {
			StatusKYCPending:   nil, // re-verification required
			StatusActive:       errs.ErrStatusUnchanged,
			StatusSuspended:    nil,
			StatusSelfExcluded: nil,
			StatusClosed:       nil,
		},
		StatusSuspended: {
			StatusKYCPending:   nil,
			StatusActive:       nil, // review cleared
			StatusSuspended:    errs.ErrStatusUnchanged,
			StatusSelfExcluded: nil, // protection outranks an operator hold
			StatusClosed:       nil,
		},
		StatusSelfExcluded: {
			StatusKYCPending: nil, // term expired, so no longer protected
			StatusActive:     nil,
			StatusSuspended:  nil,
			// The one permitted same-status pair: a player extending their own
			// exclusion. Permitted HERE only in the sense that it is not a
			// no-op — whether it actually extends anything is
			// assertSelfExclusionExtends' decision, tested below.
			StatusSelfExcluded: nil,
			StatusClosed:       nil,
		},
		StatusClosed: {
			StatusKYCPending:   errs.ErrStatusTransitionInvalid,
			StatusActive:       errs.ErrStatusTransitionInvalid,
			StatusSuspended:    errs.ErrStatusTransitionInvalid,
			StatusSelfExcluded: errs.ErrStatusTransitionInvalid,
			StatusClosed:       errs.ErrStatusUnchanged,
		},
	}

	for _, from := range all {
		for _, to := range all {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				var until *time.Time
				if from == StatusSelfExcluded {
					until = expired
				}
				err := assertTransitionAllowed(from, to, until, testNow)
				expected := want[from][to]
				switch {
				case expected == nil && err != nil:
					t.Errorf("%s → %s must be permitted, got %v", from, to, err)
				case expected != nil && !errors.Is(err, expected):
					t.Errorf("%s → %s: got %v, want %v", from, to, err, expected)
				}
			})
		}
	}
}

// TestAssertTransitionAllowed_SelfExclusionIsIrrevocable is the guardrail the
// whole control rests on.
//
// Every destination is refused while the term runs EXCEPT closure. SUSPENDED and
// KYC_PENDING are the subtle ones: neither obviously "returns the player to
// play", which is exactly why they are dangerous. Both are operator-liftable, so
// permitting them would let an irrevocable exclusion be laundered into a
// reversible hold in two steps that each look legitimate on their own.
func TestAssertTransitionAllowed_SelfExclusionIsIrrevocable(t *testing.T) {
	t.Parallel()

	inForce := at(90 * 24 * time.Hour)

	refused := []string{StatusActive, StatusSuspended, StatusKYCPending}
	for _, to := range refused {
		t.Run("refuses_"+to, func(t *testing.T) {
			err := assertTransitionAllowed(StatusSelfExcluded, to, inForce, testNow)
			if !errors.Is(err, errs.ErrSelfExclusionActive) {
				t.Errorf("SELF_EXCLUDED → %s during the term: got %v, want ErrSelfExclusionActive", to, err)
			}
		})
	}

	// Closure is permitted mid-term: strictly more restrictive, and terminal, so
	// it can never be a route back to play. A player who decides they want out
	// entirely must not be told to wait out the exclusion first.
	t.Run("permits_CLOSED", func(t *testing.T) {
		if err := assertTransitionAllowed(StatusSelfExcluded, StatusClosed, inForce, testNow); err != nil {
			t.Errorf("SELF_EXCLUDED → CLOSED must be permitted mid-term, got %v", err)
		}
	})
}

// TestAssertTransitionAllowed_ExpiryBoundary pins the instant the term ends.
// A boundary that is off by one comparison either traps a player a moment too
// long or releases them a moment early; the second is the one that matters.
func TestAssertTransitionAllowed_ExpiryBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		until   *time.Time
		wantErr bool
	}{
		{"one nanosecond before expiry", at(time.Nanosecond), true},
		{"exactly at expiry", at(0), false},
		{"one nanosecond after expiry", at(-time.Nanosecond), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertTransitionAllowed(StatusSelfExcluded, StatusActive, c.until, testNow)
			if c.wantErr && !errors.Is(err, errs.ErrSelfExclusionActive) {
				t.Errorf("got %v, want ErrSelfExclusionActive", err)
			}
			if !c.wantErr && err != nil {
				t.Errorf("got %v, want nil", err)
			}
		})
	}
}

// TestAssertTransitionAllowed_NilTermFailsClosed covers a row shape that
// migration 000010's constraint now makes impossible. The rule is kept anyway:
// a control that protects a player should not depend on a constraint having been
// applied to the database it is running against.
func TestAssertTransitionAllowed_NilTermFailsClosed(t *testing.T) {
	t.Parallel()

	err := assertTransitionAllowed(StatusSelfExcluded, StatusActive, nil, testNow)
	if !errors.Is(err, errs.ErrSelfExclusionActive) {
		t.Errorf("a SELF_EXCLUDED row with no term must fail closed: got %v", err)
	}
}

// TestAssertSelfExclusionExtends covers the gap A2 shipped with: a self-excluded
// player could not ask for longer.
//
// The rule that matters is the second case. A player 170 days into a 180-day
// term who requests "180 days" is asking for a term that ends SOONER than the
// one they are serving — a lift dressed as an extension. A floor check alone
// would wave it through, because 180 days clears the 180-day minimum.
func TestAssertSelfExclusionExtends(t *testing.T) {
	t.Parallel()

	current := at(10 * 24 * time.Hour) // 10 days left to serve

	cases := []struct {
		name     string
		from, to string
		current  *time.Time
		newUntil time.Time
		wantErr  error
	}{
		{
			"a longer term extends",
			StatusSelfExcluded, StatusSelfExcluded, current,
			testNow.Add(MinSelfExclusionTerm), nil,
		},
		{
			"one nanosecond longer still extends",
			StatusSelfExcluded, StatusSelfExcluded, current,
			current.Add(time.Nanosecond), nil,
		},
		{
			"an identical term is not an extension",
			StatusSelfExcluded, StatusSelfExcluded, current,
			*current, errs.ErrSelfExclusionNotExtended,
		},
		{
			"a shorter term is a lift in disguise",
			StatusSelfExcluded, StatusSelfExcluded, current,
			current.Add(-24 * time.Hour), errs.ErrSelfExclusionNotExtended,
		},
		{
			"no recorded term fails closed",
			StatusSelfExcluded, StatusSelfExcluded, nil,
			testNow.Add(MinSelfExclusionTerm), errs.ErrSelfExclusionActive,
		},
		{
			// A first exclusion is not an extension and this rule must not fire.
			"entering a self-exclusion is unaffected",
			StatusActive, StatusSelfExcluded, nil,
			testNow.Add(MinSelfExclusionTerm), nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertSelfExclusionExtends(c.from, c.to, c.current, c.newUntil)
			switch {
			case c.wantErr == nil && err != nil:
				t.Errorf("got %v, want nil", err)
			case c.wantErr != nil && !errors.Is(err, c.wantErr):
				t.Errorf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

// TestAssertSelfExclusionTerm covers the regulatory floor and the pairing rule.
//
// Note the absence of a clock: because the term is a day count the engine
// converts against its own clock, the floor is an exact integer comparison. The
// earlier timestamp-based form could not be tested this cleanly, and that was a
// symptom — a rule whose outcome depends on how long the request spent in
// transport is a rule with a race in it.
func TestAssertSelfExclusionTerm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		to      string
		days    int
		wantErr error
	}{
		{"exactly the floor is accepted", StatusSelfExcluded, MinSelfExclusionDays, nil},
		{"a year is accepted", StatusSelfExcluded, 365, nil},
		{"one day under the floor is refused", StatusSelfExcluded, MinSelfExclusionDays - 1, errs.ErrSelfExclusionTooShort},
		{"a 30-day exclusion is refused", StatusSelfExcluded, 30, errs.ErrSelfExclusionTooShort},
		{"a 7-day 'cool-off' is refused", StatusSelfExcluded, 7, errs.ErrSelfExclusionTooShort},
		{
			// The application half of the invariant migration 000010 also
			// enforces in the schema.
			"SELF_EXCLUDED with no term is refused",
			StatusSelfExcluded, 0, errs.ErrInvalidAmount,
		},
		{"a negative term is refused", StatusSelfExcluded, -180, errs.ErrInvalidAmount},
		{
			// Catches a unit-confusion bug before it becomes an exclusion
			// nobody can explain. A permanent decision is CLOSED.
			"an implausible term is refused",
			StatusSelfExcluded, maxSelfExclusionDays + 1, errs.ErrInvalidAmount,
		},
		{
			// Silently ignoring it would hide a caller that has confused two
			// operations.
			"a term supplied with SUSPENDED is refused",
			StatusSuspended, MinSelfExclusionDays, errs.ErrInvalidAmount,
		},
		{"no term with SUSPENDED is fine", StatusSuspended, 0, nil},
		{"no term with CLOSED is fine", StatusClosed, 0, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertSelfExclusionTerm(c.to, c.days)
			switch {
			case c.wantErr == nil && err != nil:
				t.Errorf("got %v, want nil", err)
			case c.wantErr != nil && !errors.Is(err, c.wantErr):
				t.Errorf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

// TestMinSelfExclusionTermIsTheRegulatoryFloor states the constant's value
// directly. Loosening it is a policy decision with an evidence base behind it
// (Hopfgartner 2023: 75.3% return after ≤38 days, 0.9% after 90 days–1 year),
// not a tuning knob, so a change has to come here and be argued for.
func TestMinSelfExclusionTermIsTheRegulatoryFloor(t *testing.T) {
	t.Parallel()
	if got, want := MinSelfExclusionDays, 180; got != want {
		t.Errorf("MinSelfExclusionDays = %d, want %d", got, want)
	}
	if got, want := MinSelfExclusionTerm, 180*24*time.Hour; got != want {
		t.Errorf("MinSelfExclusionTerm = %v, want %v (180 days)", got, want)
	}
}

func TestStatusTransitionRequest_Validate(t *testing.T) {
	t.Parallel()

	valid := func() StatusTransitionRequest {
		return StatusTransitionRequest{
			OperatorCode: "OP1",
			PlayerID:     uuid.New(),
			ToStatus:     StatusSuspended,
			ActorType:    "OPERATOR",
			Reason:       "fraud review opened",
		}
	}

	cases := []struct {
		name    string
		mutate  func(*StatusTransitionRequest)
		wantErr error
	}{
		{"valid", func(*StatusTransitionRequest) {}, nil},
		{"empty operator", func(r *StatusTransitionRequest) { r.OperatorCode = "" }, errs.ErrInvalidAmount},
		{"nil player", func(r *StatusTransitionRequest) { r.PlayerID = uuid.Nil }, errs.ErrPlayerNotFound},
		{"unknown status", func(r *StatusTransitionRequest) { r.ToStatus = "BANNED" }, errs.ErrInvalidAmount},
		{"unknown actor", func(r *StatusTransitionRequest) { r.ActorType = "ROBOT" }, errs.ErrInvalidAmount},
		{"empty reason", func(r *StatusTransitionRequest) { r.Reason = "" }, errs.ErrInvalidAmount},
		{
			"over-long reason",
			func(r *StatusTransitionRequest) { r.Reason = repeat('x', maxTransitionReasonLen+1) },
			errs.ErrInvalidAmount,
		},
		{
			"reason at the limit is accepted",
			func(r *StatusTransitionRequest) { r.Reason = repeat('x', maxTransitionReasonLen) },
			nil,
		},
		{
			"over-long actor_ref",
			func(r *StatusTransitionRequest) { r.ActorRef = repeat('y', maxTransitionActorRefLen+1) },
			errs.ErrInvalidAmount,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := valid()
			c.mutate(&r)
			r.normalize()
			err := r.validate()
			switch {
			case c.wantErr == nil && err != nil:
				t.Errorf("got %v, want nil", err)
			case c.wantErr != nil && !errors.Is(err, c.wantErr):
				t.Errorf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

// TestStatusTransitionRequest_Normalize covers the input shapes a real caller
// produces: lower-case enums from a JSON client and padded strings from a form.
func TestStatusTransitionRequest_Normalize(t *testing.T) {
	t.Parallel()

	r := StatusTransitionRequest{
		ToStatus:  "  self_excluded ",
		ActorType: " player ",
		ActorRef:  "  ticket-42  ",
		Reason:    "  player requested a 180-day exclusion  ",
	}
	r.normalize()

	if r.ToStatus != StatusSelfExcluded {
		t.Errorf("ToStatus = %q, want %q", r.ToStatus, StatusSelfExcluded)
	}
	if r.ActorType != "PLAYER" {
		t.Errorf("ActorType = %q, want PLAYER", r.ActorType)
	}
	if r.ActorRef != "ticket-42" {
		t.Errorf("ActorRef = %q, want %q", r.ActorRef, "ticket-42")
	}
	if r.Reason != "player requested a 180-day exclusion" {
		t.Errorf("Reason = %q", r.Reason)
	}
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
