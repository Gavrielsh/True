package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Regex shortcuts for the KYC-specific SQL constants (kyc.go).
const (
	rxKYCLockUser        = `SELECT id, kyc_verified_at FROM users WHERE external_id`
	rxKYCInsertDecision  = `INSERT INTO kyc_decisions`
	rxKYCApplyVerified   = `UPDATE users`
	rxKYCSelectByEventID = `SELECT player_id, external_id, decision, applied, reason FROM kyc_decisions`
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

// ----------------------------------------------------------------------------
// RecordDecision — VERIFIED applies (first-ever decision, baseline is NULL)
// ----------------------------------------------------------------------------

func TestRecordDecision_Verified_AppliesFromNilBaseline(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	decisionID := uuid.New()
	decidedAt := mustTime(t, "2026-01-01T00:00:00Z")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).AddRow(playerID, pgtype.Timestamptz{}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-1", "evt-1", "VERIFIED", decidedAt, true, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(decisionID))
	mock.ExpectExec(rxKYCApplyVerified).
		WithArgs(playerID, decidedAt).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	got, err := e.RecordDecision(context.Background(), KYCDecisionRequest{
		ExternalID: "ext-1",
		EventID:    "evt-1",
		Decision:   "VERIFIED",
		DecidedAt:  decidedAt,
	})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if !got.Applied {
		t.Error("Applied: got false, want true")
	}
	if got.PlayerID != playerID {
		t.Errorf("PlayerID: got %v, want %v", got.PlayerID, playerID)
	}
}

// ----------------------------------------------------------------------------
// RecordDecision — VERIFIED applies over an older baseline
// ----------------------------------------------------------------------------

func TestRecordDecision_Verified_AppliesWhenNewer(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	decisionID := uuid.New()
	baseline := mustTime(t, "2026-01-01T00:00:00Z")
	decidedAt := mustTime(t, "2026-01-02T00:00:00Z")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-2").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).
			AddRow(playerID, pgtype.Timestamptz{Time: baseline, Valid: true}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-2", "evt-2", "VERIFIED", decidedAt, true, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(decisionID))
	mock.ExpectExec(rxKYCApplyVerified).
		WithArgs(playerID, decidedAt).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	got, err := e.RecordDecision(context.Background(), KYCDecisionRequest{
		ExternalID: "ext-2",
		EventID:    "evt-2",
		Decision:   "VERIFIED",
		DecidedAt:  decidedAt,
	})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if !got.Applied {
		t.Error("Applied: got false, want true")
	}
}

// ----------------------------------------------------------------------------
// RecordDecision — out-of-order: a stale VERIFIED must NOT downgrade/replay
// over a newer already-applied one. Audited, but no UPDATE is issued.
// ----------------------------------------------------------------------------

func TestRecordDecision_Verified_StaleDoesNotApply(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	decisionID := uuid.New()
	baseline := mustTime(t, "2026-01-05T00:00:00Z")  // newer, already-applied decision
	decidedAt := mustTime(t, "2026-01-01T00:00:00Z") // stale delivery arriving late

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-3").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).
			AddRow(playerID, pgtype.Timestamptz{Time: baseline, Valid: true}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-3", "evt-3", "VERIFIED", decidedAt, false, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(decisionID))
	// No sqlKYCApplyVerified expectation: pgxmock's ExpectationsWereMet (via
	// newEngine's t.Cleanup) fails the test if an UPDATE is issued.
	mock.ExpectCommit()

	got, err := e.RecordDecision(context.Background(), KYCDecisionRequest{
		ExternalID: "ext-3",
		EventID:    "evt-3",
		Decision:   "VERIFIED",
		DecidedAt:  decidedAt,
	})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if got.Applied {
		t.Error("Applied: got true, want false for a stale decision")
	}
}

// ----------------------------------------------------------------------------
// RecordDecision — REJECTED is always audit-only, even when "newer".
// ----------------------------------------------------------------------------

func TestRecordDecision_Rejected_NeverApplies(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	decisionID := uuid.New()
	decidedAt := mustTime(t, "2026-01-01T00:00:00Z")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-4").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).AddRow(playerID, pgtype.Timestamptz{}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-4", "evt-4", "REJECTED", decidedAt, false, "manual review failed").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(decisionID))
	mock.ExpectCommit()

	got, err := e.RecordDecision(context.Background(), KYCDecisionRequest{
		ExternalID: "ext-4",
		EventID:    "evt-4",
		Decision:   "REJECTED",
		DecidedAt:  decidedAt,
		Reason:     "manual review failed",
	})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if got.Applied {
		t.Error("Applied: got true, want false for REJECTED")
	}
}

// ----------------------------------------------------------------------------
// RecordDecision — duplicate event_id replay (23505) reconstructs the
// originally-recorded outcome instead of reprocessing.
// ----------------------------------------------------------------------------

func TestRecordDecision_DuplicateEventID_ReplaysOriginal(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	playerID := uuid.New()
	decidedAt := mustTime(t, "2026-01-01T00:00:00Z")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-5").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).AddRow(playerID, pgtype.Timestamptz{}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-5", "evt-5", "VERIFIED", decidedAt, true, nil).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	mock.ExpectQuery(rxKYCSelectByEventID).WithArgs("evt-5").
		WillReturnRows(pgxmock.NewRows([]string{"player_id", "external_id", "decision", "applied", "reason"}).
			AddRow(playerID, "ext-5", "VERIFIED", true, nil))

	got, err := e.RecordDecision(context.Background(), KYCDecisionRequest{
		ExternalID: "ext-5",
		EventID:    "evt-5",
		Decision:   "VERIFIED",
		DecidedAt:  decidedAt,
	})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if !got.Applied {
		t.Error("Applied: got false, want true (replayed original outcome)")
	}
	if got.PlayerID != playerID {
		t.Errorf("PlayerID: got %v, want %v", got.PlayerID, playerID)
	}
}

