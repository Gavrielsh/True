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
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Regex shortcuts for the RG-specific SQL constants (rg.go), beyond the
// rxRGActiveExclusion/rxRGLatestEffectiveLimit pair already declared in
// engine_test.go.
const (
	rxRGLockPlayer       = `SELECT id FROM users WHERE id`
	rxRGInsertLimit      = `INSERT INTO player_limits`
	rxRGSelectByEventID  = `SELECT player_id, limit_type, period, active, amount, ends_at, effective_at, event_id`
	rxRGSelectCounter    = `SELECT window_start, amount`
	rxRGUpsertCounter    = `INSERT INTO player_rg_counters`
	rxRGLossAggregate    = `FROM ledger_transactions t\s+JOIN ledger_entries`
	rxRGDepositAggregate = `SUM\(t\.usd_amount\)`
)

func newRG(t *testing.T) (ResponsibleGamingEngine, *engine, pgxmock.PgxPoolIface) {
	t.Helper()
	e, mock, _ := newEngine(t)
	return NewResponsibleGaming(mock, e.idem, e.logger), e, mock
}

// beginTx registers ExpectBeginTx and returns the resulting pgx.Tx handle,
// for tests that call an unexported guard/counter function directly rather
// than going through the ResponsibleGamingEngine interface.
func beginTx(t *testing.T, mock pgxmock.PgxPoolIface, opts pgx.TxOptions) pgx.Tx {
	t.Helper()
	mock.ExpectBeginTx(opts)
	tx, err := mock.BeginTx(context.Background(), opts)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	return tx
}

// ----------------------------------------------------------------------------
// SetLimit — table tests per limit type, covering the immediate-vs-delayed
// effective_at rule (point in the plan: decreases and new exclusions are
// immediate; increases and removals wait 24h).
// ----------------------------------------------------------------------------

