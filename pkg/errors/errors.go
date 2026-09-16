// Package errors holds the sentinel domain errors and the stable, public
// error codes returned to operators.
//
// Compare sentinel errors with the stdlib `errors.Is`. Surface `Code` to
// clients — NEVER the wrapped error string, which may carry PII or internal
// state (per cursor rule §9: DATA PRIVACY).
package errors

import (
	stderrors "errors"
)

// Sentinel domain errors. Wrap with fmt.Errorf("%w: ...", Err...) to attach
// per-call context; the wrap chain keeps `errors.Is` working.
var (
	ErrInsufficientFunds   = stderrors.New("insufficient funds")
	ErrInvalidAmount       = stderrors.New("invalid amount")
	ErrUnsupportedCurrency = stderrors.New("unsupported currency")
	ErrPlayerNotFound      = stderrors.New("player not found")
	ErrPlayerNotActive     = stderrors.New("player not active")
	ErrTransactionPending  = stderrors.New("duplicate transaction in-flight")
	ErrTransactionConflict = stderrors.New("transaction id conflict")
	ErrRollbackNotFound    = stderrors.New("rollback target not found")
	ErrRollbackAlready     = stderrors.New("transaction already rolled back")
	ErrRollbackUnsupported = stderrors.New("rollback not supported for this transaction type")

	// ErrUnsupportedGame: the requested game_id has no registered paytable.
	// Rejected outright rather than falling back to a default, so a caller
	// cannot shop for a better-paying table with a bogus id.
	ErrUnsupportedGame = stderrors.New("unsupported game")
	// ErrRNGUnavailable: the entropy source failed. A spin that cannot be
	// drawn securely is not drawn at all — never degrade to a weaker source.
	ErrRNGUnavailable = stderrors.New("random number generator unavailable")
	// ErrIdempotencyMismatch: the idempotency key was reused with a
	// materially different request (different player, amount, or currency).
	ErrIdempotencyMismatch = stderrors.New("idempotency key reused with a different request")
	// ErrWinExceedsCeiling: a third-party WIN credit exceeded the operator's
	// configured absolute ceiling. Signals a provider bug or a compromised
	// webhook secret — always alert, never auto-raise the ceiling.
	ErrWinExceedsCeiling = stderrors.New("win exceeds configured ceiling")

	// ─── Player status transitions (migration 000009/000010) ────────────────
	// Each of these names a DISTINCT rule the caller violated. They are kept
	// separate rather than folded into one "bad transition" error because the
	// caller's correct response differs for each: retry never helps for an
	// illegal transition, helps only after a date for an active exclusion, and
	// is already unnecessary for an unchanged status.

	// ErrStatusUnchanged: the player already holds the requested status. The
	// audit trail records CHANGES, so a no-op is refused rather than written.
	// Zone 2 should treat this as an idempotent success when replaying a
	// transition it may already have applied.
	ErrStatusUnchanged = stderrors.New("player already holds the requested status")
	// ErrStatusTransitionInvalid: the move is not permitted from the player's
	// current status (e.g. anything out of CLOSED, which is terminal).
	ErrStatusTransitionInvalid = stderrors.New("status transition not permitted")
	// ErrSelfExclusionActive: the player is self-excluded and the term has not
	// expired. A self-exclusion is IRREVOCABLE before its expiry — no operator,
	// and no request from the player themselves, may lift it. Returned for every
	// move that would restore play, and for every move to another liftable
	// status, which would otherwise launder an exclusion into something an
	// operator can clear.
	ErrSelfExclusionActive = stderrors.New("self-exclusion is in force")
	// ErrSelfExclusionTooShort: the requested term is below the regulatory
	// minimum. Named separately from generic validation because the caller must
	// be told WHICH rule to satisfy in order to resubmit.
	ErrSelfExclusionTooShort = stderrors.New("self-exclusion term is below the minimum")
	// ErrSelfExclusionNotExtended: a self-excluded player asked to change their
	// term to one that does not extend it. Refused as an attempt to shorten an
	// exclusion, which is the same act as lifting it early.
	ErrSelfExclusionNotExtended = stderrors.New("self-exclusion term must be extended, not shortened")

	// ErrLimitExceeded: the wager would take the player past a limit they set
	// themselves. Distinct from ErrInsufficientFunds — the money is there, and
	// the player asked us not to let them spend it.
	ErrLimitExceeded = stderrors.New("player limit exceeded")
)

