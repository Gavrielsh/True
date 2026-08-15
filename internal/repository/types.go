package repository

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/Gavrielsh/True/internal/domain"
)

// Engine is the single, cohesive entry point for state-mutating wallet
// operations. It chains the Redis idempotency barrier, pessimistic row
// locking, the pure domain allocators, and double-entry ledger writes into
// one method per operator transaction type.
//
// Every method takes context.Context first and propagates it to Redis and
// pgx unmodified — no internal context.Background() calls during the
// operational path (cursor rule §7).
type Engine interface {
	// GetBalances loads a snapshot of the wallet (no FOR UPDATE lock).
	// Suitable for the /session endpoint where reads can be slightly stale.
	GetBalances(ctx context.Context, playerID uuid.UUID) (domain.Wallet, error)

	// ProcessBet runs the golden flow for a bet: Redis idempotency →
	// BEGIN (READ COMMITTED) → SELECT FOR UPDATE → AllocateBet → UPDATE
	// wallets → INSERT ledger_transactions → INSERT ledger_entries →
	// COMMIT → cache response. Includes the Ghost-Spin (23505) recovery
	// branch.
	ProcessBet(ctx context.Context, req BetRequest) (TxResult, error)

	// ProcessWin runs the golden flow for a win (always credits SC_REDEEMABLE
	// for SC family).
	ProcessWin(ctx context.Context, req WinRequest) (TxResult, error)

	// ProcessRollback reverses a previously-committed BET. The ledger is
	// strictly APPEND-ONLY: the original tx is NEVER mutated — instead an
	// offsetting ROLLBACK ledger_transaction is posted with opposite-direction
	// entries that re-credit the wallet, and that row's reference_transaction_id
	// is the audit-trail marker that the BET has been reversed. Same Redis
	// idempotency barrier + 23505 Ghost-Spin recovery as the BET and WIN flows.
	ProcessRollback(ctx context.Context, req RollbackRequest) (TxResult, error)
}

// BetRequest is the validated, in-engine representation of a /bet call.
// All currency values are already domain.Money — the HTTP layer is
// responsible for the parse-and-reject-bad-precision step.
type BetRequest struct {
	OperatorCode          string // e.g. "PRAGMATIC"
	OperatorTransactionID string // idempotency anchor
	PlayerID              uuid.UUID
	Family                domain.CurrencyFamily // GC or SC
	Amount                domain.Money          // > 0
	GameID                string                // optional ("" = NULL)
	RoundID               string                // optional ("" = NULL)
	Metadata              json.RawMessage       // <= 512 bytes (DB-enforced)
	// BodyHash is the hex SHA-256 of the verified raw request body, set by
	// the HTTP layer. Together with PlayerID it binds the idempotency key to
	// this exact request — see cache.Fingerprint.
	BodyHash string
}

// WinRequest carries the same shape as BetRequest. A win typically references
// the originating bet via ReferenceTransactionID — non-nil for game wins,
// nil for promo / adjustment credits.
type WinRequest struct {
	OperatorCode           string
	OperatorTransactionID  string
	PlayerID               uuid.UUID
	Family                 domain.CurrencyFamily
	Amount                 domain.Money
	GameID                 string
	RoundID                string
	ReferenceTransactionID uuid.UUID // uuid.Nil = NULL
	Metadata               json.RawMessage
	// BodyHash — see BetRequest.BodyHash.
	BodyHash string
}

// RollbackRequest reverses a previously-committed BET. The operator supplies
// its own OperatorTransactionID for the rollback (distinct from the original)
// so the rollback itself is idempotent; ReferenceTransactionID points at the
// BET being reversed.
//
// Currently BET rollbacks only. A WIN rollback would re-debit the wallet,
// which may hit the CHECK (balance >= 0) constraint if the player has spent
// the winnings — that branch is deliberately deferred.
type RollbackRequest struct {
	OperatorCode           string
	OperatorTransactionID  string    // the rollback's own ID
	PlayerID               uuid.UUID // for cross-check against the original
	ReferenceTransactionID uuid.UUID // the BET ledger_transactions.id to reverse
	Metadata               json.RawMessage
	// BodyHash — see BetRequest.BodyHash.
	BodyHash string
}

// ResultStatus discriminates the three terminal outcomes of a Process* call.
// Distinct from cache.AcquireStatus so HTTP code paths don't have to import
// the cache package.
type ResultStatus string

const (
	// StatusProcessed: this call performed the DB transaction.
	StatusProcessed ResultStatus = "PROCESSED"
	// StatusCached: idempotency replay — we returned a previously cached payload.
	StatusCached ResultStatus = "CACHED"
	// StatusGhostRecovered: the DB had already committed this transaction
	// (Postgres 23505 on the operator UNIQUE constraint). Payload was
	// reconstructed from the current DB state.
	StatusGhostRecovered ResultStatus = "GHOST_RECOVERED"
)

// BalanceSummary is the wire-friendly post-state snapshot returned to the
// operator and embedded in the Redis idempotency payload. Money fields are
// serialised as JSON strings (see domain/money.go) to preserve precision.
type BalanceSummary struct {
	GC           domain.Money `json:"gc"`
	SCUnplayed   domain.Money `json:"sc_unplayed"`
	SCRedeemable domain.Money `json:"sc_redeemable"`
}

func balanceSummaryOf(w domain.Wallet) BalanceSummary {
	return BalanceSummary{GC: w.GC, SCUnplayed: w.SCUnplayed, SCRedeemable: w.SCRedeemable}
}

// TxResult is the value returned from any Process* call and the canonical
// payload cached in Redis for idempotent replay.
type TxResult struct {
	OperatorCode          string         `json:"operator_code"`
	OperatorTransactionID string         `json:"operator_transaction_id"`
	LedgerTransactionID   uuid.UUID      `json:"ledger_transaction_id"`
	PlayerID              uuid.UUID      `json:"player_id"`
	TransactionType       string         `json:"transaction_type"` // "BET" / "WIN"
	Family                string         `json:"family"`           // "GC" / "SC"
	Amount                domain.Money   `json:"amount"`
	PostBalances          BalanceSummary `json:"post_balances"`
	Status                ResultStatus   `json:"status"`
}