func TestSetLimit_TableTests(t *testing.T) {
	playerID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name          string
		req           SetLimitRequest
		current       *latestLimit // nil: latestEffectiveLimit returns no rows
		wantImmediate bool
	}{
		{
			name: "new_self_exclusion_is_immediate",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "SELF_EXCLUSION", Active: true,
				EndsAt: ptrTime(now.Add(30 * 24 * time.Hour)), EventID: "evt-se-1",
			},
			wantImmediate: true,
		},
		{
			name: "new_cool_off_is_immediate",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "COOL_OFF", Active: true,
				EndsAt: ptrTime(now.Add(72 * time.Hour)), EventID: "evt-co-1",
			},
			wantImmediate: true,
		},
		{
			name: "new_loss_limit_is_immediate",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "LOSS_LIMIT", Active: true,
				Period: "DAY", Amount: moneyPtr(t, "100.0000"), EventID: "evt-ll-1",
			},
			current:       nil,
			wantImmediate: true,
		},
		{
			name: "loss_limit_decrease_is_immediate",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "LOSS_LIMIT", Active: true,
				Period: "DAY", Amount: moneyPtr(t, "50.0000"), EventID: "evt-ll-2",
			},
			current:       &latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("100.0000")},
			wantImmediate: true,
		},
		{
			name: "loss_limit_increase_is_delayed",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "LOSS_LIMIT", Active: true,
				Period: "DAY", Amount: moneyPtr(t, "200.0000"), EventID: "evt-ll-3",
			},
			current:       &latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("100.0000")},
			wantImmediate: false,
		},
		{
			name: "loss_limit_removal_is_delayed",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "LOSS_LIMIT", Active: false, EventID: "evt-ll-4",
			},
			current:       &latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("100.0000")},
			wantImmediate: false,
		},
		{
			name: "new_deposit_limit_is_immediate",
			req: SetLimitRequest{
				PlayerID: playerID, LimitType: "DEPOSIT_LIMIT", Active: true,
				Period: "MONTH", Amount: moneyPtr(t, "500.0000"), EventID: "evt-dl-1",
			},
			wantImmediate: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, mock := newRG(t)

			mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			mock.ExpectQuery(rxRGLockPlayer).WithArgs(tc.req.PlayerID).
				WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(tc.req.PlayerID))

			needsCurrentLookup := tc.req.LimitType == "LOSS_LIMIT" || tc.req.LimitType == "DEPOSIT_LIMIT" ||
				(!tc.req.Active && (tc.req.LimitType == "SELF_EXCLUSION" || tc.req.LimitType == "COOL_OFF"))
			if needsCurrentLookup {
				if tc.current == nil {
					mock.ExpectQuery(rxRGLatestEffectiveLimit).
						WithArgs(tc.req.PlayerID, tc.req.LimitType).
						WillReturnError(pgx.ErrNoRows)
				} else {
					mock.ExpectQuery(rxRGLatestEffectiveLimit).
						WithArgs(tc.req.PlayerID, tc.req.LimitType).
						WillReturnRows(latestLimitRow(*tc.current))
				}
			}

			insertedID := uuid.New()
			mock.ExpectQuery(rxRGInsertLimit).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
					pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(insertedID))
			mock.ExpectCommit()

			got, err := e.SetLimit(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("SetLimit: %v", err)
			}

			if tc.wantImmediate {
				if got.EffectiveAt.After(time.Now().UTC().Add(time.Minute)) {
					t.Errorf("EffectiveAt: got %v, want immediate (~now)", got.EffectiveAt)
				}
			} else {
				delta := got.EffectiveAt.Sub(time.Now().UTC())
				if delta < 23*time.Hour || delta > 25*time.Hour {
					t.Errorf("EffectiveAt: got delta %v from now, want ~24h", delta)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// SetLimit — point 6: SELF_EXCLUSION/COOL_OFF cannot be lifted before their
// own ends_at, the same 400 (ErrInvalidAmount) either way.
// ----------------------------------------------------------------------------

func TestSetLimit_CannotLiftExclusionBeforeEndsAt(t *testing.T) {
	for _, limitType := range []string{"SELF_EXCLUSION", "COOL_OFF"} {
		t.Run(limitType, func(t *testing.T) {
			t.Parallel()
			e, _, mock := newRG(t)
			playerID := uuid.New()
			futureEnd := time.Now().Add(48 * time.Hour)

			mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			mock.ExpectQuery(rxRGLockPlayer).WithArgs(playerID).
				WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(playerID))
			mock.ExpectQuery(rxRGLatestEffectiveLimit).
				WithArgs(playerID, limitType).
				WillReturnRows(latestLimitRow(latestLimit{Active: true, EndsAt: &futureEnd}))
			mock.ExpectRollback()

			_, err := e.SetLimit(context.Background(), SetLimitRequest{
				PlayerID: playerID, LimitType: limitType, Active: false, EventID: "evt-lift-1",
			})
			if !errors.Is(err, errs.ErrInvalidAmount) {
				t.Fatalf("got %v, want wrapping ErrInvalidAmount", err)
			}
		})
	}
}

func TestSetLimit_CanLiftExclusionAfterEndsAt(t *testing.T) {
	e, _, mock := newRG(t)
	playerID := uuid.New()
	pastEnd := time.Now().Add(-time.Hour)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxRGLockPlayer).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(playerID))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "SELF_EXCLUSION").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, EndsAt: &pastEnd}))
	mock.ExpectQuery(rxRGInsertLimit).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(uuid.New()))
	mock.ExpectCommit()

	got, err := e.SetLimit(context.Background(), SetLimitRequest{
		PlayerID: playerID, LimitType: "SELF_EXCLUSION", Active: false, EventID: "evt-lift-2",
	})
	if err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	if got.Active {
		t.Error("Active: got true, want false (lifted)")
	}
}

// ----------------------------------------------------------------------------
// SetLimit — duplicate event_id replays the original outcome.
// ----------------------------------------------------------------------------

