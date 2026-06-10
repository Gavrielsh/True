package repository

// casino.go implements the real-money wrapper operations layered on top of the
// core engine: player provisioning, fiat purchases (GC + SC_UNPLAYED promo
// issuance), and fiat redemptions (SC_REDEEMABLE only).
//
// These reuse the exact same money-safety machinery as bet/win/rollback:
//   - Redis idempotency barrier (Acquire → Store/Release).
//   - SELECT ... FOR UPDATE pessimistic wallet lock.
//   - Pure domain allocators (domain/casino.go), run inside the lock window.
//   - Double-entry ledger writes + the global dedup anchor (insertLedgerTx).
//   - 23505 Ghost-Spin recovery.
//
// CasinoEngine is deliberately a SEPARATE interface from Engine so the existing
// Engine consumers (and their test doubles) are untouched; the same concrete
// *engine implements both.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// House pools the wrapper endpoints post against (added in migration 000006).
const (
	accountHouseIssuancePool   = "HOUSE_ISSUANCE_POOL"
	accountHouseRedemptionPool = "HOUSE_REDEMPTION_POOL"
)

// CasinoEngine wraps the core engine with player-management and real-money
// movement operations. Every method takes context.Context first and propagates
// it unmodified, identical to Engine.
type CasinoEngine interface {
	// CreatePlayer provisions a player: a users row and its (zero-balance)
	// wallets row, atomically. Idempotent on external_id — a repeat call
	// returns the existing player with Created=false instead of erroring.
	CreatePlayer(ctx context.Context, req CreatePlayerRequest) (CreatePlayerResult, error)

	// ProcessPurchase issues GC (and optional SC_UNPLAYED promo) against
	// HOUSE_ISSUANCE_POOL for a fiat purchase. Idempotent + Ghost-Spin safe.
	ProcessPurchase(ctx context.Context, req PurchaseRequest) (TxResult, error)

	// ProcessRedeem deducts SC_REDEEMABLE (only) against HOUSE_REDEMPTION_POOL
	// for a fiat redemption. Idempotent + Ghost-Spin safe.
	ProcessRedeem(ctx context.Context, req RedeemRequest) (TxResult, error)
}

// NewCasino builds the casino wrapper from the same dependencies as New. The
// returned value is backed by the same *engine type, so it shares all of the
// engine's locking/ledger/idempotency helpers.
func NewCasino(db DB, idem cache.Store, logger *slog.Logger) CasinoEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &engine{db: db, idem: idem, logger: logger}
}

// ----------------------------------------------------------------------------
// Request / result types
// ----------------------------------------------------------------------------

// CreatePlayerRequest provisions a player. ExternalID is the operator's stable
// player key and the idempotency anchor for creation. The remaining fields are
// optional and defaulted by normalize().
type CreatePlayerRequest struct {
	ExternalID  string // required: operator's unique player id
	Username    string // optional: defaults to ExternalID
	Email       string // optional: "" → NULL
	CountryCode string // optional: defaults "US"; must match ^[A-Z]{2}$
	Status      string // optional: defaults "ACTIVE"
}

// CreatePlayerResult reports the provisioned (or pre-existing) player.
type CreatePlayerResult struct {
	PlayerID uuid.UUID      `json:"player_id"`
	Created  bool           `json:"created"` // false = already existed (idempotent replay)
	Balances BalanceSummary `json:"balances"`
}

// PurchaseRequest is a validated fiat purchase. GCAmount is the Gold Coin
// package the player bought; SCPromoAmount is the SC_UNPLAYED promo granted
// alongside it (may be zero).
type PurchaseRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	GCAmount              domain.Money    // > 0 (the purchased package)
	SCPromoAmount         domain.Money    // >= 0 (promotional SC_UNPLAYED)
	Metadata              json.RawMessage // <= 512 bytes (DB-enforced)
}

// RedeemRequest is a validated fiat redemption. Amount is drawn EXCLUSIVELY
// from SC_REDEEMABLE.
type RedeemRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	Amount                domain.Money // > 0, SC_REDEEMABLE only
	Metadata              json.RawMessage
}

// ----------------------------------------------------------------------------
// SQL — player/wallet provisioning. (Wallet read/lock/update + ledger SQL are
// shared with the core engine via queries.go.)
// ----------------------------------------------------------------------------

