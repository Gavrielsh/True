package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/repository"
)

// Regex shortcuts mirroring internal/repository/rg.go's SQL constants —
// duplicated here (rather than exported) so this file drives the REAL
// repository.ResponsibleGamingEngine through the full HTTP stack, not a
// stub, exactly like kyc_router_test.go.
const (
	rxRGLockPlayer           = `SELECT id FROM users WHERE id`
	rxRGInsertLimit          = `INSERT INTO player_limits`
	rxRGActiveExclusion      = `SELECT limit_type, ends_at FROM latest`
	rxRGLatestEffectiveLimit = `SELECT active, period, amount, ends_at`
)

// fullStackRG builds the production router wired to a REAL
// repository.ResponsibleGamingEngine backed by pgxmock, so these tests
// exercise HMAC -> replay-guard -> handler -> repository -> SQL end to end.
func fullStackRG(t *testing.T, mock pgxmock.PgxPoolIface, secret string) *http.ServeMux {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	rgEng := repository.NewResponsibleGaming(mock, cache.NewRedis(client), discardLogger())

	r := NewRouter(Config{
		Engine:            &fakeEngine{},
		ResponsibleGaming: rgEng,
		Redis:             client,
		Secrets:           map[string]string{"OP1": secret},
		Logger:            discardLogger(),
	})
	mux := http.NewServeMux()
	mux.Handle("/", r)
	return mux
}

func newRGMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(func() {
		mock.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})
	return mock
}

// ----------------------------------------------------------------------------
// End-to-end: signed SetLimit request -> a new, immediate LOSS_LIMIT row.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_SetLimit_EndToEnd(t *testing.T) {
	t.Parallel()
	const secret = "rg-secret"
	mock := newRGMock(t)
	playerID := uuid.New()
	limitID := uuid.New()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxRGLockPlayer).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(playerID))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGInsertLimit).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(limitID))
	mock.ExpectCommit()

	srv := fullStackRG(t, mock, secret)
	body := `{"player_id":"` + playerID.String() + `","limit_type":"LOSS_LIMIT","active":true,` +
		`"period":"DAY","amount":"100.0000","event_id":"evt-router-1"}`
	req := signedRequest(http.MethodPost, "/api/v1/player/limits", body, secret, "nonce-rg-set-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp setLimitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.LimitType != "LOSS_LIMIT" || !resp.Result.Active {
		t.Errorf("got %+v, want an active LOSS_LIMIT", resp.Result)
	}
	if resp.Result.EffectiveAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("EffectiveAt: got %v, want immediate (a brand-new limit)", resp.Result.EffectiveAt)
	}
}

// ----------------------------------------------------------------------------
// End-to-end: signed QueryLimits request -> a reported active exclusion.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_QueryLimits_EndToEnd(t *testing.T) {
	t.Parallel()
	const secret = "rg-secret"
	mock := newRGMock(t)
	playerID := uuid.New()
	endsAt := time.Now().Add(48 * time.Hour)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "SELF_EXCLUSION").
		WillReturnRows(pgxmock.NewRows([]string{"active", "period", "amount", "ends_at"}).
			AddRow(true, pgtype.Text{}, nil, pgtype.Timestamptz{Time: endsAt, Valid: true}))
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "COOL_OFF").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "LOSS_LIMIT").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(rxRGLatestEffectiveLimit).
		WithArgs(playerID, "DEPOSIT_LIMIT").WillReturnError(pgx.ErrNoRows)

	srv := fullStackRG(t, mock, secret)
	body := `{"player_id":"` + playerID.String() + `"}`
	req := signedRequest(http.MethodPost, "/api/v1/player/limits/query", body, secret, "nonce-rg-query-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp queryLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.SelfExclusion == nil {
		t.Fatal("SelfExclusion: got nil, want set")
	}
}

// ----------------------------------------------------------------------------
// End-to-end: signed CheckPurchase request refused by an active exclusion —
// the pre-charge check (point 1) surfaces RG_RESTRICTED with a reason.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_CheckPurchase_RefusedWhenExcluded(t *testing.T) {
	t.Parallel()
	const secret = "rg-secret"
	mock := newRGMock(t)
	playerID := uuid.New()
	until := time.Now().Add(24 * time.Hour)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(rxRGActiveExclusion).WithArgs(playerID).
		WillReturnRows(pgxmock.NewRows([]string{"limit_type", "ends_at"}).AddRow("COOL_OFF", until))

	srv := fullStackRG(t, mock, secret)
	body := `{"player_id":"` + playerID.String() + `","usd_amount":"25.0000"}`
	req := signedRequest(http.MethodPost, "/api/v1/player/limits/check-purchase", body, secret, "nonce-rg-check-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", w.Code, w.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != "RG_RESTRICTED" {
		t.Errorf("Code: got %q, want RG_RESTRICTED", resp.Code)
	}
	if resp.Reason != "COOL_OFF" {
		t.Errorf("Reason: got %q, want COOL_OFF", resp.Reason)
	}
}

// ----------------------------------------------------------------------------
// HMAC zero-trust perimeter: an unsigned delivery never reaches the
// repository layer.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_SetLimit_UnsignedRejected(t *testing.T) {
	t.Parallel()
	mock := newRGMock(t) // no expectations: the engine must never be reached
	srv := fullStackRG(t, mock, "rg-secret")

	body := `{"player_id":"` + uuid.New().String() + `","limit_type":"LOSS_LIMIT","active":true,` +
		`"period":"DAY","amount":"100.0000","event_id":"evt-unsigned"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/player/limits", strings.NewReader(body))
	// No signature/operator/timestamp/nonce headers.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", w.Code)
	}
}