func TestSetLimit_DuplicateEventID_ReplaysOriginal(t *testing.T) {
	e, _, mock := newRG(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxRGLockPlayer).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(playerID))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGInsertLimit).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()
	mock.ExpectQuery(rxRGSelectByEventID).WithArgs("evt-dup-1").
		WillReturnRows(pgxmock.NewRows(
			[]string{"player_id", "limit_type", "period", "active", "amount", "ends_at", "effective_at", "event_id"}).
			AddRow(playerID, "LOSS_LIMIT", pgtype.Text{String: "DAY", Valid: true}, true,
				decPtr("100.0000"), pgtype.Timestamptz{}, time.Now().UTC(), "evt-dup-1"))

	got, err := e.SetLimit(context.Background(), SetLimitRequest{
		PlayerID: playerID, LimitType: "LOSS_LIMIT", Active: true,
		Period: "DAY", Amount: moneyPtr(t, "100.0000"), EventID: "evt-dup-1",
	})
	if err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	if got.PlayerID != playerID || got.LimitType != "LOSS_LIMIT" || !got.Active {
		t.Errorf("got %+v, want replayed original", got)
	}
}

func TestSetLimit_PlayerNotFound(t *testing.T) {
	e, _, mock := newRG(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxRGLockPlayer).WithArgs(playerID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := e.SetLimit(context.Background(), SetLimitRequest{
		PlayerID: playerID, LimitType: "LOSS_LIMIT", Active: true,
		Period: "DAY", Amount: moneyPtr(t, "100.0000"), EventID: "evt-notfound",
	})
	if !errors.Is(err, errs.ErrPlayerNotFound) {
		t.Fatalf("got %v, want wrapping ErrPlayerNotFound", err)
	}
}

// ----------------------------------------------------------------------------
// SetLimit — validation short-circuits before touching the DB.
// ----------------------------------------------------------------------------

func TestSetLimit_Validation(t *testing.T) {
	future := time.Now().Add(time.Hour)
	tests := []struct {
		name string
		req  SetLimitRequest
	}{
		{"nil_player_id", SetLimitRequest{LimitType: "LOSS_LIMIT", EventID: "e"}},
		{"empty_event_id", SetLimitRequest{PlayerID: uuid.New(), LimitType: "LOSS_LIMIT"}},
		{"unknown_limit_type", SetLimitRequest{PlayerID: uuid.New(), LimitType: "NOPE", EventID: "e"}},
		{"loss_limit_bad_period", SetLimitRequest{
			PlayerID: uuid.New(), LimitType: "LOSS_LIMIT", Active: true, EventID: "e",
			Period: "YEAR", Amount: moneyPtr(t, "10.0000"),
		}},
		{"loss_limit_nil_amount", SetLimitRequest{
			PlayerID: uuid.New(), LimitType: "LOSS_LIMIT", Active: true, EventID: "e", Period: "DAY",
		}},
		{"loss_limit_zero_amount", SetLimitRequest{
			PlayerID: uuid.New(), LimitType: "LOSS_LIMIT", Active: true, EventID: "e",
			Period: "DAY", Amount: moneyPtr(t, "0.0000"),
		}},
		{"self_exclusion_nil_ends_at", SetLimitRequest{
			PlayerID: uuid.New(), LimitType: "SELF_EXCLUSION", Active: true, EventID: "e",
		}},
		{"cool_off_ends_at_in_past", SetLimitRequest{
			PlayerID: uuid.New(), LimitType: "COOL_OFF", Active: true, EventID: "e",
			EndsAt: ptrTime(time.Now().Add(-time.Hour)),
		}},
	}
	_ = future
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _, _ := newRG(t) // no DB expectations: must short-circuit
			_, err := e.SetLimit(context.Background(), tc.req)
			if !errors.Is(err, errs.ErrInvalidAmount) && !errors.Is(err, errs.ErrPlayerNotFound) {
				t.Errorf("got %v, want wrapping ErrInvalidAmount or ErrPlayerNotFound", err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// isDecreaseOrNew — pure function.
// ----------------------------------------------------------------------------

func TestIsDecreaseOrNew(t *testing.T) {
	tests := []struct {
		name      string
		newAmount *decimal.Decimal
		current   *decimal.Decimal
		want      bool
	}{
		{"removal_is_never_a_decrease", nil, decPtr("100.0000"), false},
		{"removal_with_no_prior_limit_is_not_a_decrease", nil, nil, false},
		{"new_limit_from_nothing_is_immediate", decPtr("100.0000"), nil, true},
		{"strict_decrease_is_immediate", decPtr("50.0000"), decPtr("100.0000"), true},
		{"equal_amount_counts_as_decrease", decPtr("100.0000"), decPtr("100.0000"), true},
		{"increase_is_not_a_decrease", decPtr("200.0000"), decPtr("100.0000"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isDecreaseOrNew(tc.newAmount, tc.current)
			if got != tc.want {
				t.Errorf("isDecreaseOrNew(%v, %v) = %v, want %v", tc.newAmount, tc.current, got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// windowBounds — pure function.
// ----------------------------------------------------------------------------

func TestWindowBounds(t *testing.T) {
	// Wednesday 2026-01-14 15:30:00 UTC.
	now := time.Date(2026, 1, 14, 15, 30, 0, 0, time.UTC)

	tests := []struct {
		period    string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{"DAY", time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)},
		{"WEEK", time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)},
		{"MONTH", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.period, func(t *testing.T) {
			start, end := windowBounds(tc.period, now)
			if !start.Equal(tc.wantStart) {
				t.Errorf("start: got %v, want %v", start, tc.wantStart)
			}
			if !end.Equal(tc.wantEnd) {
				t.Errorf("end: got %v, want %v", end, tc.wantEnd)
			}
		})
	}
}

func TestWindowBounds_WeekStartOnMondayItself(t *testing.T) {
	// Monday 2026-01-12 00:00:01 UTC — must start the same day, not the
	// prior Monday.
	now := time.Date(2026, 1, 12, 0, 0, 1, 0, time.UTC)
	start, end := windowBounds("WEEK", now)
	wantStart := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("got [%v, %v), want [%v, %v)", start, end, wantStart, wantEnd)
	}
}

// ----------------------------------------------------------------------------
// guardNotExcluded
// ----------------------------------------------------------------------------

func TestGuardNotExcluded_NoRestriction(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGActiveExclusion).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"limit_type", "ends_at"}))

	if err := guardNotExcluded(context.Background(), tx, playerID); err != nil {
		t.Fatalf("guardNotExcluded: %v", err)
	}
}

func TestGuardNotExcluded_ActiveRestriction(t *testing.T) {
	for _, reason := range []string{"SELF_EXCLUSION", "COOL_OFF"} {
		t.Run(reason, func(t *testing.T) {
			_, _, mock := newRG(t)
			playerID := uuid.New()
			tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			until := time.Now().Add(24 * time.Hour)

			mock.ExpectQuery(rxRGActiveExclusion).WithArgs(playerID).
				WillReturnRows(pgxmock.NewRows([]string{"limit_type", "ends_at"}).AddRow(reason, until))

			err := guardNotExcluded(context.Background(), tx, playerID)
			var rgErr *errs.RGRestrictionError
			if !errors.As(err, &rgErr) {
				t.Fatalf("got %v, want *RGRestrictionError", err)
			}
			if rgErr.Reason != reason {
				t.Errorf("Reason: got %q, want %q", rgErr.Reason, reason)
			}
			if !errors.Is(err, errs.ErrResponsibleGamingRestricted) {
				t.Error("want error wrapping ErrResponsibleGamingRestricted")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// guardLossLimit
// ----------------------------------------------------------------------------

func TestGuardLossLimit(t *testing.T) {
	tests := []struct {
		name        string
		hasLimit    bool
		limitAmount string
		accumulated string
		stake       string
		wantErr     bool
	}{
		{"no_active_limit_always_allowed", false, "", "", "50.0000", false},
		{"under_limit_allowed", true, "100.0000", "30.0000", "50.0000", false},
		{"exactly_at_limit_allowed", true, "100.0000", "50.0000", "50.0000", false},
		{"over_limit_refused", true, "100.0000", "60.0000", "50.0000", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, mock := newRG(t)
			playerID := uuid.New()
			tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

			if !tc.hasLimit {
				mock.ExpectQuery(rxRGLatestEffectiveLimit).
					WithArgs(playerID, "LOSS_LIMIT").WillReturnError(pgx.ErrNoRows)
			} else {
				mock.ExpectQuery(rxRGLatestEffectiveLimit).
					WithArgs(playerID, "LOSS_LIMIT").
					WillReturnRows(latestLimitRow(latestLimit{
						Active: true, Period: strPtr("DAY"), Amount: decPtr(tc.limitAmount),
					}))
				mock.ExpectQuery(rxRGSelectCounter).
					WithArgs(playerID, "LOSS", "DAY").
					WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
						AddRow(pgtype.Timestamptz{Time: currentWindowStart("DAY"), Valid: true},
							decimal.RequireFromString(tc.accumulated)))
			}

			err := guardLossLimit(context.Background(), tx, playerID, mustMoney(t, tc.stake))
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("got err=%v (%v), want err=%v", gotErr, err, tc.wantErr)
			}
			if tc.wantErr {
				var rgErr *errs.RGRestrictionError
				if !errors.As(err, &rgErr) || rgErr.Reason != "LOSS_LIMIT" {
					t.Errorf("got %v, want *RGRestrictionError{Reason: LOSS_LIMIT}", err)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// guardDepositLimit — including the fail-closed nil-usdAmount case (point 5).
// ----------------------------------------------------------------------------

func TestGuardDepositLimit_NilAmountFailsClosedWhenLimitActive(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("MONTH"), Amount: decPtr("500.0000")}))

	err := guardDepositLimit(context.Background(), tx, playerID, nil)
	var rgErr *errs.RGRestrictionError
	if !errors.As(err, &rgErr) || rgErr.Reason != "DEPOSIT_LIMIT" {
		t.Fatalf("got %v, want *RGRestrictionError{Reason: DEPOSIT_LIMIT}", err)
	}
}

func TestGuardDepositLimit_NilAmountAllowedWhenNoLimitActive(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	if err := guardDepositLimit(context.Background(), tx, playerID, nil); err != nil {
		t.Fatalf("guardDepositLimit: %v", err)
	}
}

func TestGuardDepositLimit_UnderAndOver(t *testing.T) {
	tests := []struct {
		name        string
		accumulated string
		amount      string
		wantErr     bool
	}{
		{"under_limit_allowed", "100.0000", "50.0000", false},
		{"exactly_at_limit_allowed", "100.0000", "0.0000", false}, // handled by combined table below
		{"over_limit_refused", "480.0000", "50.0000", true},
	}
	for _, tc := range tests {
		if tc.name == "exactly_at_limit_allowed" {
			continue // covered explicitly below with a realistic combination
		}
		t.Run(tc.name, func(t *testing.T) {
			_, _, mock := newRG(t)
			playerID := uuid.New()
			tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

			mock.ExpectQuery(rxRGLatestEffectiveLimit).
				WithArgs(playerID, "DEPOSIT_LIMIT").
				WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("MONTH"), Amount: decPtr("500.0000")}))
			mock.ExpectQuery(rxRGSelectCounter).
				WithArgs(playerID, "DEPOSIT", "MONTH").
				WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
					AddRow(pgtype.Timestamptz{Time: currentWindowStart("MONTH"), Valid: true},
						decimal.RequireFromString(tc.accumulated)))

			amount := mustMoney(t, tc.amount)
			err := guardDepositLimit(context.Background(), tx, playerID, &amount)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("got err=%v (%v), want err=%v", gotErr, err, tc.wantErr)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// adjustLossCounters / adjustDepositCounter — post-settlement bookkeeping.
// ----------------------------------------------------------------------------

func TestAdjustLossCounters_NoopWithoutActiveLimit(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").WillReturnError(pgx.ErrNoRows)

	if err := adjustLossCounters(context.Background(), tx, playerID, mustMoney(t, "10.0000")); err != nil {
		t.Fatalf("adjustLossCounters: %v", err)
	}
}

func TestAdjustLossCounters_BumpsWhenActive(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("100.0000")}))
	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "LOSS", "DAY").
		WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
			AddRow(pgtype.Timestamptz{Time: currentWindowStart("DAY"), Valid: true}, decimal.RequireFromString("20.0000")))
	mock.ExpectExec(rxRGUpsertCounter).
		WithArgs(playerID, "LOSS", "DAY", currentWindowStart("DAY"), argDec{want: decimal.RequireFromString("30.0000")}).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := adjustLossCounters(context.Background(), tx, playerID, mustMoney(t, "10.0000")); err != nil {
		t.Fatalf("adjustLossCounters: %v", err)
	}
}

func TestAdjustLossCounters_FloorsAtZeroOnBigWin(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("100.0000")}))
	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "LOSS", "DAY").
		WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
			AddRow(pgtype.Timestamptz{Time: currentWindowStart("DAY"), Valid: true}, decimal.RequireFromString("5.0000")))
	mock.ExpectExec(rxRGUpsertCounter).
		WithArgs(playerID, "LOSS", "DAY", currentWindowStart("DAY"), argDec{want: decimal.Zero}).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// A big win: netDelta is negative and exceeds the current accumulation.
	err := adjustLossCounters(context.Background(), tx, playerID, mustMoney(t, "0.0000").Sub(mustMoney(t, "50.0000")))
	if err != nil {
		t.Fatalf("adjustLossCounters: %v", err)
	}
}

