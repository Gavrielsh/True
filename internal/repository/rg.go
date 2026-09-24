package repository

// rg.go implements PLAN.md item 6 (responsible gaming), engine side: the
// engine refuses money-moving calls for a player who is self-excluded, in
// cool-off, or over a loss / deposit limit.
//
// Two kinds of state, matching migration 000011:
//
//   - player_limits (APPEND-ONLY): every limit ever set. "Current state" is
//     the latest row per (player_id, limit_type) with effective_at <= now()
//     — see latestEffectiveLimit.
//   - player_rg_counters (MUTABLE): a cache of the player's accumulated
//     loss/deposit activity for the CURRENT window. Never trusted blind: a
//     missing or stale row is re-seeded from the ledger itself before use
//     (getOrSeedCounter), never from zero — a player who already lost X this
//     window and only THEN sets a limit must see X counted immediately.
//
// Loss is defined as game activity only: stakes from BET/SPIN minus wins
// from WIN/SPIN; a ROLLBACK reverses its own contribution. Purchases, promo
// grants, redemptions and admin adjustments never touch it. Because a
// server-authoritative spin ALSO writes typed BET/WIN ledger_transactions
// rows (sqlSettleSpin), filtering ledger_transactions.transaction_type IN
// ('BET', 'WIN', 'ROLLBACK') captures both /bet+/win and /spin activity with
// one definition — see sqlRGLossAggregate.
//
// Every guard and every counter update in this file runs on the SAME tx
// handle the caller already holds the player's wallets row lock on (BET,
// SPIN, PURCHASE, PROMO_GRANT all call selectWalletForUpdate/
// lockWalletAndStatus first). That lock is what serializes concurrent calls
// for one player — a second guard function never needs its own lock on
// player_limits or player_rg_counters, since nothing else can be running for
// this player at the same time.
//
// The one exception is CheckPurchase and QueryLimits, which are read-only,
// pre-charge / informational endpoints with no wallet lock: they run in a
// short read-only transaction of their own for a consistent snapshot, not to
// serialize against anything.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// limitIncreaseDelay is how long an increase to a loss/deposit limit, or a
// removal of one, takes to become effective — point in the plan: "decreases
// and new exclusions are immediate; increases and removals wait 24h". A
// self-exclusion/cool-off is always immediate instead (see SetLimit); it can
// only ever get MORE restrictive without a delay, and it can never be lifted
// before its own ends_at at all, delay or not.
const limitIncreaseDelay = 24 * time.Hour

// ----------------------------------------------------------------------------
// ResponsibleGamingEngine
// ----------------------------------------------------------------------------

// ResponsibleGamingEngine is the Gateway-facing surface for setting and
// reading player limits. The refusal guards themselves (guardNotExcluded,
// guardLossLimit, guardDepositLimit) are unexported: they are called
// directly, on the caller's own tx handle, from ProcessBet/ProcessSpin/
// ProcessPurchase/ProcessPromoGrant — not through this interface.
type ResponsibleGamingEngine interface {
	// SetLimit records a new limit row (append-only) and reports when it
	// takes effect. A duplicate EventID replays the original result.
	SetLimit(ctx context.Context, req SetLimitRequest) (LimitResult, error)

	// QueryLimits reports the player's current effective limits and, for
	// LOSS_LIMIT/DEPOSIT_LIMIT, the remaining allowance in the current
	// window.
	QueryLimits(ctx context.Context, playerID uuid.UUID) (PlayerLimitsSummary, error)

	// CheckPurchase is the PRE-CHARGE check: the gateway must call this
	// BEFORE charging the card, and refund if /store/purchase's own
	// backstop guard ever refuses instead (which should only happen on a
	// genuine race, or a gateway that skipped this call).
	CheckPurchase(ctx context.Context, req CheckPurchaseRequest) (CheckPurchaseResult, error)
}

// NewResponsibleGaming constructs a ResponsibleGamingEngine backed by the
// same underlying *engine type used by Engine/CasinoEngine/KYCEngine, so
// callers share one pool/idem/logger triple across all four interfaces.
// idem is accepted for that consistency even though this file does not use
// the Redis idempotency barrier (event_id + the append-only table's UNIQUE
// constraint is the idempotency anchor, exactly like kyc_decisions).
func NewResponsibleGaming(db DB, idem cache.Store, logger *slog.Logger) ResponsibleGamingEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &engine{db: db, idem: idem, logger: logger}
}

