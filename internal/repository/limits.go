package repository

// limits.go enforces player-set wagering limits, and is the write path for
// setting them.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS LIVES INSIDE THE WALLET LOCK
// ─────────────────────────────────────────────────────────────────────────────
// A limit checked outside the lock is not a limit, it is a suggestion.
//
// Consider a player with a £50 daily loss limit and £0 consumed, firing twenty
// concurrent £10 spins. If each request read the usage on its own connection,
// all twenty would read 0, all twenty would compute 10 <= 50, and all twenty
// would settle: £200 of losses against a £50 cap. The check would pass its unit
// tests and fail the only scenario it exists for, because "am I within my
// limit" and "consume some of my limit" would be two separate observations of a
// value that changed in between.
//
// Both halves therefore happen on the tx handle that already holds
// SELECT ... FOR UPDATE on the player's wallet row:
//
//	lock wallet ─→ read limits + usage ─→ decide ─→ settle ─→ record usage ─→ commit
//
// Postgres serializes the twenty transactions on that row, so each one reads the
// total the previous one committed. The nineteenth sees £180 consumed and is
// refused. No new lock is introduced and no lock ordering changes, so the
// deadlock surface is exactly what it was — the limit rides along on the
// serialization the money path already needed for its own correctness.
//
// ─────────────────────────────────────────────────────────────────────────────
// WAGER vs LOSS
// ─────────────────────────────────────────────────────────────────────────────
// WAGER counts every stake. LOSS counts stakes net of returns, so a player who
// is up for the period has consumed none of it — and its usage is allowed to go
// NEGATIVE, which is the part that is easy to get wrong. Clamping a winning
// period at zero would hand back headroom on the way down: a player up 500 who
// then loses 500 + L has lost L net, but against a clamped counter they would
// still appear to have L left.
//
// The check is made BEFORE the outcome is known, so it is deliberately
// conservative: a wager is admitted only if the stake alone would fit, as though
// it were to lose entirely. A spin that then wins refunds its consumption when
// the settlement records the net. The alternative — admitting a wager on the
// assumption it might win — would let a single large stake exceed the cap
// whenever it happened to lose.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Limit kinds, mirroring the limit_kind ENUM (000011).
const (
	LimitWager = "WAGER"
	LimitLoss  = "LOSS"
)

// Limit periods, mirroring the limit_period ENUM (000011).
const (
	PeriodDaily   = "DAILY"
	PeriodWeekly  = "WEEKLY"
	PeriodMonthly = "MONTHLY"
)

// LimitIncreaseCoolOff is the delay a limit INCREASE must serve before it takes
// effect. A decrease serves nothing and applies at once.
//
// 24 hours. The asymmetry is the control: a player mid-session who wants a
// higher cap is, at that moment, the person least able to judge whether they
// should have one, and the delay hands the decision back to a calmer version of
// them. Shortening this is a policy change with the same evidential weight as
// shortening the self-exclusion floor, so it lives here as a named constant
// rather than as configuration somebody can quietly lower.
const LimitIncreaseCoolOff = 24 * time.Hour

var validLimitKinds = map[string]struct{}{
	LimitWager: {}, LimitLoss: {},
}

var validLimitPeriods = map[string]struct{}{
	PeriodDaily: {}, PeriodWeekly: {}, PeriodMonthly: {},
}

// limitRow is one limit and the usage standing against it for the CURRENT
// period, as read inside the lock.
type limitRow struct {
	Kind      string
	Period    string
	Effective decimal.Decimal // the value in force NOW, pending increase resolved
	Used      decimal.Decimal // consumed within the current period; may be negative for LOSS
}

const (
	// sqlSelectEffectiveLimits resolves the cooling-off rule and joins the
	// current period's usage in ONE round trip, so the lock window grows by a
	// single statement rather than by one per limit.
	//
	// The CASE is the whole cooling-off mechanism: a pending increase becomes
	// the effective value the moment its deadline passes, with nothing scheduled
	// and nothing to miss. now() is the DATABASE clock, the same one the usage
	// bucket is derived from, so a limit cannot be in force by one clock and its
	// usage accounted by another.
	sqlSelectEffectiveLimits = `
		SELECT l.limit_kind,
		       l.period,
		       CASE
		           WHEN l.pending_effective_at IS NOT NULL AND l.pending_effective_at <= now()
		           THEN l.pending_amount
		           ELSE l.amount
		       END AS effective_amount,
		       COALESCE(u.used, 0) AS used
		FROM player_limits l
		LEFT JOIN player_limit_usage u
		       ON u.player_id    = l.player_id
		      AND u.limit_kind   = l.limit_kind
		      AND u.period       = l.period
		      AND u.period_start = limit_period_start(l.period, now())
		WHERE l.player_id = $1
		  AND l.limit_kind = ANY($2::limit_kind[])`

	// sqlUpsertLimitUsage adds a delta to every period bucket the player has a
	// limit of this kind on.
	//
	// Driven by a SELECT over player_limits rather than by a caller-supplied
	// list of periods: the set of buckets to move is exactly the set of limits
	// that exist, so the read that decided the wager and the write that records
	// it cannot describe different sets. A player with no limit of this kind
	// writes no rows at all.
	sqlUpsertLimitUsage = `
		INSERT INTO player_limit_usage (player_id, limit_kind, period, period_start, used)
		SELECT l.player_id, l.limit_kind, l.period, limit_period_start(l.period, now()), $3::numeric
		FROM player_limits l
		WHERE l.player_id = $1 AND l.limit_kind = $2::limit_kind
		ON CONFLICT (player_id, limit_kind, period, period_start)
		DO UPDATE SET used = player_limit_usage.used + EXCLUDED.used`
)