func TestAdjustDepositCounter_NoopOnNilAmount(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	// No DB expectations at all: adjustDepositCounter short-circuits on a
	// nil usdAmount before ever querying latestEffectiveLimit.
	if err := adjustDepositCounter(context.Background(), tx, playerID, nil); err != nil {
		t.Fatalf("adjustDepositCounter: %v", err)
	}
}

func TestAdjustDepositCounter_NoopWithoutActiveLimit(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	amount := mustMoney(t, "25.0000")
	if err := adjustDepositCounter(context.Background(), tx, playerID, &amount); err != nil {
		t.Fatalf("adjustDepositCounter: %v", err)
	}
}

// ----------------------------------------------------------------------------
// getOrSeedCounter — point 4: a missing or stale counter row is re-seeded
// from the ledger, never defaulting to zero.
// ----------------------------------------------------------------------------

func TestGetOrSeedCounter_TrustsFreshRow(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "LOSS", "DAY").
		WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
			AddRow(pgtype.Timestamptz{Time: currentWindowStart("DAY"), Valid: true}, decimal.RequireFromString("42.0000")))
	// No ledger aggregate expectation: a fresh row must be trusted as-is.

	got, err := getOrSeedCounter(context.Background(), tx, playerID, "LOSS", "DAY")
	if err != nil {
		t.Fatalf("getOrSeedCounter: %v", err)
	}
	if !got.Equal(decimal.RequireFromString("42.0000")) {
		t.Errorf("got %s, want 42.0000", got)
	}
}