// ----------------------------------------------------------------------------
// Request / result types
// ----------------------------------------------------------------------------

// SetLimitRequest sets or lifts one limit for one player. It is always a NEW
// row (player_limits is append-only) — a "change" is Active=true with a new
// Amount/EndsAt, and a "lift"/"removal" is Active=false.
type SetLimitRequest struct {
	PlayerID  uuid.UUID
	LimitType string // SELF_EXCLUSION | COOL_OFF | LOSS_LIMIT | DEPOSIT_LIMIT
	Active    bool
	// Period is required when Active and LimitType is LOSS_LIMIT/DEPOSIT_LIMIT.
	Period string // DAY | WEEK | MONTH
	// Amount is required when Active and LimitType is LOSS_LIMIT/DEPOSIT_LIMIT.
	Amount *domain.Money
	// EndsAt is required when Active and LimitType is SELF_EXCLUSION/COOL_OFF.
	EndsAt *time.Time
	// EventID is the Gateway's idempotency key for this set/lift request.
	EventID string
	// RequestedBy identifies who asked for this (player self-service vs. an
	// operator acting on the player's behalf); free text, optional.
	RequestedBy string
}

// LimitResult reports how a SetLimit request was resolved.
type LimitResult struct {
	PlayerID    uuid.UUID     `json:"player_id"`
	LimitType   string        `json:"limit_type"`
	Active      bool          `json:"active"`
	Period      string        `json:"period,omitempty"`
	Amount      *domain.Money `json:"amount,omitempty"`
	EndsAt      *time.Time    `json:"ends_at,omitempty"`
	EffectiveAt time.Time     `json:"effective_at"`
	EventID     string        `json:"event_id"`
}

// CheckPurchaseRequest is the pre-charge deposit/exclusion check.
type CheckPurchaseRequest struct {
	PlayerID  uuid.UUID
	USDAmount domain.Money // required, > 0
}

// CheckPurchaseResult reports whether a purchase of the given USD amount is
// currently allowed. When it is not, CheckPurchase returns an error instead
// (an *errs.RGRestrictionError) rather than Allowed=false — the same
// RG_RESTRICTED body every other guard produces.
type CheckPurchaseResult struct {
	PlayerID uuid.UUID `json:"player_id"`
	Allowed  bool      `json:"allowed"`
}

// PlayerLimitsSummary is the current effective state of every limit type for
// a player, for the Gateway to display back to them.
type PlayerLimitsSummary struct {
	PlayerID      uuid.UUID           `json:"player_id"`
	SelfExclusion *ExclusionState     `json:"self_exclusion,omitempty"`
	CoolOff       *ExclusionState     `json:"cool_off,omitempty"`
	LossLimit     *WindowedLimitState `json:"loss_limit,omitempty"`
	DepositLimit  *WindowedLimitState `json:"deposit_limit,omitempty"`
}

// ExclusionState is present iff the exclusion/cool-off is currently active.
type ExclusionState struct {
	EndsAt time.Time `json:"ends_at"`
}

// WindowedLimitState is present iff a LOSS_LIMIT/DEPOSIT_LIMIT is currently
// active. Remaining is Amount - Used, floored at zero.
type WindowedLimitState struct {
	Period    string       `json:"period"`
	Amount    domain.Money `json:"amount"`
	Used      domain.Money `json:"used"`
	Remaining domain.Money `json:"remaining"`
}

// ----------------------------------------------------------------------------
// SQL
// ----------------------------------------------------------------------------

