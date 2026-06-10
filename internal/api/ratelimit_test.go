package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/True/internal/metrics"
	"github.com/Gavrielsh/True/internal/repository"
)

// testClock is a manually-advanced clock so window expiry is deterministic —
// no sleeps, no wall-time flake.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLimiter(t *testing.T, opts ...RateLimitOption) (*RateLimiter, *testClock, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	clk := &testClock{t: time.Now()}
	rl := NewRateLimiter(client, discardLogger(), opts...)
	rl.now = clk.Now
	return rl, clk, mr
}

// Exceed then recover: the 4th request in the window is denied with a
// positive retry hint; advancing past the window admits again.
func TestRateLimiter_ExceedAndRecover(t *testing.T) {
	t.Parallel()
	rl, clk, _ := newTestLimiter(t, WithPlayerRPS(3))
	ctx := context.Background()
	playerID := uuid.New()

	for i := range 3 {
		admitted, _ := rl.AllowPlayer(ctx, "bet", playerID)
		if !admitted {
			t.Fatalf("request %d: denied, want admitted", i+1)
		}
		clk.Advance(50 * time.Millisecond) // 3 requests inside 150ms
	}

	admitted, retryAfter := rl.AllowPlayer(ctx, "bet", playerID)
	if admitted {
		t.Fatal("4th request in window: admitted, want denied")
	}
	if retryAfter <= 0 || retryAfter > rateLimitWindow {
		t.Errorf("retry_after: got %v want (0, %v]", retryAfter, rateLimitWindow)
	}

	// Walk past the window: the oldest entries expire and we are admitted.
	clk.Advance(rateLimitWindow)
	admitted, _ = rl.AllowPlayer(ctx, "bet", playerID)
	if !admitted {
		t.Error("after window elapsed: denied, want admitted (recovery)")
	}
}

// Separate keys are independent: throttling one player/endpoint must not
// affect another.
func TestRateLimiter_KeysAreIndependent(t *testing.T) {
	t.Parallel()
	rl, _, _ := newTestLimiter(t, WithPlayerRPS(1))
	ctx := context.Background()
	playerA, playerB := uuid.New(), uuid.New()

	if a, _ := rl.AllowPlayer(ctx, "bet", playerA); !a {
		t.Fatal("playerA first request denied")
	}
	if a, _ := rl.AllowPlayer(ctx, "bet", playerA); a {
		t.Fatal("playerA second request admitted, want denied")
	}
	if a, _ := rl.AllowPlayer(ctx, "bet", playerB); !a {
		t.Error("playerB must not be throttled by playerA's window")
	}
	if a, _ := rl.AllowPlayer(ctx, "win", playerA); !a {
		t.Error("win endpoint must not be throttled by bet's window")
	}
}

// Redis down → FAIL OPEN: every request is admitted (availability over
// strict limiting; the WARN is the operator's signal).
func TestRateLimiter_RedisDownFailsOpen(t *testing.T) {
	t.Parallel()
	rl, _, mr := newTestLimiter(t, WithPlayerRPS(1))
	ctx := context.Background()
	playerID := uuid.New()

	if a, _ := rl.AllowPlayer(ctx, "bet", playerID); !a {
		t.Fatal("first request denied")
	}
	mr.Close() // limiter control plane goes away

	for i := range 5 {
		admitted, retryAfter := rl.AllowPlayer(ctx, "bet", playerID)
		if !admitted {
			t.Fatalf("request %d with Redis down: denied, want fail-open admit", i+1)
		}
		if retryAfter != 0 {
			t.Errorf("fail-open retry_after: got %v want 0", retryAfter)
		}
	}
}