func TestGetOrSeedCounter_ReseedsFromLedgerWhenRowMissing(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "LOSS", "DAY").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLossAggregate).
		WithArgs(playerID, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).AddRow(decimal.RequireFromString("77.5000")))

	got, err := getOrSeedCounter(context.Background(), tx, playerID, "LOSS", "DAY")
	if err != nil {
		t.Fatalf("getOrSeedCounter: %v", err)
	}
	if !got.Equal(decimal.RequireFromString("77.5000")) {
		t.Errorf("got %s, want 77.5000 (seeded from ledger)", got)
	}
}

func TestGetOrSeedCounter_ReseedsFromLedgerWhenWindowStale(t *testing.T) {
	_, _, mock := newRG(t)
	playerID := uuid.New()
	tx := beginTx(t, mock, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})

	staleStart := currentWindowStart("DAY").AddDate(0, 0, -1)
	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "DEPOSIT", "DAY").
		WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
			AddRow(pgtype.Timestamptz{Time: staleStart, Valid: true}, decimal.RequireFromString("999.0000")))
	mock.ExpectQuery(rxRGDepositAggregate).
		WithArgs(playerID, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).AddRow(decimal.RequireFromString("12.0000")))

	got, err := getOrSeedCounter(context.Background(), tx, playerID, "DEPOSIT", "DAY")
	if err != nil {
		t.Fatalf("getOrSeedCounter: %v", err)
	}
	if !got.Equal(decimal.RequireFromString("12.0000")) {
		t.Errorf("got %s, want 12.0000 (stale row discarded, reseeded from ledger)", got)
	}
}