// ----------------------------------------------------------------------------
// RecordDecision — unknown external_id.
// ----------------------------------------------------------------------------

func TestRecordDecision_PlayerNotFound(t *testing.T) {
	t.Parallel()
	e, mock, _ := newEngine(t)
	decidedAt := mustTime(t, "2026-01-01T00:00:00Z")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-unknown").WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := e.RecordDecision(context.Background(), KYCDecisionRequest{
		ExternalID: "ext-unknown",
		EventID:    "evt-6",
		Decision:   "VERIFIED",
		DecidedAt:  decidedAt,
	})
	if !errors.Is(err, errs.ErrPlayerNotFound) {
		t.Fatalf("got %v, want wrapping ErrPlayerNotFound", err)
	}
}

// ----------------------------------------------------------------------------
// RecordDecision — validation short-circuits before touching the DB.
// ----------------------------------------------------------------------------

func TestRecordDecision_Validation(t *testing.T) {
	t.Parallel()
	decidedAt := mustTime(t, "2026-01-01T00:00:00Z")
	tests := []struct {
		name string
		req  KYCDecisionRequest
	}{
		{"empty_external_id", KYCDecisionRequest{EventID: "e", Decision: "VERIFIED", DecidedAt: decidedAt}},
		{"empty_event_id", KYCDecisionRequest{ExternalID: "x", Decision: "VERIFIED", DecidedAt: decidedAt}},
		{"bad_decision", KYCDecisionRequest{ExternalID: "x", EventID: "e", Decision: "MAYBE", DecidedAt: decidedAt}},
		{"zero_decided_at", KYCDecisionRequest{ExternalID: "x", EventID: "e", Decision: "VERIFIED"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newEngine(t) // no DB expectations: must short-circuit
			_, err := e.RecordDecision(context.Background(), tc.req)
			if !errors.Is(err, errs.ErrInvalidAmount) {
				t.Errorf("got %v, want wrapping ErrInvalidAmount", err)
			}
		})
	}
}
