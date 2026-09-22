package repository

// kyc.go implements Zone 1's side of the C4 KYC reconciliation loop: it
// ingests identity-verification decisions relayed by the Gateway (Zone 2)
// and reconciles them against player status.
//
// Zone 1 is the single source of truth for the resulting player state per
// cursor rule G1 — the Gateway authenticates, signs, and forwards; it never
// asserts a post-state. This file only ever runs on an already
// HMAC-verified request (see internal/api/kyc.go).
//
// Unlike /bet, /win, /rollback, RecordDecision does NOT use the Redis
// idempotency barrier. KYC decisions are low-volume, non-hot-path traffic,
// and the DB-level UNIQUE(event_id) constraint on kyc_decisions (migration
// 000009) is itself a durable, sufficient replay guard — the append-only
// audit row IS the idempotency record, so there is no separate cache to
// keep in sync with it.
//
// Out-of-order resolution: a decision is only APPLIED (mutates
// users.status / users.kyc_verified_at) when it is VERIFIED and its
// decided_at is strictly newer than the player's current kyc_verified_at.
// A stale/older decision — of either type — is always recorded for audit
// but never applied, so a late-arriving webhook can never downgrade or
// redundantly re-process a player already verified by a newer decision.
// REJECTED never has a status effect: it is audited so a compliance
// reviewer can always reconstruct what the provider decided, but it does
// not map onto any user_status value.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// KYCEngine ingests identity-verification decisions relayed by the Gateway
// and reconciles them against player status.
type KYCEngine interface {
	// RecordDecision audits an incoming KYC decision and, unless it is
	// superseded by an already-applied newer decision, applies it.
	RecordDecision(ctx context.Context, req KYCDecisionRequest) (KYCDecisionResult, error)
}

// NewKYC constructs a KYCEngine backed by the same underlying *engine type
// used by Engine/CasinoEngine, so callers share one pool/idem/logger triple
// across all three interfaces. idem is accepted for that consistency even
// though RecordDecision does not use it today.
func NewKYC(db DB, idem cache.Store, logger *slog.Logger) KYCEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &engine{db: db, idem: idem, logger: logger}
}

// ----------------------------------------------------------------------------
// Request / result types
// ----------------------------------------------------------------------------

// KYCDecisionRequest is a single decision delivery relayed from the KYC
// provider via the Gateway.
type KYCDecisionRequest struct {
	ExternalID string // operator's stable player key (users.external_id)
	EventID    string // Gateway/provider idempotency key for this delivery
	Decision   string // "VERIFIED" or "REJECTED"
	// DecidedAt is the PROVIDER's decision timestamp, carried through the
	// Gateway — never the time this request was received. It is the value
	// out-of-order resolution compares against users.kyc_verified_at.
	DecidedAt time.Time
	Reason    string // optional (e.g. rejection reason); "" -> NULL
}

// KYCDecisionResult reports how a decision was resolved.
type KYCDecisionResult struct {
	PlayerID uuid.UUID `json:"player_id"`
	EventID  string    `json:"event_id"`
	Decision string    `json:"decision"`
	// Applied is false whenever the decision was audited only: REJECTED
	// always, and a VERIFIED decision superseded by a newer applied one.
	Applied bool   `json:"applied"`
	Reason  string `json:"reason,omitempty"`
}

// ----------------------------------------------------------------------------
// SQL
// ----------------------------------------------------------------------------

const (
	// sqlKYCLockUser locks the player row for the duration of the decision.
	// FOR UPDATE serializes concurrent deliveries for the same player so the
	// out-of-order comparison below is race-free.
	sqlKYCLockUser = `
		SELECT id, kyc_verified_at
		FROM users
		WHERE external_id = $1
		FOR UPDATE
	`

	// sqlKYCInsertDecision is the append-only audit write. event_id is
	// UNIQUE (migration 000009): a duplicate webhook delivery raises 23505,
	// caught by the caller and resolved via recoverKYCReplay.
	sqlKYCInsertDecision = `
		INSERT INTO kyc_decisions
			(player_id, external_id, event_id, decision, decided_at, applied, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`

	// sqlKYCApplyVerified activates the player and stamps the out-of-order
	// resolution baseline. Only reached when applied == true.
	sqlKYCApplyVerified = `
		UPDATE users
		SET status = 'ACTIVE', kyc_verified_at = $2
		WHERE id = $1
	`

	// sqlKYCSelectByEventID re-reads the audit row for a replayed delivery.
	sqlKYCSelectByEventID = `
		SELECT player_id, external_id, decision, applied, reason
		FROM kyc_decisions
		WHERE event_id = $1
	`
)

// ----------------------------------------------------------------------------
// RecordDecision
// ----------------------------------------------------------------------------

