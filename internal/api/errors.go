package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Gavrielsh/True/pkg/errors"
)

// httpStatusFor maps a domain error code to an HTTP status. Mappings are
// designed so an operator's automatic retry policy can be driven entirely
// off the status code:
//
//	400  malformed input or business-rule violation (don't retry)
//	401  authentication failure (don't retry — fix the request)
//	403  player is not allowed to transact (don't retry)
//	404  player or rollback target does not exist (don't retry)
//	409  duplicate / concurrent / conflict (retry safely)
//	500  internal failure — retry with backoff
func httpStatusFor(code errors.Code) int {
	switch code {
	case errors.CodeOK:
		return http.StatusOK
	case errors.CodeInsufficientFunds:
		return http.StatusBadRequest
	case errors.CodeInvalidAmount:
		return http.StatusBadRequest
	case errors.CodeUnsupportedCurrency:
		return http.StatusBadRequest
	case errors.CodePlayerNotFound:
		return http.StatusNotFound
	case errors.CodePlayerNotActive:
		return http.StatusForbidden
	case errors.CodeTransactionPending:
		return http.StatusConflict
	case errors.CodeTransactionConflict:
		return http.StatusConflict
	case errors.CodeRollbackNotFound:
		return http.StatusNotFound
	case errors.CodeRollbackAlready:
		return http.StatusConflict
	case errors.CodeRollbackUnsupported:
		return http.StatusUnprocessableEntity
	case errors.CodeUnsupportedGame:
		return http.StatusBadRequest
	case errors.CodeWinExceedsCeiling:
		// 422, not 400: the request is well-formed but refused by policy.
		// Terminal — an operator retry will not help, a human must look.
		return http.StatusUnprocessableEntity
	case errors.CodeIdempotencyMismatch:
		// 409 per the audit's requirement: the key is in use by a DIFFERENT
		// request. Distinct from CodeTransactionPending, which means the SAME
		// request is still in flight.
		return http.StatusConflict
	case errors.CodeRNGUnavailable:
		// 503 — transient infrastructure failure; the spin was NOT settled
		// and the same key is safe to retry.
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// respondError writes the canonical error envelope.
//
// Per cursor rule §9 (DATA PRIVACY), the wrapped error chain is NOT
// surfaced to the client — only the stable Code and a short Message.
// Internal errors hide their wrapped detail entirely; only structured
// log entries get the underlying error string.
func respondError(c *gin.Context, err error) {
	code := errors.CodeFor(err)
	status := httpStatusFor(code)

	msg := publicMessageFor(code)
	body := errorResponse{
		Code:    code,
		Message: msg,
		TraceID: traceIDFrom(c),
	}
	c.AbortWithStatusJSON(status, body)
}

// respondErrorCode is for middleware that wants to surface a specific code
// (e.g. the HMAC middleware) without owning a sentinel error.
func respondErrorCode(c *gin.Context, status int, code errors.Code, message string) {
	c.AbortWithStatusJSON(status, errorResponse{
		Code:    code,
		Message: message,
		TraceID: traceIDFrom(c),
	})
}

// publicMessageFor returns a short, operator-safe description for each code.
// Kept tight on purpose — these strings appear in webhook responses and we
// never want to leak DB schema names, SQL fragments, or PII.
func publicMessageFor(code errors.Code) string {
	switch code {
	case errors.CodeOK:
		return "ok"
	case errors.CodeInsufficientFunds:
		return "insufficient funds"
	case errors.CodeInvalidAmount:
		return "invalid request"
	case errors.CodeUnsupportedCurrency:
		return "unsupported currency"
	case errors.CodePlayerNotFound:
		return "player not found"
	case errors.CodePlayerNotActive:
		return "player not active"
	case errors.CodeTransactionPending:
		return "transaction already in flight"
	case errors.CodeTransactionConflict:
		return "transaction conflict"
	case errors.CodeRollbackNotFound:
		return "rollback target not found"
	case errors.CodeRollbackAlready:
		return "transaction already rolled back"
	case errors.CodeRollbackUnsupported:
		return "rollback not supported for this transaction type"
	case errors.CodeUnsupportedGame:
		return "unknown game"
	case errors.CodeWinExceedsCeiling:
		return "win exceeds permitted ceiling"
	case errors.CodeIdempotencyMismatch:
		return "idempotency key already used for a different request"
	case errors.CodeRNGUnavailable:
		return "game temporarily unavailable"
	default:
		return "internal error"
	}
}
