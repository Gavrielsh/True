package repository

// refund.go — ProcessRedemptionRefund: giving back SC_REDEEMABLE that a
// redemption took and never paid out.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS
// ─────────────────────────────────────────────────────────────────────────────
// A redemption debits SC_REDEEMABLE the moment the player asks, because that is
// what stops the same balance being redeemed twice while review is pending. If
// the payout is then refused — by an operator at review, or terminally by the
// payout rail — the player is left debited and unpaid. Without this primitive
// the only honest thing Zone 2 could do was mark the request stuck and page a
// human, which is not a control, it is an IOU.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT IT DELIBERATELY DOES NOT CHECK
// ─────────────────────────────────────────────────────────────────────────────
// There is NO requirePlayerActive here, and its absence is the most important
// line in this file.
//
// Every other money path refuses a SUSPENDED or SELF_EXCLUDED player, and must.
// This one must not. A player who self-excludes, or is suspended pending an
// investigation, after asking to redeem is precisely the player most likely to
// have a payout refused — and blocking their refund would mean a player
// protection mechanism confiscating their money. The status controls exist to
// stop money LEAVING on a blocked player's behalf. Returning money that was
// already taken from them is the opposite act.
//
// There is no playthrough check either: the wagering requirement gates turning
// SC into cash, and this is un-taking a withdrawal, not making one.
//
// There is no balance check: AllocateRedemptionRefund cannot fail for want of
// funds, because the money being returned was already taken from this wallet.
//
// ─────────────────────────────────────────────────────────────────────────────
// IDEMPOTENCY
// ─────────────────────────────────────────────────────────────────────────────
// Identical to every other money path, and load-bearing in the same way: the
// caller's operator_transaction_id (Zone 2 derives it from the redemption id, so
// it is deterministic) passes through the Redis barrier, the
// ledger_transaction_dedup UNIQUE, and Ghost-Spin recovery on 23505. A refund
// credited twice is money invented, so the three layers matter here exactly as
// much as they do for a debit.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// txTypeRedemptionRefund is the ledger transaction type added by migration
// 000013. Distinct from ROLLBACK (which reverses a BET) and from ADJUSTMENT
// (manual correction) so "how much did we take and not pay?" stays a WHERE
// clause rather than a forensic exercise.
const txTypeRedemptionRefund = "REDEMPTION_REFUND"

// RedemptionRefundRequest returns a previously-redeemed amount to the player.
//
// Amount must be EXACTLY what the redemption debited. The engine does not look
// the original up and cannot verify the figure — it credits what it is told —
// so the caller passing a different number is the one failure mode this design
// cannot catch. Zone 2 passes the stored decimal string verbatim for that
// reason, never a recomputed one.
type RedemptionRefundRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	Amount                domain.Money // > 0; the exact amount originally debited
	// ReferenceTransactionID is the redemption's ledger transaction, recorded on
	// the refund so the pair can be read as one story. Optional (uuid.Nil when
	// absent, the codebase's idiom): a refund whose original debit id was lost is
	// still better than no refund.
	ReferenceTransactionID uuid.UUID
	Metadata               json.RawMessage
	// BodyHash — see BetRequest.BodyHash.
	BodyHash string
}

func (r RedemptionRefundRequest) validate() error {
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
		return fmt.Errorf("%w: refund amount must be > 0", errs.ErrInvalidAmount)
	}
	return nil
}

// ProcessRedemptionRefund credits SC_REDEEMABLE and debits
// HOUSE_REDEMPTION_POOL — the exact reverse of ProcessRedeem's entries, so the
// pool nets to zero for a redemption that was taken and given back.
func (e *engine) ProcessRedemptionRefund(ctx context.Context, req RedemptionRefundRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)
	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		// FAIL CLOSED — never credit when the idempotency barrier is down.
		return TxResult{}, mapIdempotencyErr(req.OperatorCode, err)
	}
	//nolint:exhaustive // StatusAcquired and StatusUnknown fall THROUGH to the DB
	// phase, exactly as in every other money path.
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	result, err := e.processRedemptionRefundTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}
	e.cacheResultQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (e *engine) processRedemptionRefundTx(ctx context.Context, req RedemptionRefundRequest) (result TxResult, err error) {
	ctx, span := telemetry.StartSpan(ctx, "db.redemption_refund_tx",
		attribute.String("player_id", req.PlayerID.String()))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The same wallet lock every money path takes, so a refund serializes with a
	// concurrent wager rather than racing it.
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// No status guard, no playthrough guard — see the file header. This is the
	// one money path that must reach a blocked player.

	alloc, err := wallet.AllocateRedemptionRefund(req.Amount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyRedemptionRefund(alloc)

	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  txTypeRedemptionRefund,
		Reference:             req.ReferenceTransactionID,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Ghost recovery: a refund that already committed returns ITS receipt
			// rather than crediting a second time.
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, txTypeRedemptionRefund, domain.FamilySC, req.Amount, req.BodyHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}
	span.SetAttributes(attribute.String("ledger_transaction_id", ledgerTxID.String()))

	// Double-entry, the mirror of ProcessRedeem: player SC_REDEEMABLE CREDIT
	// balanced by HOUSE_REDEMPTION_POOL DEBIT.
	balanceAfter := post.BalanceFor(alloc.Credit.Currency)
	if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, alloc.Credit.Currency, "CREDIT", alloc.Credit.Amount, balanceAfter); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, accountHouseRedemptionPool, alloc.Credit.Currency, "DEBIT", alloc.Credit.Amount); err != nil {
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
		TransactionType:       txTypeRedemptionRefund,
		Family:                "SC",
		Amount:                req.Amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}