const (
	// sqlRGLockPlayer serializes concurrent SetLimit calls for the same
	// player, exactly like sqlKYCLockUser — the effective_at/delay decision
	// below reads the player's current limit state and must not race another
	// SetLimit doing the same.
	sqlRGLockPlayer = `SELECT id FROM users WHERE id = $1 FOR UPDATE`

	// sqlRGInsertLimit is the append-only audit write. event_id is UNIQUE
	// (migration 000011): a duplicate delivery raises 23505, caught by the
	// caller and resolved via recoverRGLimitReplay.
	sqlRGInsertLimit = `
		INSERT INTO player_limits
			(player_id, limit_type, period, active, amount, ends_at, effective_at, event_id, requested_by)
		VALUES ($1, $2::rg_limit_type, $3::rg_period, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`

	// sqlRGSelectByEventID re-reads the audit row for a replayed delivery.
	sqlRGSelectByEventID = `
		SELECT player_id, limit_type, period, active, amount, ends_at, effective_at, event_id
		FROM player_limits
		WHERE event_id = $1
	`

	// sqlRGLatestEffectiveLimit reads the single row that currently governs
	// a (player_id, limit_type): the most recent row whose effective_at has
	// already passed. Used both for the exclusion "cannot lift before
	// ends_at" check and for reading the currently active LOSS_LIMIT/
	// DEPOSIT_LIMIT amount.
	sqlRGLatestEffectiveLimit = `
		SELECT active, period, amount, ends_at
		FROM player_limits
		WHERE player_id = $1 AND limit_type = $2::rg_limit_type AND effective_at <= now()
		ORDER BY created_at DESC
		LIMIT 1
	`

	// sqlRGActiveExclusion is the hot-path guard: the latest row per
	// exclusion type, filtered to ones that are active AND not yet expired.
	// A dated exclusion/cool-off needs no explicit "lift" row to stop
	// restricting once ends_at passes — the WHERE clause outside the CTE
	// does that automatically. Single round trip covers both
	// SELF_EXCLUSION and COOL_OFF.
	sqlRGActiveExclusion = `
		WITH latest AS (
			SELECT DISTINCT ON (limit_type) limit_type, ends_at
			FROM player_limits
			WHERE player_id = $1 AND limit_type IN ('SELF_EXCLUSION', 'COOL_OFF')
			ORDER BY limit_type, created_at DESC
		)
		SELECT limit_type, ends_at FROM latest WHERE ends_at > now()
	`

	// sqlRGSelectCounter reads the cached running total for one window.
	sqlRGSelectCounter = `
		SELECT window_start, amount
		FROM player_rg_counters
		WHERE player_id = $1 AND counter_type = $2 AND period = $3::rg_period
	`

	// sqlRGUpsertCounter writes the authoritative post-settlement total.
	// ON CONFLICT because the row may or may not already exist for this
	// window (first activity of the window inserts it).
	sqlRGUpsertCounter = `
		INSERT INTO player_rg_counters (player_id, counter_type, period, window_start, amount, updated_at)
		VALUES ($1, $2, $3::rg_period, $4, $5, now())
		ON CONFLICT (player_id, counter_type, period)
		DO UPDATE SET window_start = EXCLUDED.window_start, amount = EXCLUDED.amount, updated_at = now()
	`

	// sqlRGLossAggregate re-derives the LOSS counter for a window straight
	// from the ledger: stakes from BET (spins write a BET row too — see
	// file header) minus wins from WIN, minus a rolled-back BET's own stake
	// (a ROLLBACK entry is a CREDIT back to the player, so summing it in
	// with the same sign as WIN reverses the original BET correctly).
	// ledger_transactions/ledger_entries have no FK (dropped for daily
	// partitioning — see queries.go); the join is by id AND created_at,
	// which are guaranteed equal for rows written in the same transaction
	// (transaction_timestamp() is constant within a transaction) and keeps
	// the join partition-pruned. Driven by ledger_tx_player_id_idx
	// (player_id, created_at DESC); a single indexed query, as required.
	sqlRGLossAggregate = `
		SELECT COALESCE(SUM(
			CASE WHEN t.transaction_type = 'BET' THEN e.amount ELSE -e.amount END
		), 0)
		FROM ledger_transactions t
		JOIN ledger_entries e
			ON e.ledger_transaction_id = t.id AND e.created_at = t.created_at
		WHERE t.player_id = $1
		  AND t.transaction_type IN ('BET', 'WIN', 'ROLLBACK')
		  AND t.created_at >= $2
		  AND t.created_at < $3
		  AND e.account_type = 'PLAYER_WALLET'
	`

	// sqlRGDepositAggregate re-derives the DEPOSIT counter for a window
	// straight from the ledger. usd_amount lives directly on
	// ledger_transactions (migration 000011), so no join is needed.
	sqlRGDepositAggregate = `
		SELECT COALESCE(SUM(t.usd_amount), 0)
		FROM ledger_transactions t
		WHERE t.player_id = $1
		  AND t.transaction_type = 'DEPOSIT'
		  AND t.usd_amount IS NOT NULL
		  AND t.created_at >= $2
		  AND t.created_at < $3
	`
)