const (
	sqlSelectUserIDByExternal = `SELECT id FROM users WHERE external_id = $1`

	sqlInsertUser = `
		INSERT INTO users (external_id, username, email, country_code, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`

	// ON CONFLICT keeps creation idempotent and self-healing: a prior attempt
	// that created the user but failed before the wallet leaves no orphan.
	sqlInsertWalletIfAbsent = `
		INSERT INTO wallets (player_id) VALUES ($1)
		ON CONFLICT (player_id) DO NOTHING
	`
)

var countryCodeRe = regexp.MustCompile(`^[A-Z]{2}$`)

// ----------------------------------------------------------------------------
// CreatePlayer
// ----------------------------------------------------------------------------

func (e *engine) CreatePlayer(ctx context.Context, req CreatePlayerRequest) (CreatePlayerResult, error) {
	req.normalize()
	if err := req.validate(); err != nil {
		return CreatePlayerResult{}, err
	}

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CreatePlayerResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency: a player with this external_id already exists? We check
	// BEFORE inserting so a retried create returns the existing player rather
	// than aborting the transaction on a unique violation (a failed statement
	// poisons the surrounding tx in Postgres).
	var (
		playerID uuid.UUID
		created  bool
	)
	err = tx.QueryRow(ctx, sqlSelectUserIDByExternal, req.ExternalID).Scan(&playerID)
	switch {
	case err == nil:
		created = false
	case errors.Is(err, pgx.ErrNoRows):
		created = true
		err = tx.QueryRow(ctx, sqlInsertUser,
			req.ExternalID,
			req.Username,
			nullableString(req.Email),
			req.CountryCode,
			req.Status,
		).Scan(&playerID)
		if err != nil {
			if isUniqueViolation(err) {
				// A racing create won, or username/email collides with a
				// different player. Either way the caller should retry (a
				// same-external_id retry then hits the idempotent path above).
				return CreatePlayerResult{}, fmt.Errorf(
					"%w: external_id, username, or email already in use", errs.ErrTransactionConflict)
			}
			return CreatePlayerResult{}, fmt.Errorf("insert user: %w", err)
		}
	default:
		return CreatePlayerResult{}, fmt.Errorf("lookup user: %w", err)
	}

	// Ensure the wallet row exists (no-op if a prior attempt already made it).
	if _, err := tx.Exec(ctx, sqlInsertWalletIfAbsent, playerID); err != nil {
		return CreatePlayerResult{}, fmt.Errorf("insert wallet: %w", err)
	}

	var gc, scU, scR decimal.Decimal
	if err := tx.QueryRow(ctx, sqlSelectWalletBalances, playerID).Scan(&gc, &scU, &scR); err != nil {
		return CreatePlayerResult{}, fmt.Errorf("select wallet: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreatePlayerResult{}, fmt.Errorf("commit: %w", err)
	}

	w, err := walletFromDecimals(playerID, gc, scU, scR)
	if err != nil {
		return CreatePlayerResult{}, err
	}
	return CreatePlayerResult{
		PlayerID: playerID,
		Created:  created,
		Balances: balanceSummaryOf(w),
	}, nil
}

// ----------------------------------------------------------------------------
// ProcessPurchase — fiat purchase issuing GC (+ SC_UNPLAYED promo).
// ----------------------------------------------------------------------------

func (e *engine) ProcessPurchase(ctx context.Context, req PurchaseRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	status, payload, err := e.idem.Acquire(ctx, idemKey)
	if err != nil {
		return TxResult{}, fmt.Errorf("idempotency acquire: %w", err)
	}
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	result, err := e.processPurchaseTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}
	e.cacheResultQuietly(ctx, idemKey, result)
	return result, nil
}