// readEffectiveLimits loads the player's limits of the given kinds, with the
// current period's usage, on the supplied tx handle.
//
// MUST run on a tx that already holds the wallet lock. Called on a pool
// connection it would return a snapshot unordered with respect to this player's
// in-flight wagers, which is the precise failure this file exists to prevent.
func readEffectiveLimits(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, kinds []string) ([]limitRow, error) {
	rows, err := tx.Query(ctx, sqlSelectEffectiveLimits, playerID, kinds)
	if err != nil {
		return nil, fmt.Errorf("select player limits: %w", err)
	}
	defer rows.Close()

	var out []limitRow
	for rows.Next() {
		var r limitRow
		if err := rows.Scan(&r.Kind, &r.Period, &r.Effective, &r.Used); err != nil {
			return nil, fmt.Errorf("scan player limit: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate player limits: %w", err)
	}
	return out, nil
}

// addLimitUsage moves every bucket for one kind by delta. A negative delta is
// how a win gives back the loss headroom its stake consumed.
func addLimitUsage(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, kind string, delta decimal.Decimal) error {
	if _, err := tx.Exec(ctx, sqlUpsertLimitUsage, playerID, kind, delta); err != nil {
		return fmt.Errorf("record %s usage: %w", kind, err)
	}
	return nil
}

// assertStakeWithinLimits is the decision, isolated from its I/O so the rule can
// be exercised exhaustively without a database.
//
// Conservative by construction: it asks whether the STAKE alone fits, for both
// kinds. For WAGER that is simply the definition. For LOSS it assumes the wager
// loses in full, because the outcome is not known at this point and admitting a
// wager on the hope of a win would let one large stake breach the cap whenever
// it happened to lose.
func assertStakeWithinLimits(rows []limitRow, stake domain.Money) error {
	amount := stake.Decimal()
	for _, r := range rows {
		projected := r.Used.Add(amount)
		if projected.GreaterThan(r.Effective) {
			return fmt.Errorf("%w: %s %s limit %s, %s consumed, %s requested",
				errs.ErrLimitExceeded,
				strings.ToLower(r.Period), strings.ToLower(r.Kind),
				r.Effective.StringFixed(domain.MoneyScale),
				r.Used.StringFixed(domain.MoneyScale),
				amount.StringFixed(domain.MoneyScale))
		}
	}
	return nil
}

// enforceWagerLimits is the call every wagering path makes, immediately after
// taking the wallet lock and before any money moves.
//
// One statement, one decision. When the player has no limits the query returns
// no rows and this costs a single round trip — the price of the control for the
// players who have not set one.
func enforceWagerLimits(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, stake domain.Money) error {
	rows, err := readEffectiveLimits(ctx, tx, playerID, []string{LimitWager, LimitLoss})
	if err != nil {
		return err
	}
	return assertStakeWithinLimits(rows, stake)
}

// recordWagerUsage books a settled wager against both counters.
//
// Called AFTER the settlement statement, on the same tx, so a wager that fails
// to settle consumes nothing. net is the amount actually lost: the stake for a
// losing round, stake less return for a winning one, and negative when the
// return exceeds the stake.
func recordWagerUsage(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, stake, net domain.Money) error {
	if err := addLimitUsage(ctx, tx, playerID, LimitWager, stake.Decimal()); err != nil {
		return err
	}
	return addLimitUsage(ctx, tx, playerID, LimitLoss, net.Decimal())
}

// ----------------------------------------------------------------------------
// The write path
// ----------------------------------------------------------------------------

// SetPlayerLimitRequest sets or revises one limit.
type SetPlayerLimitRequest struct {
	OperatorCode string
	PlayerID     uuid.UUID

	Kind   string // WAGER | LOSS
	Period string // DAILY | WEEKLY | MONTHLY
	Amount domain.Money

	ActorType string
	ActorRef  string
}

// SetPlayerLimitResult reports what the request actually did — which is not
// always what it asked for, since an increase is deferred.
type SetPlayerLimitResult struct {
	PlayerID  uuid.UUID    `json:"player_id"`
	ChangeID  uuid.UUID    `json:"change_id"`
	Kind      string       `json:"limit_kind"`
	Period    string       `json:"period"`
	Direction string       `json:"direction"` // SET | DECREASE | INCREASE
	Amount    domain.Money `json:"amount"`    // in force NOW

	// PendingAmount and EffectiveAt are set only for an INCREASE: the value the
	// player asked for, and when they will actually get it. The client renders
	// these; the cap in force until then is Amount.
	PendingAmount *domain.Money `json:"pending_amount,omitempty"`
	EffectiveAt   *time.Time    `json:"effective_at,omitempty"`
}

const (
	sqlSelectLimitForUpdate = `
		SELECT amount FROM player_limits
		WHERE player_id = $1 AND limit_kind = $2::limit_kind AND period = $3::limit_period
		FOR UPDATE`

	// A decrease (or a first limit) sets amount and CLEARS any pending increase.
	// Clearing is the point: a player who lowers their cap while an increase is
	// still cooling off must not have that increase land on them tomorrow.
	sqlUpsertLimitImmediate = `
		INSERT INTO player_limits (player_id, limit_kind, period, amount)
		VALUES ($1, $2::limit_kind, $3::limit_period, $4::numeric)
		ON CONFLICT (player_id, limit_kind, period)
		DO UPDATE SET amount = EXCLUDED.amount,
		              pending_amount = NULL,
		              pending_effective_at = NULL`

	// An increase leaves amount untouched and parks the new value. The deadline
	// is computed from the DATABASE clock, so the cooling-off period cannot be
	// shortened by a caller with a fast watch.
	sqlSetLimitPending = `
		UPDATE player_limits
		SET pending_amount = $4::numeric,
		    pending_effective_at = now() + $5::interval
		WHERE player_id = $1 AND limit_kind = $2::limit_kind AND period = $3::limit_period
		RETURNING pending_effective_at`

	sqlInsertLimitChange = `
		INSERT INTO player_limit_changes
			(player_id, limit_kind, period, previous_amount, requested_amount,
			 direction, effective_at, actor_type, actor_ref)
		VALUES ($1, $2::limit_kind, $3::limit_period, $4::numeric, $5::numeric,
		        $6, $7, $8::status_actor, $9)
		RETURNING id`
)

// ProcessSetPlayerLimit sets or revises a player limit, applying the
// cooling-off asymmetry and recording the change.
//
// The limit row and the audit row commit together, for the same reason the
// status change and its audit row do: a cap that moved with no record of who
// moved it and when is not evidence that the cooling-off period was served.
func (e *engine) ProcessSetPlayerLimit(ctx context.Context, req SetPlayerLimitRequest) (SetPlayerLimitResult, error) {
	req.normalize()
	if err := req.validate(); err != nil {
		return SetPlayerLimitResult{}, err
	}

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SetPlayerLimitResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The wallet lock, exactly as ProcessStatusTransition takes it and for the
	// same reason: this is the lock the wagering paths contend on, so taking it
	// here means a limit change cannot land halfway through a settling wager.
	// A decrease is therefore genuinely immediate — the very next wager to
	// acquire the lock sees it — rather than immediate-ish.
	if _, err := selectWalletForUpdate(ctx, tx, req.PlayerID); err != nil {
		return SetPlayerLimitResult{}, err
	}
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return SetPlayerLimitResult{}, err
	}

	var previous *decimal.Decimal
	var current decimal.Decimal
	err = tx.QueryRow(ctx, sqlSelectLimitForUpdate, req.PlayerID, req.Kind, req.Period).Scan(&current)
	switch {
	case err == nil:
		previous = &current
	case errors.Is(err, pgx.ErrNoRows):
		// No limit of this kind yet.
	default:
		return SetPlayerLimitResult{}, fmt.Errorf("select player limit: %w", err)
	}

	requested := req.Amount.Decimal()
	direction := directionFor(previous, requested)

	result := SetPlayerLimitResult{
		PlayerID: req.PlayerID,
		Kind:     req.Kind,
		Period:   req.Period,
	}
	var effectiveAt *time.Time

	switch direction {
	case limitDirectionIncrease:
		// The row already exists — an increase is only classified as such when
		// there was something to increase — so the pending columns are set in
		// place and `amount` is left exactly where it was.
		var at time.Time
		if err := tx.QueryRow(ctx, sqlSetLimitPending,
			req.PlayerID, req.Kind, req.Period, requested, LimitIncreaseCoolOff,
		).Scan(&at); err != nil {
			return SetPlayerLimitResult{}, fmt.Errorf("park limit increase: %w", err)
		}
		effectiveAt = &at

		inForce, err := domain.NewMoney(current)
		if err != nil {
			return SetPlayerLimitResult{}, fmt.Errorf("current limit: %w", err)
		}
		pending := req.Amount
		result.Amount = inForce
		result.PendingAmount = &pending
		result.EffectiveAt = &at

	case limitDirectionSet, limitDirectionDecrease:
		if _, err := tx.Exec(ctx, sqlUpsertLimitImmediate,
			req.PlayerID, req.Kind, req.Period, requested); err != nil {
			return SetPlayerLimitResult{}, fmt.Errorf("apply limit: %w", err)
		}
		result.Amount = req.Amount

	default:
		// Re-stating the same value changes nothing and must not be recorded as
		// though a decision were made. Reusing ErrStatusUnchanged keeps one
		// "you asked for the state you already have" signal across the
		// compliance surface, which Zone 2 already treats as idempotent success.
		return SetPlayerLimitResult{}, fmt.Errorf("%w: %s %s limit is already %s",
			errs.ErrStatusUnchanged, strings.ToLower(req.Period), strings.ToLower(req.Kind), req.Amount)
	}

	var actorRef *string
	if req.ActorRef != "" {
		actorRef = &req.ActorRef
	}
	var prevArg any
	if previous != nil {
		prevArg = *previous
	}

	var changeID uuid.UUID
	if err := tx.QueryRow(ctx, sqlInsertLimitChange,
		req.PlayerID, req.Kind, req.Period, prevArg, requested,
		string(direction), effectiveAt, req.ActorType, actorRef,
	).Scan(&changeID); err != nil {
		return SetPlayerLimitResult{}, fmt.Errorf("record limit change: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return SetPlayerLimitResult{}, fmt.Errorf("commit: %w", err)
	}

	result.ChangeID = changeID
	result.Direction = string(direction)
	return result, nil
}

type limitDirection string

const (
	limitDirectionSet       limitDirection = "SET"
	limitDirectionDecrease  limitDirection = "DECREASE"
	limitDirectionIncrease  limitDirection = "INCREASE"
	limitDirectionUnchanged limitDirection = "UNCHANGED"
)

// directionFor classifies a request against the value already in force.
//
// A FIRST limit is SET, not INCREASE, and so applies immediately however large
// it is. That is deliberate: before it existed the player had no cap at all, so
// there is nothing to protect them from by delaying it, and making a first limit
// wait 24 hours would mean a player who decides to set one is unprotected for a
// day — punishing exactly the decision the control wants to encourage.
func directionFor(previous *decimal.Decimal, requested decimal.Decimal) limitDirection {
	if previous == nil {
		return limitDirectionSet
	}
	switch {
	case requested.LessThan(*previous):
		return limitDirectionDecrease
	case requested.GreaterThan(*previous):
		return limitDirectionIncrease
	default:
		return limitDirectionUnchanged
	}
}

func (r *SetPlayerLimitRequest) normalize() {
	r.Kind = strings.ToUpper(strings.TrimSpace(r.Kind))
	r.Period = strings.ToUpper(strings.TrimSpace(r.Period))
	r.ActorType = strings.ToUpper(strings.TrimSpace(r.ActorType))
	r.ActorRef = strings.TrimSpace(r.ActorRef)
}

func (r SetPlayerLimitRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if _, ok := validLimitKinds[r.Kind]; !ok {
		return fmt.Errorf("%w: unknown limit_kind %q", errs.ErrInvalidAmount, r.Kind)
	}
	if _, ok := validLimitPeriods[r.Period]; !ok {
		return fmt.Errorf("%w: unknown period %q", errs.ErrInvalidAmount, r.Period)
	}
	// Zero is legitimate — "I do not want to wager at all" is a limit a player
	// is entitled to set — so only a negative value is refused.
	if r.Amount.IsNegative() {
		return fmt.Errorf("%w: limit amount must be >= 0", errs.ErrInvalidAmount)
	}
	if err := domain.CheckAmountBound(r.Amount); err != nil {
		return err
	}
	if _, ok := validActorTypes[r.ActorType]; !ok {
		return fmt.Errorf("%w: unknown actor_type %q", errs.ErrInvalidAmount, r.ActorType)
	}
	if len(r.ActorRef) > maxTransitionActorRefLen {
		return fmt.Errorf("%w: actor_ref exceeds %d characters", errs.ErrInvalidAmount, maxTransitionActorRefLen)
	}
	return nil
}