// Code is the stable identifier surfaced to operators (e.g. in webhook
// responses and audit logs). These strings are part of the public API
// contract and MUST NOT be renamed without a versioning event.
type Code string

const (
	CodeOK                  Code = "OK"
	CodeInsufficientFunds   Code = "INSUFFICIENT_FUNDS"
	CodeInvalidAmount       Code = "INVALID_AMOUNT"
	CodeUnsupportedCurrency Code = "UNSUPPORTED_CURRENCY"
	CodePlayerNotFound      Code = "PLAYER_NOT_FOUND"
	CodePlayerNotActive     Code = "PLAYER_NOT_ACTIVE"
	CodeTransactionPending  Code = "TRANSACTION_PENDING"
	CodeTransactionConflict Code = "TRANSACTION_CONFLICT"
	CodeRollbackNotFound    Code = "ROLLBACK_NOT_FOUND"
	CodeRollbackAlready     Code = "ROLLBACK_ALREADY"
	CodeRollbackUnsupported Code = "ROLLBACK_UNSUPPORTED"
	CodeUnsupportedGame     Code = "UNSUPPORTED_GAME"
	CodeRNGUnavailable      Code = "RNG_UNAVAILABLE"
	CodeIdempotencyMismatch Code = "IDEMPOTENCY_KEY_REUSED"
	CodeWinExceedsCeiling   Code = "WIN_EXCEEDS_CEILING"

	CodeStatusUnchanged          Code = "STATUS_UNCHANGED"
	CodeStatusTransitionInvalid  Code = "STATUS_TRANSITION_INVALID"
	CodeSelfExclusionActive      Code = "SELF_EXCLUSION_ACTIVE"
	CodeSelfExclusionTooShort    Code = "SELF_EXCLUSION_TOO_SHORT"
	CodeSelfExclusionNotExtended Code = "SELF_EXCLUSION_NOT_EXTENDED"
	CodeLimitExceeded            Code = "PLAYER_LIMIT_EXCEEDED"
	// CodeGeoBlocked is returned by the jurisdiction fence (no sentinel error:
	// the middleware rejects before any domain call).
	CodeGeoBlocked Code = "GEO_BLOCKED"
	CodeInternal   Code = "INTERNAL_ERROR"
)

// CodeFor maps any error to its public code. Returns CodeOK for nil and
// CodeInternal for any error that doesn't wrap a known sentinel.
func CodeFor(err error) Code {
	switch {
	case err == nil:
		return CodeOK
	case stderrors.Is(err, ErrInsufficientFunds):
		return CodeInsufficientFunds
	case stderrors.Is(err, ErrInvalidAmount):
		return CodeInvalidAmount
	case stderrors.Is(err, ErrUnsupportedCurrency):
		return CodeUnsupportedCurrency
	case stderrors.Is(err, ErrPlayerNotFound):
		return CodePlayerNotFound
	case stderrors.Is(err, ErrPlayerNotActive):
		return CodePlayerNotActive
	case stderrors.Is(err, ErrTransactionPending):
		return CodeTransactionPending
	case stderrors.Is(err, ErrTransactionConflict):
		return CodeTransactionConflict
	case stderrors.Is(err, ErrRollbackNotFound):
		return CodeRollbackNotFound
	case stderrors.Is(err, ErrRollbackAlready):
		return CodeRollbackAlready
	case stderrors.Is(err, ErrRollbackUnsupported):
		return CodeRollbackUnsupported
	case stderrors.Is(err, ErrUnsupportedGame):
		return CodeUnsupportedGame
	case stderrors.Is(err, ErrRNGUnavailable):
		return CodeRNGUnavailable
	case stderrors.Is(err, ErrIdempotencyMismatch):
		return CodeIdempotencyMismatch
	case stderrors.Is(err, ErrWinExceedsCeiling):
		return CodeWinExceedsCeiling
	case stderrors.Is(err, ErrStatusUnchanged):
		return CodeStatusUnchanged
	case stderrors.Is(err, ErrStatusTransitionInvalid):
		return CodeStatusTransitionInvalid
	case stderrors.Is(err, ErrSelfExclusionActive):
		return CodeSelfExclusionActive
	case stderrors.Is(err, ErrSelfExclusionTooShort):
		return CodeSelfExclusionTooShort
	case stderrors.Is(err, ErrSelfExclusionNotExtended):
		return CodeSelfExclusionNotExtended
	case stderrors.Is(err, ErrLimitExceeded):
		return CodeLimitExceeded
	default:
		return CodeInternal
	}
}
