package repository

// promo.go implements POST /api/v1/store/promo-grant: coins issued with NO
// purchase behind them — the statutory free entry (AMOE), marketing bonuses
// (daily wheel, missions) and goodwill compensation.
//
// It is the purchase flow with three differences, and nothing else:
//   - the ledger transaction is PROMO_CREDIT (not DEPOSIT), so a free grant is
//     never booked as revenue or counted as a purchase;
//   - the counterparty is HOUSE_PROMO_POOL (not HOUSE_ISSUANCE_POOL);
//   - a typed promo_grants row (channel + channel_reference) is written in the
//     same database transaction, so the legal route of every grant is recorded
//     in a column, not a convention.
//
// Everything else is the shared money-safety machinery: the Redis idempotency
// barrier, SELECT ... FOR UPDATE, the player-status guard, the double-entry
// ledger with its global dedup anchor, and 23505 Ghost-Spin recovery.
//
// THE ENGINE DOES NOT CAP GRANTS. It records what an authenticated operator
// asks for. Frequency caps are the Gateway's (unique rows taken before it calls).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

const (
	accountHousePromoPool = "HOUSE_PROMO_POOL"
	txTypePromoCredit     = "PROMO_CREDIT"

	// maxChannelReferenceLen bounds the free-text reference (e.g. "AMOE-<uuid>").
	maxChannelReferenceLen = 128
)

// PromoChannel is the no-purchase route a grant came through. Mirrors the
// promo_grant_channel enum (migration 000010).
type PromoChannel string

const (
	PromoChannelAMOE         PromoChannel = "AMOE"
	PromoChannelBonus        PromoChannel = "BONUS"
	PromoChannelCompensation PromoChannel = "COMPENSATION"
)

// Valid reports whether c is one of the three channels the database accepts.
func (c PromoChannel) Valid() bool {
	switch c {
	case PromoChannelAMOE, PromoChannelBonus, PromoChannelCompensation:
		return true
	}
	return false
}

// PromoGrantRequest is a validated no-purchase grant.
type PromoGrantRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	GCAmount              domain.Money // >= 0
	SCAmount              domain.Money // >= 0, credited as SC_UNPLAYED
	Channel               PromoChannel
	// ChannelReference ties the grant to its source (required for AMOE: an
	// AMOE credit that cannot be traced to its entry is an assertion, not
	// evidence).
	ChannelReference string
	Metadata         json.RawMessage // <= 512 bytes (DB-enforced)
	// BodyHash — see BetRequest.BodyHash.
	BodyHash string
}

const sqlInsertPromoGrant = `
	INSERT INTO promo_grants (
		ledger_transaction_id, operator_code, operator_transaction_id, player_id,
		channel, channel_reference, gc_amount, sc_amount)
	VALUES ($1, $2, $3, $4, $5::promo_grant_channel, $6, $7, $8)
`

// ProcessPromoGrant issues GC and/or SC_UNPLAYED against HOUSE_PROMO_POOL.
// Idempotent + Ghost-Spin safe, exactly like ProcessPurchase.
func (e *engine) ProcessPromoGrant(ctx context.Context, req PromoGrantRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)
	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		return TxResult{}, mapIdempotencyErr(req.OperatorCode, err)
	}
	//nolint:exhaustive // StatusAcquired/StatusUnknown fall through by design — see ProcessPurchase.
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	result, err := e.processPromoGrantTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}
	e.cacheResultQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (e *engine) processPromoGrantTx(ctx context.Context, req PromoGrantRequest) (result TxResult, err error) {
	// §9: player_id is a UUID (not PII); amounts are deliberately omitted.
	ctx, span := telemetry.StartSpan(ctx, "db.promo_grant_tx",
		attribute.String("player_id", req.PlayerID.String()),
		attribute.String("channel", string(req.Channel)))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}
	// A closed, suspended or self-excluded account receives nothing — not even
	// a free entry. Same guard, same lock window as every other money flow.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}
	// Exclusion only — a promo grant is not a loss/deposit, so no loss- or
	// deposit-limit check applies here.
	if err := guardNotExcluded(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}

	alloc, err := wallet.AllocatePromoGrant(req.GCAmount, req.SCAmount)
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
		Type:                  txTypePromoCredit,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, txTypePromoCredit, domain.FamilyUnknown, promoHeadlineAmount(req), req.BodyHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}
	span.SetAttributes(attribute.String("ledger_transaction_id", ledgerTxID.String()))

	if _, err := tx.Exec(ctx, sqlInsertPromoGrant,
		ledgerTxID,
		req.OperatorCode,
		req.OperatorTransactionID,
		req.PlayerID,
		string(req.Channel),
		nullableString(req.ChannelReference),
		req.GCAmount.Decimal(),
		req.SCAmount.Decimal(),
	); err != nil {
		return TxResult{}, fmt.Errorf("insert promo grant: %w", err)
	}

	// Double-entry: each issued currency is a player CREDIT balanced by a
	// HOUSE_PROMO_POOL DEBIT of the same amount.
	for _, c := range alloc.Credits {
		balanceAfter := post.BalanceFor(c.Currency)
		if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, c.Currency, "CREDIT", c.Amount, balanceAfter); err != nil {
			return TxResult{}, err
		}
		if err := insertHouseEntry(ctx, tx, ledgerTxID, accountHousePromoPool, c.Currency, "DEBIT", c.Amount); err != nil {
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
		TransactionType:       txTypePromoCredit,
		Family:                "", // multi-currency (GC and/or SC_UNPLAYED)
		Amount:                promoHeadlineAmount(req),
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// promoHeadlineAmount is the single `amount` the TxResult envelope carries:
// the GC amount when one was granted, otherwise the SC amount. The full split
// is in the ledger entries and the promo_grants row.
func promoHeadlineAmount(req PromoGrantRequest) domain.Money {
	if req.GCAmount.IsPositive() {
		return req.GCAmount
	}
	return req.SCAmount
}

func (r PromoGrantRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if !r.Channel.Valid() {
		return fmt.Errorf("%w: channel must be AMOE, BONUS or COMPENSATION, got %q", errs.ErrInvalidAmount, r.Channel)
	}
	if r.Channel == PromoChannelAMOE && strings.TrimSpace(r.ChannelReference) == "" {
		return fmt.Errorf("%w: channel_reference is required for AMOE", errs.ErrInvalidAmount)
	}
	if len(r.ChannelReference) > maxChannelReferenceLen {
		return fmt.Errorf("%w: channel_reference longer than %d bytes", errs.ErrInvalidAmount, maxChannelReferenceLen)
	}
	if r.GCAmount.IsNegative() || r.SCAmount.IsNegative() {
		return fmt.Errorf("%w: promo grant amounts must be >= 0", errs.ErrInvalidAmount)
	}
	if !r.GCAmount.IsPositive() && !r.SCAmount.IsPositive() {
		return fmt.Errorf("%w: promo grant must issue a positive GC and/or SC_UNPLAYED amount", errs.ErrInvalidAmount)
	}
	return nil
}
