package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/metrics"
	"github.com/Gavrielsh/True/internal/repository"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fullStack builds the production router (Recovery → Telemetry → HMAC →
// Replay → handler) wired to a fake engine and a real miniredis nonce store.
func fullStack(t *testing.T, eng repository.Engine, secret string) *http.ServeMux {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	r := NewRouter(Config{
		Engine:  eng,
		Redis:   client,
		Secrets: map[string]string{"OP1": secret},
		Logger:  discardLogger(),
	})
	mux := http.NewServeMux()
	mux.Handle("/", r)
	return mux
}

// signedRequest builds a fully-authenticated request: HMAC over the CANONICAL
// STRING (timestamp.nonce.body), plus the matching timestamp and nonce headers.
//
// The timestamp is captured ONCE and used for both the signature and the
// header. Computing it twice would let the two land on either side of a second
// boundary, producing a signature over a timestamp the request never carried —
// an intermittent 401 that looks like a security bug but is a test bug.
//
// ReplayGuard IS mounted in this router (unlike hmacRouter), so the timestamp
// must be genuinely fresh rather than the fixed constant used in the
// HMAC-only tests.
func signedRequest(method, path, body, secret, nonce string) *http.Request {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(HeaderOperatorCode, "OP1")
	req.Header.Set(HeaderSignature, signAt(secret, timestamp, nonce, body))
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)
	return req
}

func TestRouter_FullStack_SignedBetSucceeds(t *testing.T) {
	t.Parallel()
	const secret = "shared-secret"
	playerID := uuid.New()
	eng := &fakeEngine{
		bet: func(_ context.Context, req repository.BetRequest) (repository.TxResult, error) {
			return repository.TxResult{
				PlayerID: req.PlayerID, TransactionType: "BET", Amount: req.Amount,
				PostBalances: repository.BalanceSummary{GC: mzero(t), SCUnplayed: mzero(t), SCRedeemable: m(t, "90.0000")},
				Status:       repository.StatusProcessed,
			}, nil
		},
	}
	srv := fullStack(t, eng, secret)

	body := `{"operator_transaction_id":"op-1","player_id":"` + playerID.String() + `","currency":"SC","amount":"10.0000"}`
	req := signedRequest(http.MethodPost, "/api/v1/bet", body, secret, "nonce-bet-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp successResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != errs.CodeOK {
		t.Errorf("code: got %s want OK", resp.Code)
	}
	if w.Header().Get("X-Trace-Id") == "" {
		t.Error("X-Trace-Id response header missing")
	}
}