// ----------------------------------------------------------------------------
// SetLimit
// ----------------------------------------------------------------------------

func (e *engine) SetLimit(ctx context.Context, req SetLimitRequest) (result LimitResult, err error) {
	if err := req.validate(); err != nil {
		return LimitResult{}, err
	}

	ctx, span := telemetry.StartSpan(ctx, "db.rg_set_limit_tx",
		attribute.String("player_id", req.PlayerID.String()),
		attribute.String("limit_type", req.LimitType))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return LimitResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var playerID uuid.UUID
	err = tx.QueryRow(ctx, sqlRGLockPlayer, req.PlayerID).Scan(&playerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LimitResult{}, errs.ErrPlayerNotFound
	}
	if err != nil {
		return LimitResult{}, fmt.Errorf("lock player: %w", err)
	}

	now := time.Now().UTC()
	var effectiveAt time.Time

	switch req.LimitType {
	case "SELF_EXCLUSION", "COOL_OFF":
		if !req.Active {
			// Point 6: cannot be lifted before the current restriction's own
			// ends_at, for either type.
			current, ok, lerr := latestEffectiveLimit(ctx, tx, req.PlayerID, req.LimitType)
			if lerr != nil {
				return LimitResult{}, lerr
			}
			if ok && current.Active && current.EndsAt != nil && current.EndsAt.After(now) {
				return LimitResult{}, fmt.Errorf("%w: %s cannot be lifted before %s",
					errs.ErrInvalidAmount, req.LimitType, current.EndsAt.Format(time.RFC3339))
			}
		}
		// Setting or extending an exclusion/cool-off is always MORE
		// protective, so it is always immediate. Lifting one is only ever
		// possible once its own ends_at has already passed (checked above),
		// so an extra delay on top would be meaningless — also immediate.
		effectiveAt = now
	case "LOSS_LIMIT", "DEPOSIT_LIMIT":
		current, ok, lerr := latestEffectiveLimit(ctx, tx, req.PlayerID, req.LimitType)
		if lerr != nil {
			return LimitResult{}, lerr
		}
		var currentAmount *decimal.Decimal
		if ok && current.Active && current.Amount != nil {
			currentAmount = current.Amount
		}
		var newAmount *decimal.Decimal
		if req.Active {
			d := req.Amount.Decimal()
			newAmount = &d
		}
		if isDecreaseOrNew(newAmount, currentAmount) {
			effectiveAt = now
		} else {
			effectiveAt = now.Add(limitIncreaseDelay)
		}
	}

	var (
		period *string
		amount *decimal.Decimal
		endsAt *time.Time
	)
	if req.Active {
		switch req.LimitType {
		case "LOSS_LIMIT", "DEPOSIT_LIMIT":
			p := req.Period
			period = &p
			a := req.Amount.Decimal()
			amount = &a
		case "SELF_EXCLUSION", "COOL_OFF":
			endsAt = req.EndsAt
		}
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, sqlRGInsertLimit,
		req.PlayerID, req.LimitType, period, req.Active, amount, endsAt, effectiveAt,
		req.EventID, nullableString(req.RequestedBy),
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			// Stale FOR UPDATE lock released by the deferred Rollback.
			_ = tx.Rollback(ctx)
			return e.recoverRGLimitReplay(ctx, req.EventID)
		}
		return LimitResult{}, fmt.Errorf("insert player limit: %w", err)
	}
	span.SetAttributes(attribute.String("player_limit_id", id.String()))

	if err := tx.Commit(ctx); err != nil {
		return LimitResult{}, fmt.Errorf("commit: %w", err)
	}

	return LimitResult{
		PlayerID:    req.PlayerID,
		LimitType:   req.LimitType,
		Active:      req.Active,
		Period:      req.Period,
		Amount:      req.Amount,
		EndsAt:      endsAt,
		EffectiveAt: effectiveAt,
		EventID:     req.EventID,
	}, nil
}

