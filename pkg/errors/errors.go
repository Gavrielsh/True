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
	default:
		return CodeInternal
	}
}
