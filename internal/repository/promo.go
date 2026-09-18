package repository

// promo.go — ProcessPromoGrant: issuing coins with no purchase behind them.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS
// ─────────────────────────────────────────────────────────────────────────────
// A US sweepstakes casino is lawful because it is a sweepstakes and not a
// lottery, and the difference is consideration: there must be a genuine way to
// obtain the sweepstakes currency without paying for it. That route — the
// Alternative Method of Entry — is not a marketing feature. It is the load-
// bearing element of the legal model, and every coin issued through it has to be
// as real, as provable, and as spendable as one that was bought.
//
// Before this path existed the engine could only issue SC alongside a fiat
// purchase (ProcessPurchase, against HOUSE_ISSUANCE_POOL). An operator wanting
// to honour a mail-in entry had to either post a fake purchase — putting a
// fiat-shaped DEPOSIT in the ledger for money nobody paid, which is worse than
// no record — or move balance outside the ledger entirely. This path gives the
// free route its own honest accounting.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT MAKES THE "NO PURCHASE NECESSARY" CLAIM PROVABLE
// ─────────────────────────────────────────────────────────────────────────────
// Not a flag, and not a metadata key. Two structural ledger facts:
//
//	1. transaction_type = 'PROMO_CREDIT'. Distinct from DEPOSIT, so the two
//	   issuance routes can never be confused for one another in a query.
//	2. The counterparty is HOUSE_PROMO_POOL, not HOUSE_ISSUANCE_POOL. A promo
//	   grant has NO fiat leg — there is no payment to point at, and the
//	   double-entry shape says so on its own.
//
// "Show me every coin issued without payment" is therefore one WHERE clause over
// an append-only ledger, not an argument about what some JSON field meant.
//
// The promo_grants row written in the same transaction (migration 000014) adds
// the half the ledger cannot carry: WHICH free route — a statutory AMOE entry,
// discretionary marketing, or a goodwill credit. See that migration for why that
// distinction earns a typed column rather than a metadata key.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THIS PATH DELIBERATELY DOES CHECK — CONTRAST WITH refund.go
// ─────────────────────────────────────────────────────────────────────────────
// requirePlayerActive IS enforced here, and the contrast with the refund path is
// the point.
//
// ProcessRedemptionRefund deliberately reaches a blocked player, because
// returning money already taken from someone is not something a self-exclusion
// should be able to prevent. A promo grant is the opposite act: it puts NEW
// coins in front of someone. Handing fresh entries to a player who has excluded
// themselves is textbook inducement — the single thing a self-exclusion is
// for — and doing it through the *free* channel makes it worse, not better,
// because free coins are exactly what re-engagement campaigns are built from.
//
// So: refunds reach blocked players; grants do not. Both rules protect the same
// person.
//
// ─────────────────────────────────────────────────────────────────────────────
// EQUAL DIGNITY, ENFORCED RATHER THAN ASSERTED
// ─────────────────────────────────────────────────────────────────────────────
// SC issued here lands in SC_UNPLAYED and creates an sc_playthrough obligation
// through recordPlaythroughGrant — the SAME function, the same 1x multiplier,
// and the same FIFO discharge as a purchaser's promotional SC. The free entrant
// is not given a worse coin, and not a better one either. That symmetry is what
// keeps AMOE a real alternative rather than a token gesture, and it holds here
// because both paths call one implementation, not because two implementations
// were written to agree.
//
// ─────────────────────────────────────────────────────────────────────────────
// IDEMPOTENCY
// ─────────────────────────────────────────────────────────────────────────────
// The same three layers as every other money path — Redis barrier,
// ledger_transaction_dedup UNIQUE, 23505 Ghost-Spin recovery — and load-bearing
// in the same way a refund's is. A grant is a pure CREDIT bounded by nothing the
// player owns, so a duplicate is not a mis-posting, it is coins invented. The
// promo_grants insert carries its own ON CONFLICT DO NOTHING on the ledger
// transaction id so that a Ghost-Spin re-entry cannot record one mailed entry
// twice.

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

// accountHousePromoPool is the counterparty for every no-purchase issuance.
// Present in the account_type enum since migration 000001; deliberately NOT
// HOUSE_ISSUANCE_POOL, so "issued for payment" and "issued for free" are
// separable by account in every ledger query.
const accountHousePromoPool = "HOUSE_PROMO_POOL"