// recoverRGLimitReplay handles a duplicate event_id delivery: the audit row
// already exists from a prior successful SetLimit, so this is a replay, not
// a new request. Reconstructed from the append-only row itself, exactly
// like recoverKYCReplay.
func (e *engine) recoverRGLimitReplay(ctx context.Context, eventID string) (LimitResult, error) {
	var (
		playerID  uuid.UUID
		limitType string
		period    pgtype.Text
		active    bool
		amount    *decimal.Decimal
		endsAt    pgtype.Timestamptz
		effAt     time.Time
	)
	err := e.db.QueryRow(ctx, sqlRGSelectByEventID, eventID).
		Scan(&playerID, &limitType, &period, &active, &amount, &endsAt, &effAt, &eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LimitResult{}, fmt.Errorf("%w: event %s vanished post-conflict",
			errs.ErrTransactionConflict, eventID)
	}
	if err != nil {
		return LimitResult{}, fmt.Errorf("rg limit replay lookup: %w", err)
	}

	result := LimitResult{
		PlayerID:    playerID,
		LimitType:   limitType,
		Active:      active,
		EffectiveAt: effAt,
		EventID:     eventID,
	}
	if period.Valid {
		result.Period = period.String
	}
	if amount != nil {
		m, merr := domain.NewMoney(*amount)
		if merr != nil {
			return LimitResult{}, fmt.Errorf("rg limit replay: %w", merr)
		}
		result.Amount = &m
	}
	if endsAt.Valid {
		t := endsAt.Time
		result.EndsAt = &t
	}
	return result, nil
}

// isDecreaseOrNew reports whether moving from currentAmount to newAmount is
// a decrease (more restrictive) or a brand-new limit where none existed
// before — both take effect immediately. nil means "no limit" (+infinity):
// nil currentAmount makes any finite newAmount a decrease (tightening from
// unlimited); nil newAmount (a removal) is always an increase-or-removal, so
// it is never a "decrease" even when currentAmount is also nil.
func isDecreaseOrNew(newAmount, currentAmount *decimal.Decimal) bool {
	if newAmount == nil {
		return false
	}
	if currentAmount == nil {
		return true
	}
	return newAmount.LessThanOrEqual(*currentAmount)
}

// ----------------------------------------------------------------------------
// QueryLimits
// ----------------------------------------------------------------------------