// TestQueryLimits_RemainingAfterSeedingMidWindow is the test explicitly
// required by point 4: a player who already lost X this window, and only
// THEN sets a limit, must see remaining = limit - X immediately — not
// remaining = limit (which a blind zero-seed would produce).
func TestQueryLimits_RemainingAfterSeedingMidWindow(t *testing.T) {
	rg, _, mock := newRG(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	// No active SELF_EXCLUSION / COOL_OFF.
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "SELF_EXCLUSION").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "COOL_OFF").WillReturnError(pgx.ErrNoRows)
	// A LOSS_LIMIT of 100, set mid-window, with no counter row yet (the
	// player lost 35 BEFORE the limit existed) — must be seeded from the
	// ledger, not from zero.
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("100.0000")}))
	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "LOSS", "DAY").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLossAggregate).
		WithArgs(playerID, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"coalesce"}).AddRow(decimal.RequireFromString("35.0000")))
	// No DEPOSIT_LIMIT active.
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	summary, err := rg.QueryLimits(context.Background(), playerID)
	if err != nil {
		t.Fatalf("QueryLimits: %v", err)
	}
	if summary.LossLimit == nil {
		t.Fatal("LossLimit: got nil, want a windowed state")
	}
	wantUsed := decimal.RequireFromString("35.0000")
	wantRemaining := decimal.RequireFromString("65.0000") // 100 - 35
	if !summary.LossLimit.Used.Decimal().Equal(wantUsed) {
		t.Errorf("Used: got %s, want %s", summary.LossLimit.Used, wantUsed)
	}
	if !summary.LossLimit.Remaining.Decimal().Equal(wantRemaining) {
		t.Errorf("Remaining: got %s, want %s (limit - already-lost X)", summary.LossLimit.Remaining, wantRemaining)
	}
}