func (e *engine) processPurchaseTx(ctx context.Context, req PurchaseRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// KYC/compliance guard — same tx handle, serialized by the lock above.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}

	alloc, err := wallet.AllocatePurchase(req.GCAmount, req.SCPromoAmount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyPurchase(alloc)

	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "DEPOSIT",
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "DEPOSIT", domain.FamilyUnknown, req.GCAmount)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}

	// Double-entry: each issued currency is a player CREDIT balanced by a
	// HOUSE_ISSUANCE_POOL DEBIT of the same amount.
	for _, c := range alloc.Credits {
		balanceAfter := post.BalanceFor(c.Currency)
		if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, c.Currency, "CREDIT", c.Amount, balanceAfter); err != nil {
			return TxResult{}, err
		}
		if err := insertHouseEntry(ctx, tx, ledgerTxID, accountHouseIssuancePool, c.Currency, "DEBIT", c.Amount); err != nil {
			return TxResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return TxResult{}, fmt.Errorf("commit: %w", err)
	}

	return TxResult{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              req.PlayerID,
		TransactionType:       "DEPOSIT",
		Family:                "", // multi-currency (GC + optional SC_UNPLAYED)
		Amount:                req.GCAmount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// ----------------------------------------------------------------------------
// ProcessRedeem — fiat redemption, SC_REDEEMABLE only.
// ----------------------------------------------------------------------------

func (e *engine) ProcessRedeem(ctx context.Context, req RedeemRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	status, payload, err := e.idem.Acquire(ctx, idemKey)
	if err != nil {
		return TxResult{}, fmt.Errorf("idempotency acquire: %w", err)
	}
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	result, err := e.processRedeemTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}
	e.cacheResultQuietly(ctx, idemKey, result)
	return result, nil
}

func (e *engine) processRedeemTx(ctx context.Context, req RedeemRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// KYC/compliance guard — same tx handle, serialized by the lock above.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}

	// AllocateRedeem enforces SC_REDEEMABLE-only and rejects on insufficient
	// redeemable balance even if SC_UNPLAYED would otherwise cover it.
	alloc, err := wallet.AllocateRedeem(req.Amount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyRedeem(alloc)

	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "WITHDRAWAL",
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "WITHDRAWAL", domain.FamilySC, req.Amount)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}

	// Double-entry: player SC_REDEEMABLE DEBIT balanced by HOUSE_REDEMPTION_POOL CREDIT.
	balanceAfter := post.BalanceFor(alloc.Debit.Currency)
	if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, alloc.Debit.Currency, "DEBIT", alloc.Debit.Amount, balanceAfter); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, accountHouseRedemptionPool, alloc.Debit.Currency, "CREDIT", alloc.Debit.Amount); err != nil {
		return TxResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TxResult{}, fmt.Errorf("commit: %w", err)
	}

	return TxResult{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              req.PlayerID,
		TransactionType:       "WITHDRAWAL",
		Family:                "SC",
		Amount:                req.Amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// ----------------------------------------------------------------------------
// Validation / normalization
// ----------------------------------------------------------------------------

// validUserStatuses mirrors the user_status ENUM (migration 000001).
var validUserStatuses = map[string]struct{}{
	"KYC_PENDING": {}, "ACTIVE": {}, "SUSPENDED": {}, "CLOSED": {},
}

func (r *CreatePlayerRequest) normalize() {
	r.ExternalID = strings.TrimSpace(r.ExternalID)
	r.Username = strings.TrimSpace(r.Username)
	if r.Username == "" {
		r.Username = r.ExternalID
	}
	r.Email = strings.TrimSpace(r.Email)
	r.CountryCode = strings.ToUpper(strings.TrimSpace(r.CountryCode))
	if r.CountryCode == "" {
		r.CountryCode = "US"
	}
	r.Status = strings.ToUpper(strings.TrimSpace(r.Status))
	if r.Status == "" {
		r.Status = "ACTIVE"
	}
}

func (r CreatePlayerRequest) validate() error {
	if r.ExternalID == "" {
		return fmt.Errorf("%w: empty external_id", errs.ErrInvalidAmount)
	}
	if !countryCodeRe.MatchString(r.CountryCode) {
		return fmt.Errorf("%w: country_code must be 2 uppercase letters, got %q", errs.ErrInvalidAmount, r.CountryCode)
	}
	if _, ok := validUserStatuses[r.Status]; !ok {
		return fmt.Errorf("%w: invalid status %q", errs.ErrInvalidAmount, r.Status)
	}
	return nil
}

func (r PurchaseRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if r.GCAmount.IsNegative() || r.SCPromoAmount.IsNegative() {
		return fmt.Errorf("%w: purchase amounts must be >= 0", errs.ErrInvalidAmount)
	}
	if !r.GCAmount.IsPositive() && !r.SCPromoAmount.IsPositive() {
		return fmt.Errorf("%w: purchase must issue a positive GC and/or SC_UNPLAYED amount", errs.ErrInvalidAmount)
	}
	return nil
}

func (r RedeemRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if !r.Amount.IsPositive() {
		return fmt.Errorf("%w: redeem amount must be > 0", errs.ErrInvalidAmount)
	}
	return nil
}
