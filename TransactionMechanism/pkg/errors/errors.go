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
	CodeInternal            Code = "INTERNAL_ERROR"
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
	default:
		return CodeInternal
	}
}