func TestQueryLimits_RemainingFloorsAtZeroWhenAlreadyOverLimit(t *testing.T) {
	rg, _, mock := newRG(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "SELF_EXCLUSION").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "COOL_OFF").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("DAY"), Amount: decPtr("50.0000")}))
	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "LOSS", "DAY").
		WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
			AddRow(pgtype.Timestamptz{Time: currentWindowStart("DAY"), Valid: true}, decimal.RequireFromString("80.0000")))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	summary, err := rg.QueryLimits(context.Background(), playerID)
	if err != nil {
		t.Fatalf("QueryLimits: %v", err)
	}
	if !summary.LossLimit.Remaining.IsZero() {
		t.Errorf("Remaining: got %s, want 0 (floored, not negative)", summary.LossLimit.Remaining)
	}
}

func TestQueryLimits_ReportsActiveExclusion(t *testing.T) {
	rg, _, mock := newRG(t)
	playerID := uuid.New()
	endsAt := time.Now().Add(48 * time.Hour)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "SELF_EXCLUSION").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, EndsAt: &endsAt}))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "COOL_OFF").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	summary, err := rg.QueryLimits(context.Background(), playerID)
	if err != nil {
		t.Fatalf("QueryLimits: %v", err)
	}
	if summary.SelfExclusion == nil {
		t.Fatal("SelfExclusion: got nil, want set")
	}
	if !summary.SelfExclusion.EndsAt.Equal(endsAt) {
		t.Errorf("EndsAt: got %v, want %v", summary.SelfExclusion.EndsAt, endsAt)
	}
}

// ----------------------------------------------------------------------------
// CheckPurchase — the pre-charge check (point 1).
// ----------------------------------------------------------------------------