// txTypePromoCredit is the ledger classification for a no-purchase issuance.
// Present in transaction_type since migration 000001.
const txTypePromoCredit = "PROMO_CREDIT"

// PromoChannel names which no-purchase route a grant came through. It is a
// first-class field rather than a metadata key because the AMOE defense turns on
// exactly this distinction — see migration 000014.
type PromoChannel string

const (
	// PromoChannelAMOE is the statutorily-required free entry method: a mail-in
	// or equivalent entry, honoured with coins. ChannelReference is REQUIRED for
	// this channel (DB-enforced) — an AMOE record that cannot be tied back to a
	// received entry is an assertion, not evidence.
	PromoChannelAMOE PromoChannel = "AMOE"
	// PromoChannelBonus is discretionary promotional credit.
	PromoChannelBonus PromoChannel = "BONUS"
	// PromoChannelCompensation is a goodwill credit for a service failure.
	PromoChannelCompensation PromoChannel = "COMPENSATION"
)

// validPromoChannels mirrors the promo_grant_channel ENUM. Validated in Go as
// well as in the database so a bad channel is a clean 400 rather than a 23514
// surfacing as a 500 — and so the enforcement does not depend on which writer
// reached the table.
var validPromoChannels = map[PromoChannel]struct{}{
	PromoChannelAMOE: {}, PromoChannelBonus: {}, PromoChannelCompensation: {},
}

// maxChannelReferenceLen matches the promo_grants_reference_length CHECK.
const maxChannelReferenceLen = 128

// sqlInsertPromoGrant records which free route issued these coins.
//
// ON CONFLICT DO NOTHING on the ledger transaction id makes it idempotent for
// the same reason sqlInsertPlaythroughGrant is: Ghost-Spin recovery can re-enter
// with a ledger transaction that already has its row, and one mailed AMOE entry
// must never appear as two.
const sqlInsertPromoGrant = `
	INSERT INTO promo_grants (
		player_id, ledger_transaction_id, channel, channel_reference, gc_amount, sc_amount)
	VALUES ($1, $2, $3::promo_grant_channel, $4, $5::numeric, $6::numeric)
	ON CONFLICT (ledger_transaction_id) DO NOTHING`

// PromoGrantRequest issues coins with no purchase behind them.
//
// GCAmount and SCAmount are both optional individually, but at least one must be
// positive. SCAmount is credited to SC_UNPLAYED and carries the standard 1x
// wagering requirement; there is no field for SC_REDEEMABLE and there cannot be
// one — see domain.AllocatePromoGrant.
type PromoGrantRequest struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	GCAmount              domain.Money // >= 0
	SCAmount              domain.Money // >= 0, credited to SC_UNPLAYED
	Channel               PromoChannel
	// ChannelReference is the operator's identifier for the entry this grant
	// answers: the mail-in entry's reference for AMOE, a campaign id otherwise.
	// Required when Channel is AMOE.
	ChannelReference string
	Metadata         json.RawMessage
	// BodyHash — see BetRequest.BodyHash.
	BodyHash string
}

func (r *PromoGrantRequest) normalize() {
	r.Channel = PromoChannel(strings.ToUpper(strings.TrimSpace(string(r.Channel))))
	r.ChannelReference = strings.TrimSpace(r.ChannelReference)
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
	if r.GCAmount.IsNegative() || r.SCAmount.IsNegative() {
		return fmt.Errorf("%w: promo grant amounts must be >= 0", errs.ErrInvalidAmount)
	}
	if !r.GCAmount.IsPositive() && !r.SCAmount.IsPositive() {
		return fmt.Errorf("%w: promo grant must issue a positive GC and/or SC amount", errs.ErrInvalidAmount)
	}
	if _, ok := validPromoChannels[r.Channel]; !ok {
		return fmt.Errorf("%w: invalid promo channel %q", errs.ErrInvalidAmount, r.Channel)
	}
	// An AMOE grant must name the entry it answers. Checked here as well as by
	// the DB CHECK so the caller gets told which field is missing.
	if r.Channel == PromoChannelAMOE && r.ChannelReference == "" {
		return fmt.Errorf("%w: channel_reference is required for an AMOE grant", errs.ErrInvalidAmount)
	}
	if len(r.ChannelReference) > maxChannelReferenceLen {
		return fmt.Errorf("%w: channel_reference exceeds %d bytes",
			errs.ErrInvalidAmount, maxChannelReferenceLen)
	}
	return nil
}