func (e *engine) QueryLimits(ctx context.Context, playerID uuid.UUID) (summary PlayerLimitsSummary, err error) {
	if playerID == uuid.Nil {
		return PlayerLimitsSummary{}, fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}

	ctx, span := telemetry.StartSpan(ctx, "db.rg_query_limits",
		attribute.String("player_id", playerID.String()))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PlayerLimitsSummary{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	summary.PlayerID = playerID
	now := time.Now()

	for _, lt := range []string{"SELF_EXCLUSION", "COOL_OFF"} {
		l, ok, lerr := latestEffectiveLimit(ctx, tx, playerID, lt)
		if lerr != nil {
			return PlayerLimitsSummary{}, lerr
		}
		if !ok || !l.Active || l.EndsAt == nil || !l.EndsAt.After(now) {
			continue
		}
		state := &ExclusionState{EndsAt: *l.EndsAt}
		if lt == "SELF_EXCLUSION" {
			summary.SelfExclusion = state
		} else {
			summary.CoolOff = state
		}
	}

	for _, spec := range []struct {
		limitType, counterType string
	}{
		{"LOSS_LIMIT", "LOSS"},
		{"DEPOSIT_LIMIT", "DEPOSIT"},
	} {
		l, ok, lerr := latestEffectiveLimit(ctx, tx, playerID, spec.limitType)
		if lerr != nil {
			return PlayerLimitsSummary{}, lerr
		}
		if !ok || !l.Active || l.Period == nil || l.Amount == nil {
			continue
		}
		used, uerr := getOrSeedCounter(ctx, tx, playerID, spec.counterType, *l.Period)
		if uerr != nil {
			return PlayerLimitsSummary{}, uerr
		}
		limitAmount, merr := domain.NewMoney(*l.Amount)
		if merr != nil {
			return PlayerLimitsSummary{}, fmt.Errorf("query limits: %w", merr)
		}
		usedMoney, merr := domain.NewMoney(used)
		if merr != nil {
			return PlayerLimitsSummary{}, fmt.Errorf("query limits: %w", merr)
		}
		remaining := limitAmount.Sub(usedMoney)
		if remaining.IsNegative() {
			remaining = domain.ZeroMoney()
		}
		state := &WindowedLimitState{
			Period:    *l.Period,
			Amount:    limitAmount,
			Used:      usedMoney,
			Remaining: remaining,
		}
		if spec.limitType == "LOSS_LIMIT" {
			summary.LossLimit = state
		} else {
			summary.DepositLimit = state
		}
	}

	return summary, nil
}

// ----------------------------------------------------------------------------
// CheckPurchase
// ----------------------------------------------------------------------------

func (e *engine) CheckPurchase(ctx context.Context, req CheckPurchaseRequest) (result CheckPurchaseResult, err error) {
	if req.PlayerID == uuid.Nil {
		return CheckPurchaseResult{}, fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if !req.USDAmount.IsPositive() {
		return CheckPurchaseResult{}, fmt.Errorf("%w: usd_amount must be > 0", errs.ErrInvalidAmount)
	}

	ctx, span := telemetry.StartSpan(ctx, "db.rg_check_purchase",
		attribute.String("player_id", req.PlayerID.String()))
	defer func() { telemetry.EndSpan(span, err) }()

	// Read-only, no wallet lock: this is the PRE-CHARGE check (point 1), run
	// before the card is charged. /store/purchase's own guard, inside the
	// money-moving transaction, is the backstop of record.
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CheckPurchaseResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := guardNotExcluded(ctx, tx, req.PlayerID); err != nil {
		return CheckPurchaseResult{}, err
	}
	amount := req.USDAmount
	if err := guardDepositLimit(ctx, tx, req.PlayerID, &amount); err != nil {
		return CheckPurchaseResult{}, err
	}

	return CheckPurchaseResult{PlayerID: req.PlayerID, Allowed: true}, nil
}

// ----------------------------------------------------------------------------
// Guards — called from ProcessBet/ProcessSpin/ProcessPurchase/
// ProcessPromoGrant on their own already-locked tx handle.
// ----------------------------------------------------------------------------

// guardNotExcluded refuses when the player has an active, unexpired
// SELF_EXCLUSION or COOL_OFF. Checked before any stake/purchase is drawn.
func guardNotExcluded(ctx context.Context, tx pgx.Tx, playerID uuid.UUID) error {
	rows, err := tx.Query(ctx, sqlRGActiveExclusion, playerID)
	if err != nil {
		return fmt.Errorf("select exclusion state: %w", err)
	}
	defer rows.Close()

	// Any active row restricts; the first one is enough to name the reason.
	if rows.Next() {
		var (
			limitType string
			endsAt    time.Time
		)
		if err := rows.Scan(&limitType, &endsAt); err != nil {
			return fmt.Errorf("scan exclusion state: %w", err)
		}
		return &errs.RGRestrictionError{Reason: limitType, Until: &endsAt}
	}
	return rows.Err()
}

// guardLossLimit refuses when accumulated + stake > the active LOSS_LIMIT.
// Checked using ONLY the caller's stake, before any outcome is drawn — the
// refusal never depends on the RNG result.
func guardLossLimit(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, stake domain.Money) error {
	l, ok, err := latestEffectiveLimit(ctx, tx, playerID, "LOSS_LIMIT")
	if err != nil {
		return err
	}
	if !ok || !l.Active || l.Period == nil || l.Amount == nil {
		return nil
	}
	accumulated, err := getOrSeedCounter(ctx, tx, playerID, "LOSS", *l.Period)
	if err != nil {
		return err
	}
	if accumulated.Add(stake.Decimal()).GreaterThan(*l.Amount) {
		return &errs.RGRestrictionError{Reason: "LOSS_LIMIT"}
	}
	return nil
}

// guardDepositLimit refuses when accumulated + usdAmount > the active
// DEPOSIT_LIMIT. usdAmount is nil until the gateway sends it on every
// purchase (PurchaseRequest.USDAmount is optional for now) — a nil amount
// while a DEPOSIT_LIMIT is active fails CLOSED rather than under-counting.
func guardDepositLimit(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, usdAmount *domain.Money) error {
	l, ok, err := latestEffectiveLimit(ctx, tx, playerID, "DEPOSIT_LIMIT")
	if err != nil {
		return err
	}
	if !ok || !l.Active || l.Period == nil || l.Amount == nil {
		return nil
	}
	if usdAmount == nil {
		return &errs.RGRestrictionError{Reason: "DEPOSIT_LIMIT"}
	}
	accumulated, err := getOrSeedCounter(ctx, tx, playerID, "DEPOSIT", *l.Period)
	if err != nil {
		return err
	}
	if accumulated.Add(usdAmount.Decimal()).GreaterThan(*l.Amount) {
		return &errs.RGRestrictionError{Reason: "DEPOSIT_LIMIT"}
	}
	return nil
}

// ----------------------------------------------------------------------------
// Counter maintenance — called AFTER settlement, only ever bumping a window
// that has an active limit (a player with no limit set carries no counter
// row at all, until they set one and it seeds itself from the ledger).
// ----------------------------------------------------------------------------

// adjustLossCounters bumps the LOSS counter for the player's active
// LOSS_LIMIT window (if any) by netDelta — stake minus win for a settled
// round, -stake for a rollback, or the raw stake for a plain BET that has
// not settled yet (offset later by its WIN or ROLLBACK).
func adjustLossCounters(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, netDelta domain.Money) error {
	l, ok, err := latestEffectiveLimit(ctx, tx, playerID, "LOSS_LIMIT")
	if err != nil {
		return err
	}
	if !ok || !l.Active || l.Period == nil {
		return nil
	}
	return bumpCounter(ctx, tx, playerID, "LOSS", *l.Period, netDelta.Decimal())
}

// adjustDepositCounter bumps the DEPOSIT counter for the player's active
// DEPOSIT_LIMIT window (if any) by usdAmount. No-op when usdAmount is nil
// (the gateway didn't supply it) — guardDepositLimit already fails closed on
// the check side, so this simply skips the bookkeeping it couldn't do.
func adjustDepositCounter(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, usdAmount *domain.Money) error {
	if usdAmount == nil {
		return nil
	}
	l, ok, err := latestEffectiveLimit(ctx, tx, playerID, "DEPOSIT_LIMIT")
	if err != nil {
		return err
	}
	if !ok || !l.Active || l.Period == nil {
		return nil
	}
	return bumpCounter(ctx, tx, playerID, "DEPOSIT", *l.Period, usdAmount.Decimal())
}

// bumpCounter writes the new authoritative total for this window, floored at
// zero. Every caller runs AFTER its ledger rows are inserted in the same tx,
// so when the counter row is missing or stale the ledger re-derivation
// already includes this transaction — adding delta on top of it would count
// the transaction twice. delta is applied only to a fresh counter row.
func bumpCounter(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, counterType, period string, delta decimal.Decimal) error {
	current, fresh, err := readCounter(ctx, tx, playerID, counterType, period)
	if err != nil {
		return err
	}
	updated := current
	if fresh {
		updated = current.Add(delta)
	}
	if updated.IsNegative() {
		// A win/rollback can exceed the still-outstanding stake portion of
		// the window (e.g. a big win shortly after the window rolled over)
		// — the counter floors at zero rather than going negative.
		updated = decimal.Zero
	}
	start, _ := windowBounds(period, time.Now())
	if _, err := tx.Exec(ctx, sqlRGUpsertCounter, playerID, counterType, period, start, updated); err != nil {
		return fmt.Errorf("upsert rg counter: %w", err)
	}
	return nil
}

// getOrSeedCounter returns the accumulated total for the player's CURRENT
// window. A missing row, or one whose window_start does not match the
// current window's start, is re-seeded from the ledger (point 4) rather
// than returning zero — a player who already lost/deposited X this window
// and only then sets a limit must see X counted immediately. This function
// never writes; only bumpCounter persists a value.
func getOrSeedCounter(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, counterType, period string) (decimal.Decimal, error) {
	total, _, err := readCounter(ctx, tx, playerID, counterType, period)
	return total, err
}

// readCounter is getOrSeedCounter that also reports whether the value came
// from a counter row for the current window (fresh) or was re-derived from
// the ledger.
func readCounter(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, counterType, period string) (decimal.Decimal, bool, error) {
	start, end := windowBounds(period, time.Now())

	var (
		rowStart pgtype.Timestamptz
		amount   decimal.Decimal
	)
	err := tx.QueryRow(ctx, sqlRGSelectCounter, playerID, counterType, period).Scan(&rowStart, &amount)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Decimal{}, false, fmt.Errorf("select rg counter: %w", err)
	}
	if err == nil && rowStart.Valid && rowStart.Time.Equal(start) {
		return amount, true, nil
	}
	total, err := ledgerAggregate(ctx, tx, playerID, counterType, start, end)
	return total, false, err
}

// ledgerAggregate re-derives a counter total directly from
// ledger_transactions/ledger_entries for [start, end).
func ledgerAggregate(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, counterType string, start, end time.Time) (decimal.Decimal, error) {
	var query string
	switch counterType {
	case "LOSS":
		query = sqlRGLossAggregate
	case "DEPOSIT":
		query = sqlRGDepositAggregate
	default:
		return decimal.Decimal{}, fmt.Errorf("rg: unknown counter type %q", counterType)
	}
	var total decimal.Decimal
	if err := tx.QueryRow(ctx, query, playerID, start, end).Scan(&total); err != nil {
		return decimal.Decimal{}, fmt.Errorf("aggregate %s from ledger: %w", counterType, err)
	}
	if total.IsNegative() {
		// A player can only ever be net up on a rollback-heavy window from
		// the counter's point of view once wins are netted against stakes;
		// the counter itself is a "how much have you lost" figure and is
		// never negative.
		total = decimal.Zero
	}
	return total, nil
}

// ----------------------------------------------------------------------------
// latestEffectiveLimit — shared read for both guards and SetLimit.
// ----------------------------------------------------------------------------

type latestLimit struct {
	Active bool
	Period *string
	Amount *decimal.Decimal
	EndsAt *time.Time
}

// latestEffectiveLimit reads the most recent player_limits row for
// (playerID, limitType) whose effective_at has already passed — i.e. the
// row that currently governs. ok is false when no such row exists yet.
func latestEffectiveLimit(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, limitType string) (latestLimit, bool, error) {
	var (
		active bool
		period pgtype.Text
		amount *decimal.Decimal
		endsAt pgtype.Timestamptz
	)
	err := tx.QueryRow(ctx, sqlRGLatestEffectiveLimit, playerID, limitType).Scan(&active, &period, &amount, &endsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return latestLimit{}, false, nil
	}
	if err != nil {
		return latestLimit{}, false, fmt.Errorf("select latest limit: %w", err)
	}
	l := latestLimit{Active: active, Amount: amount}
	if period.Valid {
		p := period.String
		l.Period = &p
	}
	if endsAt.Valid {
		t := endsAt.Time
		l.EndsAt = &t
	}
	return l, true, nil
}

// ----------------------------------------------------------------------------
// windowBounds
// ----------------------------------------------------------------------------

// windowBounds returns the [start, end) UTC bounds of the DAY/WEEK/MONTH
// window that `now` falls in. WEEK starts Monday 00:00 UTC.
func windowBounds(period string, now time.Time) (start, end time.Time) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	switch period {
	case "WEEK":
		offset := int(now.Weekday()) - int(time.Monday)
		if offset < 0 {
			offset += 7
		}
		start = day.AddDate(0, 0, -offset)
		end = start.AddDate(0, 0, 7)
	case "MONTH":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	default: // "DAY"
		start = day
		end = start.AddDate(0, 0, 1)
	}
	return start, end
}

// ----------------------------------------------------------------------------
// Validation
// ----------------------------------------------------------------------------

func (r SetLimitRequest) validate() error {
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if r.EventID == "" {
		return fmt.Errorf("%w: empty event_id", errs.ErrInvalidAmount)
	}
	switch r.LimitType {
	case "LOSS_LIMIT", "DEPOSIT_LIMIT":
		if r.Active {
			if r.Period != "DAY" && r.Period != "WEEK" && r.Period != "MONTH" {
				return fmt.Errorf("%w: period must be DAY, WEEK or MONTH", errs.ErrInvalidAmount)
			}
			if r.Amount == nil || !r.Amount.IsPositive() {
				return fmt.Errorf("%w: amount must be > 0", errs.ErrInvalidAmount)
			}
		}
	case "SELF_EXCLUSION", "COOL_OFF":
		if r.Active {
			if r.EndsAt == nil || !r.EndsAt.After(time.Now()) {
				return fmt.Errorf("%w: ends_at must be in the future", errs.ErrInvalidAmount)
			}
		}
	default:
		return fmt.Errorf("%w: unknown limit_type %q", errs.ErrInvalidAmount, r.LimitType)
	}
	return nil
}