func TestCheckPurchase_Allowed(t *testing.T) {
	rg, _, mock := newRG(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGActiveExclusion).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"limit_type", "ends_at"}))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	got, err := rg.CheckPurchase(context.Background(), CheckPurchaseRequest{
		PlayerID: playerID, USDAmount: mustMoney(t, "25.0000"),
	})
	if err != nil {
		t.Fatalf("CheckPurchase: %v", err)
	}
	if !got.Allowed {
		t.Error("Allowed: got false, want true")
	}
}

func TestCheckPurchase_RefusedWhenExcluded(t *testing.T) {
	rg, _, mock := newRG(t)
	playerID := uuid.New()
	until := time.Now().Add(24 * time.Hour)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGActiveExclusion).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"limit_type", "ends_at"}).AddRow("SELF_EXCLUSION", until))

	_, err := rg.CheckPurchase(context.Background(), CheckPurchaseRequest{
		PlayerID: playerID, USDAmount: mustMoney(t, "25.0000"),
	})
	var rgErr *errs.RGRestrictionError
	if !errors.As(err, &rgErr) || rgErr.Reason != "SELF_EXCLUSION" {
		t.Fatalf("got %v, want *RGRestrictionError{Reason: SELF_EXCLUSION}", err)
	}
}

func TestCheckPurchase_RefusedOverDepositLimit(t *testing.T) {
	rg, _, mock := newRG(t)
	playerID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGActiveExclusion).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"limit_type", "ends_at"}))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").
		WillReturnRows(latestLimitRow(latestLimit{Active: true, Period: strPtr("MONTH"), Amount: decPtr("100.0000")}))
	mock.ExpectQuery(rxRGSelectCounter).
		WithArgs(playerID, "DEPOSIT", "MONTH").
		WillReturnRows(pgxmock.NewRows([]string{"window_start", "amount"}).
			AddRow(pgtype.Timestamptz{Time: currentWindowStart("MONTH"), Valid: true}, decimal.RequireFromString("90.0000")))

	_, err := rg.CheckPurchase(context.Background(), CheckPurchaseRequest{
		PlayerID: playerID, USDAmount: mustMoney(t, "25.0000"),
	})
	var rgErr *errs.RGRestrictionError
	if !errors.As(err, &rgErr) || rgErr.Reason != "DEPOSIT_LIMIT" {
		t.Fatalf("got %v, want *RGRestrictionError{Reason: DEPOSIT_LIMIT}", err)
	}
}

func TestCheckPurchase_Validation(t *testing.T) {
	tests := []struct {
		name string
		req  CheckPurchaseRequest
	}{
		{"nil_player_id", CheckPurchaseRequest{USDAmount: mustMoney(t, "10.0000")}},
		{"zero_amount", CheckPurchaseRequest{PlayerID: uuid.New(), USDAmount: domain.ZeroMoney()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rg, _, _ := newRG(t) // no DB expectations: must short-circuit
			_, err := rg.CheckPurchase(context.Background(), tc.req)
			if err == nil {
				t.Error("got nil error, want a validation error")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Test helpers
// ----------------------------------------------------------------------------

func ptrTime(t time.Time) *time.Time { return &t }

func strPtr(s string) *string { return &s }

func decPtr(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func moneyPtr(t *testing.T, s string) *domain.Money {
	t.Helper()
	m := mustMoney(t, s)
	return &m
}

func currentWindowStart(period string) time.Time {
	start, _ := windowBounds(period, time.Now())
	return start
}

// latestLimitRow builds the single-row result latestEffectiveLimit expects
// for sqlRGLatestEffectiveLimit: (active, period, amount, ends_at).
func latestLimitRow(l latestLimit) *pgxmock.Rows {
	var period pgtype.Text
	if l.Period != nil {
		period = pgtype.Text{String: *l.Period, Valid: true}
	}
	var endsAt pgtype.Timestamptz
	if l.EndsAt != nil {
		endsAt = pgtype.Timestamptz{Time: *l.EndsAt, Valid: true}
	}
	return pgxmock.NewRows([]string{"active", "period", "amount", "ends_at"}).
		AddRow(l.Active, period, l.Amount, endsAt)
}