// primaryAmount is the single figure the receipt's Amount field carries.
//
// A grant can issue two currencies, and TxResult has one Amount — the same
// situation ProcessPurchase is in, which reports its GC leg and sets Family to
// "" to mean "multi-currency, read PostBalances for the breakdown".
//
// This path reports the SC leg first because SC is what the sweepstakes claim is
// about: an AMOE entry's whole purpose is the Sweeps Coins it yields, and a
// receipt that led with the GC would be answering a less important question.
// GC is reported only for a grant that issues no SC, so the figure is never
// zero — validate() guarantees one of the two is positive. The authoritative
// per-currency breakdown is in the ledger entries and the promo_grants row;
// this is a convenience field, not the record.
func (r PromoGrantRequest) primaryAmount() domain.Money {
	if r.SCAmount.IsPositive() {
		return r.SCAmount
	}
	return r.GCAmount
}

// ProcessPromoGrant credits GC and/or SC_UNPLAYED against HOUSE_PROMO_POOL and
// records the grant as a PROMO_CREDIT — the ledger shape that carries the "no
// purchase necessary" claim.
func (e *engine) ProcessPromoGrant(ctx context.Context, req PromoGrantRequest) (TxResult, error) {
	req.normalize()
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)
	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		// FAIL CLOSED — never issue coins when the idempotency barrier is down.
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

	result, err := e.processPromoGrantTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}
	e.cacheResultQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (e *engine) processPromoGrantTx(ctx context.Context, req PromoGrantRequest) (result TxResult, err error) {
	// §9: player_id is a UUID (not PII). The channel is recorded on the span
	// because "how many AMOE grants are we issuing" is an operational question,
	// and the amounts are deliberately omitted as on every other money span.
	ctx, span := telemetry.StartSpan(ctx, "db.promo_grant_tx",
		attribute.String("player_id", req.PlayerID.String()),
		attribute.String("promo_channel", string(req.Channel)))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The same wallet lock every money path takes, so a grant serializes with a
	// concurrent wager rather than racing it.
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// Enforced here and NOT in refund.go — see the file header. A grant offers
	// new coins, so a self-excluded or suspended player must not receive one.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}

	alloc, err := wallet.AllocatePromoGrant(req.GCAmount, req.SCAmount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyPromoGrant(alloc)

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
			// Ghost recovery: a grant that already committed returns ITS receipt
			// rather than issuing a second set of coins.
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, txTypePromoCredit, domain.FamilyUnknown, req.primaryAmount(), req.BodyHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}
	span.SetAttributes(attribute.String("ledger_transaction_id", ledgerTxID.String()))

	// Which free route this was, written in the SAME transaction as the ledger
	// row so a grant can never exist without its origin recorded.
	if err := recordPromoGrant(ctx, tx, req, ledgerTxID); err != nil {
		return TxResult{}, err
	}

	// The SAME 1x obligation a purchaser's promotional SC carries, created by
	// the SAME function. Equal dignity is enforced by shared code here, not by
	// two implementations that were written to agree. A zero SC grant writes no
	// row.
	if err := recordPlaythroughGrant(ctx, tx, req.PlayerID, ledgerTxID, req.SCAmount); err != nil {
		return TxResult{}, err
	}

	// Double-entry: each issued currency is a player CREDIT balanced by a
	// HOUSE_PROMO_POOL DEBIT of the same amount. Identical machinery to the
	// purchase path; only the counterparty account differs, and that difference
	// is what carries the no-purchase claim.
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
		Amount:                req.primaryAmount(),
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// recordPromoGrant writes the promo_grants row for a committed grant.
func recordPromoGrant(ctx context.Context, tx pgx.Tx, req PromoGrantRequest, ledgerTxID uuid.UUID) error {
	if _, err := tx.Exec(ctx, sqlInsertPromoGrant,
		req.PlayerID,
		ledgerTxID,
		string(req.Channel),
		nullableString(req.ChannelReference),
		req.GCAmount.Decimal(),
		req.SCAmount.Decimal(),
	); err != nil {
		return fmt.Errorf("record promo grant: %w", err)
	}
	return nil
}