func TestRouter_FullStack_UnsignedRejected(t *testing.T) {
	t.Parallel()
	eng := &fakeEngine{bet: func(_ context.Context, _ repository.BetRequest) (repository.TxResult, error) {
		t.Error("engine must not be reached without a valid signature")
		return repository.TxResult{}, nil
	}}
	srv := fullStack(t, eng, "secret")

	body := `{"operator_transaction_id":"op","player_id":"` + uuid.NewString() + `","currency":"SC","amount":"1.0000"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bet", strings.NewReader(body))
	// No signature/operator headers.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", w.Code)
	}
}

func TestRouter_FullStack_ReplayRejectedOnSecondCall(t *testing.T) {
	t.Parallel()
	const secret = "s"
	eng := &fakeEngine{bet: func(_ context.Context, req repository.BetRequest) (repository.TxResult, error) {
		return repository.TxResult{PlayerID: req.PlayerID, Status: repository.StatusProcessed,
			PostBalances: repository.BalanceSummary{GC: mzero(t), SCUnplayed: mzero(t), SCRedeemable: mzero(t)}}, nil
	}}
	srv := fullStack(t, eng, secret)
	body := `{"operator_transaction_id":"op","player_id":"` + uuid.NewString() + `","currency":"GC","amount":"1.0000"}`

	// First signed call with nonce "N" succeeds.
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, signedRequest(http.MethodPost, "/api/v1/bet", body, secret, "N"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first: got %d want 200; body=%s", w1.Code, w1.Body.String())
	}
	// Replaying the exact same nonce is rejected by the replay guard.
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, signedRequest(http.MethodPost, "/api/v1/bet", body, secret, "N"))
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("replay: got %d want 401", w2.Code)
	}
}

// Healthz coverage lives in health_test.go (deep probe: pg + redis).

// /metrics must be reachable WITHOUT HMAC headers (the scraper has none) and
// must serve the engine instruments from the default registry.
func TestRouter_Metrics(t *testing.T) {
	t.Parallel()
	// The histogram vec only appears in scrape output once a labelled series
	// has been observed; record one sample so the scrape proves end-to-end
	// exposure under the realigned name.
	metrics.ObserveDBLockDuration(metrics.OpBet, time.Now())

	srv := fullStack(t, &fakeEngine{}, "s")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The plain counter is exported even at zero, so its presence proves the
	// engine instruments are registered where promhttp serves from.
	if !strings.Contains(body, "engine_ghost_spins_recovered_total") {
		t.Errorf("metrics body missing engine_ghost_spins_recovered_total:\n%s", body)
	}
	if !strings.Contains(body, "engine_db_lock_duration_seconds") {
		t.Errorf("metrics body missing engine_db_lock_duration_seconds:\n%s", body)
	}
}

// small money helpers local to this file
func m(t *testing.T, s string) domain.Money { return mustMoney(t, s) }
func mzero(t *testing.T) domain.Money       { return mustMoney(t, "0.0000") }

// TestRouter_ComplianceRouteIsNotGeoFenced pins a deliberate exception in the
// middleware chain.
//
// POST /api/v1/player/status carries the full zero-trust stack — HMAC, operator
// rate limiting, replay protection — but is mounted OUTSIDE the jurisdiction
// fence, while every money route stays behind it. The reason is that a player
// must always be able to exclude themselves and an operator must always be able
// to close an account: refusing a self-exclusion because the request appeared to
// originate in a prohibited state would use a player-protection control to deny
// a player protection.
//
// It is asserted rather than left to a comment because the exception is
// invisible at the call site — a future refactor that moves the group below the
// fence, or replaces it with a plain v1.POST, would silently re-fence the route
// and nothing else would notice.
func TestRouter_ComplianceRouteIsNotGeoFenced(t *testing.T) {
	t.Parallel()

	// Same low-entropy placeholder the other router tests use; a realistic-
	// looking literal here trips gosec's hardcoded-credential detector for no
	// benefit.
	const secret = "shared-secret"
	const (
		blockedIP  = "203.0.113.7"
		remoteAddr = blockedIP + ":51000"
	)

	resolver := &fakeResolver{regions: map[string]string{blockedIP: "US-WA"}} // blocked jurisdiction
	gf := newFence(t, resolver, nil, nil)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	casino := &fakeCasino{
		redeem: func(context.Context, repository.RedeemRequest) (repository.TxResult, error) {
			return repository.TxResult{}, nil
		},
		status: func(_ context.Context, req repository.StatusTransitionRequest) (repository.StatusTransitionResult, error) {
			return repository.StatusTransitionResult{
				PlayerID: req.PlayerID, TransitionID: uuid.New(),
				FromStatus: repository.StatusActive, ToStatus: req.ToStatus,
			}, nil
		},
	}

	r := NewRouter(Config{
		Casino:   casino,
		Redis:    client,
		Secrets:  map[string]string{"OP1": secret},
		GeoFence: gf,
		Logger:   discardLogger(),
	})

	playerID := uuid.New().String()

	// Control: a money route from the same blocked address IS refused. Without
	// this the test could pass against a fence that does nothing at all.
	t.Run("money route is fenced", func(t *testing.T) {
		body := `{"operator_transaction_id":"geo-red-1","player_id":"` + playerID + `","amount":"5.0000"}`
		req := signedRequest(http.MethodPost, "/api/v1/store/redeem", body, secret, uuid.NewString())
		req.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("store/redeem from a blocked region: got %d want 403; body=%s", w.Code, w.Body.String())
		}
		var body2 errorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body2); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body2.Code != errs.CodeGeoBlocked {
			t.Errorf("code: got %s want %s", body2.Code, errs.CodeGeoBlocked)
		}
	})

	// The exception: the same address may still self-exclude.
	t.Run("self-exclusion is not fenced", func(t *testing.T) {
		body := `{"player_id":"` + playerID + `","to_status":"SELF_EXCLUDED","actor_type":"PLAYER",` +
			`"reason":"player requested an exclusion","self_exclusion_days":180}`
		req := signedRequest(http.MethodPost, "/api/v1/player/status", body, secret, uuid.NewString())
		req.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code == http.StatusForbidden {
			t.Fatalf("a self-exclusion was geo-blocked; a protective action must never be "+
				"refused on jurisdiction. body=%s", w.Body.String())
		}
		if w.Code != http.StatusOK {
			t.Fatalf("player/status: got %d want 200; body=%s", w.Code, w.Body.String())
		}
	})

	// And it is still a signed route: dropping the fence must not have dropped
	// the perimeter with it.
	t.Run("but it still requires a valid signature", func(t *testing.T) {
		body := `{"player_id":"` + playerID + `","to_status":"SUSPENDED","actor_type":"OPERATOR","reason":"unsigned"}`
		req := signedRequest(http.MethodPost, "/api/v1/player/status", body, "the-wrong-secret", uuid.NewString())
		req.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("an unsigned compliance request: got %d want 401", w.Code)
		}
	})
}