// Operator middleware: 429 envelope with retry_after_ms, scope counter
// incremented, and replay guard never reached (mounted before it).
func TestRateLimiter_OperatorMiddleware(t *testing.T) {
	t.Parallel()
	rl, _, _ := newTestLimiter(t, WithOperatorRPS(2))

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// Simulate the HMAC middleware having stashed the verified operator.
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(withOperator(c.Request.Context(), "OP1"))
		c.Next()
	})
	r.Use(rl.OperatorMiddleware())
	r.POST("/bet", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	before := testutil.ToFloat64(metrics.RateLimitedRequests.WithLabelValues(rlScopeOperator))
	codes := make([]int, 0, 3)
	for range 3 {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/bet", nil))
		codes = append(codes, w.Code)
		if w.Code == http.StatusTooManyRequests {
			var resp struct {
				Code         string `json:"code"`
				RetryAfterMS int64  `json:"retry_after_ms"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode 429: %v", err)
			}
			if resp.Code != "RATE_LIMITED" {
				t.Errorf("code: got %q want RATE_LIMITED", resp.Code)
			}
			if resp.RetryAfterMS < 1 {
				t.Errorf("retry_after_ms: got %d want >= 1", resp.RetryAfterMS)
			}
		}
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || codes[2] != http.StatusTooManyRequests {
		t.Errorf("codes: got %v want [200 200 429]", codes)
	}
	if got := testutil.ToFloat64(metrics.RateLimitedRequests.WithLabelValues(rlScopeOperator)); got != before+1 {
		t.Errorf("operator counter: got %v want %v", got, before+1)
	}
}

// Player-scope inside the handler: with a 1 rps player window the second bet
// for the SAME player is 429 (engine untouched), while a different player
// still passes. Wired through the full production router so the check is
// proven to run after HMAC verification.
func TestRateLimiter_PlayerScopeInHandler(t *testing.T) {
	t.Parallel()
	const secret = "s"
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	engineCalls := 0
	eng := &fakeEngine{bet: func(_ context.Context, req repository.BetRequest) (repository.TxResult, error) {
		engineCalls++
		return repository.TxResult{PlayerID: req.PlayerID, Status: repository.StatusProcessed,
			PostBalances: repository.BalanceSummary{GC: mzero(t), SCUnplayed: mzero(t), SCRedeemable: mzero(t)}}, nil
	}}
	// Operator window stays wide so only the player scope can trigger.
	rl := NewRateLimiter(client, discardLogger(), WithOperatorRPS(1000), WithPlayerRPS(1))
	router := NewRouter(Config{
		Engine:      eng,
		Redis:       client,
		Secrets:     map[string]string{"OP1": secret},
		RateLimiter: rl,
		Logger:      discardLogger(),
	})

	playerA, playerB := uuid.New(), uuid.New()
	sendBet := func(player uuid.UUID, opTx, nonce string) *httptest.ResponseRecorder {
		body := `{"operator_transaction_id":"` + opTx + `","player_id":"` + player.String() + `","currency":"GC","amount":"1.0000"}`
		w := httptest.NewRecorder()
		router.ServeHTTP(w, signedRequest(http.MethodPost, "/api/v1/bet", body, secret, nonce))
		return w
	}

	before := testutil.ToFloat64(metrics.RateLimitedRequests.WithLabelValues(rlScopePlayer))

	if w := sendBet(playerA, "rlp-1", "rlp-n1"); w.Code != http.StatusOK {
		t.Fatalf("first bet: got %d; body=%s", w.Code, w.Body.String())
	}
	w := sendBet(playerA, "rlp-2", "rlp-n2")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second bet same player: got %d want 429; body=%s", w.Code, w.Body.String())
	}
	if engineCalls != 1 {
		t.Errorf("engine calls: got %d want 1 (throttled request must not reach engine)", engineCalls)
	}
	if w := sendBet(playerB, "rlp-3", "rlp-n3"); w.Code != http.StatusOK {
		t.Errorf("other player: got %d want 200 (windows are per-player)", w.Code)
	}
	if got := testutil.ToFloat64(metrics.RateLimitedRequests.WithLabelValues(rlScopePlayer)); got != before+1 {
		t.Errorf("player counter: got %v want %v", got, before+1)
	}
}
