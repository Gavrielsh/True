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

// Regex shortcuts mirroring internal/repository/kyc.go's SQL constants —
// duplicated here (rather than exported) so this file drives the REAL
// repository.KYCEngine through the full HTTP stack, not a stub.
const (
	rxKYCLockUser       = `SELECT id, kyc_verified_at FROM users WHERE external_id`
	rxKYCInsertDecision = `INSERT INTO kyc_decisions`
	rxKYCApplyVerified  = `UPDATE users`
)

// fullStackKYC builds the production router wired to a REAL repository.KYCEngine
// backed by pgxmock, so these tests exercise HMAC -> replay-guard -> handler ->
// repository -> SQL end to end, exactly as Task C4 requires.
func fullStackKYC(t *testing.T, mock pgxmock.PgxPoolIface, secret string) *http.ServeMux {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	kycEng := repository.NewKYC(mock, cache.NewRedis(client), discardLogger())

	r := NewRouter(Config{
		Engine:  &fakeEngine{},
		KYC:     kycEng,
		Redis:   client,
		Secrets: map[string]string{"OP1": secret},
		Logger:  discardLogger(),
	})
	mux := http.NewServeMux()
	mux.Handle("/", r)
	return mux
}

func newKYCMock(t *testing.T) pgxmock.PgxPoolIface {
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
// End-to-end: signed webhook delivery -> player status transition.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_KYCDecision_EndToEndAppliesStatus(t *testing.T) {
	t.Parallel()
	const secret = "kyc-secret"
	mock := newKYCMock(t)
	playerID := uuid.New()
	decisionID := uuid.New()
	decidedAt := mustRFC3339(t, "2026-01-01T00:00:00Z")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-e2e").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).AddRow(playerID, pgtype.Timestamptz{}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-e2e", "evt-e2e", "VERIFIED", decidedAt, true, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(decisionID))
	mock.ExpectExec(rxKYCApplyVerified).
		WithArgs(playerID, decidedAt).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	srv := fullStackKYC(t, mock, secret)
	body := `{"external_id":"ext-e2e","event_id":"evt-e2e","decision":"VERIFIED","decided_at":"2026-01-01T00:00:00Z"}`
	req := signedRequest(http.MethodPost, "/api/v1/kyc/decision", body, secret, "nonce-kyc-e2e")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp kycDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Result.Applied {
		t.Error("Applied: got false, want true — VERIFIED decision should trigger the status transition")
	}
	if resp.Result.PlayerID != playerID {
		t.Errorf("PlayerID: got %v want %v", resp.Result.PlayerID, playerID)
	}
}

// ----------------------------------------------------------------------------
// Out-of-order decidedAt race: a stale delivery arriving after a newer
// applied decision must be audited but never re-applied.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_KYCDecision_OutOfOrder_StaleNotApplied(t *testing.T) {
	t.Parallel()
	const secret = "kyc-secret"
	mock := newKYCMock(t)
	playerID := uuid.New()
	decisionID := uuid.New()
	newerBaseline := mustRFC3339(t, "2026-01-10T00:00:00Z")  // already-applied VERIFIED
	staleDecidedAt := mustRFC3339(t, "2026-01-01T00:00:00Z") // arrives late

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectQuery(rxKYCLockUser).WithArgs("ext-stale").
		WillReturnRows(pgxmock.NewRows([]string{"id", "kyc_verified_at"}).
			AddRow(playerID, pgtype.Timestamptz{Time: newerBaseline, Valid: true}))
	mock.ExpectQuery(rxKYCInsertDecision).
		WithArgs(playerID, "ext-stale", "evt-stale", "VERIFIED", staleDecidedAt, false, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(decisionID))
	// No ExpectExec(rxKYCApplyVerified): newKYCMock's t.Cleanup fails the test
	// if an UPDATE is issued for a superseded decision.
	mock.ExpectCommit()

	srv := fullStackKYC(t, mock, secret)
	body := `{"external_id":"ext-stale","event_id":"evt-stale","decision":"VERIFIED","decided_at":"2026-01-01T00:00:00Z"}`
	req := signedRequest(http.MethodPost, "/api/v1/kyc/decision", body, secret, "nonce-kyc-stale")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (audited, not an error); body=%s", w.Code, w.Body.String())
	}
	var resp kycDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.Applied {
		t.Error("Applied: got true, want false — a stale decision must never downgrade/re-apply a newer approval")
	}
}

// ----------------------------------------------------------------------------
// HMAC zero-trust perimeter: unsigned and tampered deliveries never reach
// the repository layer.
// ----------------------------------------------------------------------------

func TestRouter_FullStack_KYCDecision_UnsignedRejected(t *testing.T) {
	t.Parallel()
	mock := newKYCMock(t) // no expectations: the engine must never be reached
	srv := fullStackKYC(t, mock, "kyc-secret")

	body := `{"external_id":"ext-1","event_id":"evt-1","decision":"VERIFIED","decided_at":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/kyc/decision", strings.NewReader(body))
	// No signature/operator/timestamp/nonce headers.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", w.Code)
	}
}

func TestRouter_FullStack_KYCDecision_TamperedBodyRejected(t *testing.T) {
	t.Parallel()
	const secret = "kyc-secret"
	mock := newKYCMock(t) // no expectations: the engine must never be reached
	srv := fullStackKYC(t, mock, secret)

	// Signature computed over a DIFFERENT decision than the one actually sent
	// — a compromised-in-transit or maliciously modified delivery.
	signedBody := `{"external_id":"ext-1","event_id":"evt-1","decision":"VERIFIED","decided_at":"2026-01-01T00:00:00Z"}`
	tamperedBody := `{"external_id":"ext-1","event_id":"evt-1","decision":"REJECTED","decided_at":"2026-01-01T00:00:00Z"}`

	req := signedRequest(http.MethodPost, "/api/v1/kyc/decision", signedBody, secret, "nonce-kyc-tamper")
	req.Body = httptest.NewRequest(http.MethodPost, "/api/v1/kyc/decision", strings.NewReader(tamperedBody)).Body
	req.ContentLength = int64(len(tamperedBody))

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401 (tampered body must fail signature verification)", w.Code)
	}
}

func mustRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}