func (e *engine) RecordDecision(ctx context.Context, req KYCDecisionRequest) (result KYCDecisionResult, err error) {
	if err := req.validate(); err != nil {
		return KYCDecisionResult{}, err
	}

	ctx, span := telemetry.StartSpan(ctx, "db.kyc_decision_tx",
		attribute.String("event_id", req.EventID))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return KYCDecisionResult{}, fmt.Errorf("begin tx: %w", err)
	}
	// Rollback after Commit is a no-op in pgx — safe unconditional defer.
	defer func() { _ = tx.Rollback(ctx) }()

	// pgtype.Timestamptz (not *time.Time): kyc_verified_at is NULL until a
	// player's first applied VERIFIED decision, and the driver's nullable
	// scalar scan path needs the explicit Valid flag rather than a bare
	// pointer (same convention as internal/worker/aggregator.go).
	var (
		playerID   uuid.UUID
		verifiedAt pgtype.Timestamptz
	)
	err = tx.QueryRow(ctx, sqlKYCLockUser, req.ExternalID).Scan(&playerID, &verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return KYCDecisionResult{}, errs.ErrPlayerNotFound
	}
	if err != nil {
		return KYCDecisionResult{}, fmt.Errorf("lock user: %w", err)
	}

	// Out-of-order resolution (Task C4, requirement 3): only a VERIFIED
	// decision strictly newer than the current baseline is ever applied.
	applied := req.Decision == "VERIFIED" &&
		(!verifiedAt.Valid || req.DecidedAt.After(verifiedAt.Time))

	var decisionID uuid.UUID
	err = tx.QueryRow(ctx, sqlKYCInsertDecision,
		playerID, req.ExternalID, req.EventID, req.Decision, req.DecidedAt, applied, nullableString(req.Reason),
	).Scan(&decisionID)
	if err != nil {
		if isUniqueViolation(err) {
			// Stale FOR UPDATE lock released by the deferred Rollback.
			// Duplicate event_id means a prior delivery already committed —
			// reconstruct and return its recorded outcome rather than
			// erroring or silently reprocessing.
			_ = tx.Rollback(ctx)
			return e.recoverKYCReplay(ctx, req.EventID)
		}
		return KYCDecisionResult{}, fmt.Errorf("insert kyc decision: %w", err)
	}
	span.SetAttributes(attribute.String("kyc_decision_id", decisionID.String()))

	if applied {
		tag, execErr := tx.Exec(ctx, sqlKYCApplyVerified, playerID, req.DecidedAt)
		if execErr != nil {
			return KYCDecisionResult{}, fmt.Errorf("apply verified: %w", execErr)
		}
		if tag.RowsAffected() != 1 {
			return KYCDecisionResult{}, fmt.Errorf("apply verified: expected 1 row, got %d", tag.RowsAffected())
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return KYCDecisionResult{}, fmt.Errorf("commit: %w", err)
	}

	return KYCDecisionResult{
		PlayerID: playerID,
		EventID:  req.EventID,
		Decision: req.Decision,
		Applied:  applied,
		Reason:   req.Reason,
	}, nil
}

// recoverKYCReplay handles a duplicate event_id delivery: the audit row
// already exists from a prior successful RecordDecision, so this is a
// replay, not a new decision. The outcome is reconstructed from the
// append-only audit row itself — unlike the ledger's Ghost-Spin recovery,
// there is no separate cache to fall back to, because the kyc_decisions row
// IS the durable record.
func (e *engine) recoverKYCReplay(ctx context.Context, eventID string) (KYCDecisionResult, error) {
	var (
		playerID   uuid.UUID
		externalID string
		decision   string
		applied    bool
		reason     *string
	)
	err := e.db.QueryRow(ctx, sqlKYCSelectByEventID, eventID).
		Scan(&playerID, &externalID, &decision, &applied, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		// 23505 came from this table but the row vanished — impossible
		// (append-only, never deleted), but surface conflict rather than
		// masking it as success.
		return KYCDecisionResult{}, fmt.Errorf("%w: event %s vanished post-conflict",
			errs.ErrTransactionConflict, eventID)
	}
	if err != nil {
		return KYCDecisionResult{}, fmt.Errorf("kyc replay lookup: %w", err)
	}

	result := KYCDecisionResult{
		PlayerID: playerID,
		EventID:  eventID,
		Decision: decision,
		Applied:  applied,
	}
	if reason != nil {
		result.Reason = *reason
	}
	return result, nil
}

// ----------------------------------------------------------------------------
// Validation
// ----------------------------------------------------------------------------

func (r KYCDecisionRequest) validate() error {
	if r.ExternalID == "" {
		return fmt.Errorf("%w: empty external_id", errs.ErrInvalidAmount)
	}
	if r.EventID == "" {
		return fmt.Errorf("%w: empty event_id", errs.ErrInvalidAmount)
	}
	if r.Decision != "VERIFIED" && r.Decision != "REJECTED" {
		return fmt.Errorf("%w: decision must be VERIFIED or REJECTED, got %q", errs.ErrInvalidAmount, r.Decision)
	}
	if r.DecidedAt.IsZero() {
		return fmt.Errorf("%w: decided_at is required", errs.ErrInvalidAmount)
	}
	return nil
}
