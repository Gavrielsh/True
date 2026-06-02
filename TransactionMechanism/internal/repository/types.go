package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Gavrielsh/TransactionMechanism/internal/domain"
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

	// CreatePlayer provisions a new user row and an empty wallet in one
	// atomic transaction. Idempotent: ON CONFLICT DO NOTHING means retries
	// are safe.
	CreatePlayer(ctx context.Context, req CreatePlayerRequest) error

	// ProcessDeposit credits the player's GC or SC_UNPLAYED balance and
	// posts a DEPOSIT ledger transaction. Same Redis idempotency barrier as
	// the BET/WIN flows.
	ProcessDeposit(ctx context.Context, req DepositRequest) (TxResult, error)

	// ProcessBet runs the golden flow for a bet: Redis idempotency →
	// BEGIN (READ COMMITTED) → SELECT FOR UPDATE → AllocateBet → UPDATE
	// wallets → INSERT ledger_transactions → INSERT ledger_entries →
	// COMMIT → cache response. Includes the Ghost-Spin (23505) recovery
	// branch.
	ProcessBet(ctx context.Context, req BetRequest) (TxResult, error)

	// ProcessWin runs the golden flow for a win (always credits SC_REDEEMABLE
	// for SC family).
	ProcessWin(ctx context.Context, req WinRequest) (TxResult, error)

	// ProcessRollback reverses a previously-committed BET. The original tx is
	// flipped to ROLLED_BACK and an offsetting ROLLBACK ledger_transaction is
	// posted with opposite-direction entries that re-credit the wallet.
	// Same Redis idempotency barrier + 23505 Ghost-Spin recovery as the BET
	// and WIN flows.
	ProcessRollback(ctx context.Context, req RollbackRequest) (TxResult, error)

	// ProcessEscrowReserve locks SC_REDEEMABLE for a pending withdrawal:
	// DEBIT player SC_REDEEMABLE / CREDIT HOUSE_ESCROW_POOL, posting a PENDING
	// ESCROW_RESERVE ledger_transaction. The returned LedgerTransactionID is the
	// escrow_transaction_id the operator persists and later commits/releases.
	ProcessEscrowReserve(ctx context.Context, req EscrowReserveRequest) (TxResult, error)

	// ProcessEscrowCommit finalises an approved withdrawal: it flips the
	// reserve PENDING→COMPLETED and burns the escrowed SC out of
	// HOUSE_ESCROW_POOL into HOUSE_WITHDRAWAL_POOL.
	ProcessEscrowCommit(ctx context.Context, req EscrowCommitRequest) (TxResult, error)

	// ProcessEscrowRelease reverses a rejected withdrawal: it flips the reserve
	// PENDING→ROLLED_BACK and returns the escrowed SC from HOUSE_ESCROW_POOL to
	// the player's SC_REDEEMABLE.
	ProcessEscrowRelease(ctx context.Context, req EscrowReleaseRequest) (TxResult, error)

	// ListTransactions returns the player's most recent ledger transactions
	// (financial source of truth). Backs the operator's transaction-history
	// endpoint so callers never reconstruct history from a secondary store.
	ListTransactions(ctx context.Context, req ListTransactionsRequest) ([]TransactionSummary, error)
}

// CreatePlayerRequest provisions a new player row + wallet. ExternalID is
// the Cassanova MongoDB ObjectId stored as external_id in True's users table.
type CreatePlayerRequest struct {
	PlayerID   uuid.UUID
	ExternalID string // Cassanova MongoDB _id (hex string)
	Email      string
	Username   string
}

// DepositRequest credits the player's wallet and posts a DEPOSIT ledger entry.
// Currency GC → gc_balance; SC → sc_unplayed_balance (promo coins).
type DepositRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	Currency              domain.CurrencyFamily
	Amount                domain.Money
	Metadata              json.RawMessage
	// RequestHash is the lowercase-hex SHA-256 of the raw request body, used by
	// the idempotency barrier to reject a reused operator_transaction_id whose
	// body differs from the original attempt.
	RequestHash string
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
	RequestHash           string                // hex SHA-256 of the raw body
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
	RequestHash            string // hex SHA-256 of the raw body
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
	RequestHash            string // hex SHA-256 of the raw body
}

// EscrowReserveRequest locks SC_REDEEMABLE for a withdrawal. Its own
// OperatorTransactionID is the idempotency anchor; the engine returns the
// created ledger_transactions.id as the escrow_transaction_id.
type EscrowReserveRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	Amount                domain.Money // > 0, drawn from SC_REDEEMABLE only
	Metadata              json.RawMessage
	RequestHash           string // hex SHA-256 of the raw body
}

// EscrowCommitRequest finalises (burns) a reserved withdrawal. EscrowTransactionID
// points at the ESCROW_RESERVE ledger_transactions.id returned by reserve;
// PlayerID is cross-checked against the reserve's owner.
type EscrowCommitRequest struct {
	OperatorCode          string
	OperatorTransactionID string    // the commit's own idempotency anchor
	PlayerID              uuid.UUID // cross-check against the reserve
	EscrowTransactionID   uuid.UUID // the ESCROW_RESERVE ledger tx to commit
	Metadata              json.RawMessage
	RequestHash           string // hex SHA-256 of the raw body
}

// EscrowReleaseRequest reverses a reserved withdrawal, returning the funds to
// the player. Shape mirrors EscrowCommitRequest.
type EscrowReleaseRequest struct {
	OperatorCode          string
	OperatorTransactionID string    // the release's own idempotency anchor
	PlayerID              uuid.UUID // cross-check against the reserve
	EscrowTransactionID   uuid.UUID // the ESCROW_RESERVE ledger tx to release
	Metadata              json.RawMessage
	RequestHash           string // hex SHA-256 of the raw body
}

// ListTransactionsRequest is the validated form of the history query. Limit is
// clamped by the engine to a sane maximum.
type ListTransactionsRequest struct {
	PlayerID        uuid.UUID
	Limit           int    // <= maxTransactionsLimit
	TransactionType string // optional exact filter ("" = all)
	Status          string // optional exact filter ("" = all)
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

// TransactionSummary is one row of a player's transaction history, returned by
// ListTransactions. The financial figures are sourced from the ledger (the
// single source of truth); Amount/Currency/Direction aggregate the
// PLAYER_WALLET entries of the transaction (NULL for house-only rows such as a
// commit's burn, which carry no player-facing amount).
type TransactionSummary struct {
	LedgerTransactionID   uuid.UUID     `json:"ledger_transaction_id"`
	OperatorTransactionID string        `json:"operator_transaction_id"`
	TransactionType       string        `json:"transaction_type"`
	Status                string        `json:"status"`
	GameID                string        `json:"game_id,omitempty"`
	RoundID               string        `json:"round_id,omitempty"`
	Amount                *domain.Money `json:"amount,omitempty"`
	Currency              string        `json:"currency,omitempty"`
	Direction             string        `json:"direction,omitempty"`
	CreatedAt             time.Time     `json:"created_at"`
	CompletedAt           *time.Time    `json:"completed_at,omitempty"`
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
