// Package api is the HTTP layer: Gin router, security middlewares, and the
// JSON adapters that translate operator wire formats to repository.Engine
// requests. The HTTP layer is deliberately thin — every business decision
// lives in domain or repository; this package only validates, marshals, and
// maps error codes to status codes.
package api

import (
	"encoding/json"

	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/pkg/errors"
)

// betRequestDTO is the wire format consumed by POST /api/v1/bet.
// Currency is the high-level family ("GC" or "SC"); the engine controls
// SC_UNPLAYED-first routing. Amount is a JSON string ("12.3400") to keep
// 4-decimal precision through JS / Java decoders.
type betRequestDTO struct {
	OperatorTransactionID string          `json:"operator_transaction_id" binding:"required"`
	PlayerID              string          `json:"player_id"               binding:"required"`
	Currency              string          `json:"currency"                binding:"required"`
	Amount                string          `json:"amount"                  binding:"required"`
	GameID                string          `json:"game_id,omitempty"`
	RoundID               string          `json:"round_id,omitempty"`
	Metadata              json.RawMessage `json:"metadata,omitempty"`
}

// winRequestDTO mirrors betRequestDTO; SC wins always credit SC_REDEEMABLE.
// reference_transaction_id is optional and points at the originating BET.
type winRequestDTO struct {
	OperatorTransactionID  string          `json:"operator_transaction_id"  binding:"required"`
	PlayerID               string          `json:"player_id"                binding:"required"`
	Currency               string          `json:"currency"                 binding:"required"`
	Amount                 string          `json:"amount"                   binding:"required"`
	GameID                 string          `json:"game_id,omitempty"`
	RoundID                string          `json:"round_id,omitempty"`
	ReferenceTransactionID string          `json:"reference_transaction_id,omitempty"`
	Metadata               json.RawMessage `json:"metadata,omitempty"`
}

// rollbackRequestDTO reverses a previously-committed BET.
// reference_transaction_id is the BET's ledger_transactions.id.
type rollbackRequestDTO struct {
	OperatorTransactionID  string          `json:"operator_transaction_id"  binding:"required"`
	PlayerID               string          `json:"player_id"                binding:"required"`
	ReferenceTransactionID string          `json:"reference_transaction_id" binding:"required"`
	Metadata               json.RawMessage `json:"metadata,omitempty"`
}

// sessionRequestDTO is the wire format consumed by POST /api/v1/session.
// The player id travels in the BODY — not the query string — so it is covered
// by the raw-body HMAC signature (a GET's empty body would MAC to the constant
// HMAC(secret, ""), binding the signature to nothing).
type sessionRequestDTO struct {
	PlayerID string `json:"player_id" binding:"required"`
}

// successResponse is the wire format returned on a 2xx response.
// Embeds the engine's TxResult so Money fields keep their string encoding.
type successResponse struct {
	Code   errors.Code         `json:"code"` // always "OK"
	Result repository.TxResult `json:"result"`
}

// errorResponse is the wire format for any non-2xx response. The Message
// is for operator-facing diagnostics; engineers grep by Code which is
// stable and never carries PII.
type errorResponse struct {
	Code    errors.Code `json:"code"`
	Message string      `json:"message"`
	TraceID string      `json:"trace_id,omitempty"`
}

// balancesResponse is the POST /api/v1/session payload.
type balancesResponse struct {
	Code     errors.Code               `json:"code"`
	PlayerID string                    `json:"player_id"`
	Balances repository.BalanceSummary `json:"balances"`
}
