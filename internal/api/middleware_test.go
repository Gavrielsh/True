package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// testTimestamp / testNonce are the replay headers every signed test request
// carries. They are part of the SIGNED material now (see canonicalPayload), so
// sign() and the request must agree on them EXACTLY.
//
// Both are FIXED constants, not derived from time.Now(): the HMAC middleware
// checks only well-formedness, never freshness (that is ReplayGuard's job, and
// it is not mounted in hmacRouter). Deriving the timestamp from the clock here
// would let sign() and the request header land on either side of a second
// boundary and fail intermittently — exactly the flake pattern already present
// in TestReplay_TimestampWindow.
const (
	testTimestamp = "1700000000"
	testNonce     = "test-nonce-0001"
)

// sign produces the canonical signature: HMAC(secret, timestamp.nonce.body).
func sign(secret, body string) string {
	return signAt(secret, testTimestamp, testNonce, body)
}

func signAt(secret, timestamp, nonce, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + nonce + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// signLegacy produces the PRE-AUDIT body-only signature. It exists only so the
// tests can prove that form is rejected — there is no code path that accepts it.
func signLegacy(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// ----------------------------------------------------------------------------
// HMAC middleware
// ----------------------------------------------------------------------------

// hmacRouter mounts a trivial echo handler behind the HMAC middleware. The
// echo handler reads c.Request.Body to PROVE the middleware restored it.
func hmacRouter(secrets map[string]string) *gin.Engine {
	r := gin.New()
	v := NewHMACVerifier(secrets)
	r.POST("/t", v.Middleware(), func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"err": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"body":     string(body),
			"operator": OperatorCodeFromContext(c.Request.Context()),
		})
	})
	return r
}

func TestHMAC_ValidSignature_PassesAndRestoresBody(t *testing.T) {
	t.Parallel()
	r := hmacRouter(map[string]string{"OP1": "topsecret"})
	body := `{"amount":"10.0000","nested":{"a":1}}`

	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
	req.Header.Set(HeaderOperatorCode, "OP1")
	req.Header.Set(HeaderSignature, sign("topsecret", body))
	req.Header.Set(HeaderTimestamp, testTimestamp)
	req.Header.Set(HeaderNonce, testNonce)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	// The downstream handler must see the EXACT original bytes.
	if !strings.Contains(w.Body.String(), `\"amount\":\"10.0000\"`) && !strings.Contains(w.Body.String(), body) {
		t.Errorf("body not restored for handler: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"operator":"OP1"`) {
		t.Errorf("operator not propagated: %s", w.Body.String())
	}
}

func TestHMAC_Rejections(t *testing.T) {
	t.Parallel()
	secret := "topsecret"
	body := `{"x":1}`
	good := sign(secret, body)

	cases := []struct {
		name     string
		operator string
		sig      string
	}{
		{"wrong_signature", "OP1", sign("WRONG", body)},
		{"missing_signature", "OP1", ""},
		{"unknown_operator", "GHOST", good},
		{"missing_operator", "", good},
		{"tampered_body_sig", "OP1", sign(secret, `{"x":2}`)}, // sig for different body
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := hmacRouter(map[string]string{"OP1": secret})
			req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
			if tc.operator != "" {
				req.Header.Set(HeaderOperatorCode, tc.operator)
			}
			if tc.sig != "" {
				req.Header.Set(HeaderSignature, tc.sig)
				req.Header.Set(HeaderTimestamp, testTimestamp)
				req.Header.Set(HeaderNonce, testNonce)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status: got %d want 401", w.Code)
			}
		})
	}
}

func TestHMAC_BodyTooLarge(t *testing.T) {
	t.Parallel()
	r := hmacRouter(map[string]string{"OP1": "s"})
	huge := strings.Repeat("a", maxBodyBytes+10)
	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(huge))
	req.Header.Set(HeaderOperatorCode, "OP1")
	req.Header.Set(HeaderSignature, sign("s", huge))
	req.Header.Set(HeaderTimestamp, testTimestamp)
	req.Header.Set(HeaderNonce, testNonce)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d want 413", w.Code)
	}
}

// mixHexCase upper-cases every other hex digit so the result is neither all
// lower nor all upper — exercising the case-insensitive decode path.
func mixHexCase(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i += 2 {
		if b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 32 // to upper
		}
	}
	return string(b)
}

// The raw-byte comparison must accept a correct signature regardless of hex
// case (the previous string compare spuriously rejected upper/mixed case).
func TestHMAC_CaseInsensitiveHexAccepted(t *testing.T) {
	t.Parallel()
	secret := "topsecret"
	body := `{"amount":"10.0000","nested":{"a":1}}`
	lower := sign(secret, body)

	cases := []struct{ name, sig string }{
		{"lowercase", lower},
		{"uppercase", strings.ToUpper(lower)},
		{"mixedcase", mixHexCase(lower)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := hmacRouter(map[string]string{"OP1": secret})
			req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
			req.Header.Set(HeaderOperatorCode, "OP1")
			req.Header.Set(HeaderSignature, tc.sig)
			req.Header.Set(HeaderTimestamp, testTimestamp)
			req.Header.Set(HeaderNonce, testNonce)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("case-insensitive hex must pass: got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// A signature that is not valid hex must fail closed (401), never 500.
func TestHMAC_NonHexSignatureRejected(t *testing.T) {
	t.Parallel()
	secret := "topsecret"
	body := `{"x":1}`
	r := hmacRouter(map[string]string{"OP1": secret})
	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
	req.Header.Set(HeaderOperatorCode, "OP1")
	req.Header.Set(HeaderSignature, "nothexZZ"+sign(secret, body)) // invalid hex prefix
	req.Header.Set(HeaderTimestamp, testTimestamp)
	req.Header.Set(HeaderNonce, testNonce)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("non-hex signature: got %d want 401", w.Code)
	}
}

// Pool-safety proof: many concurrent requests with DISTINCT bodies must all
// verify. A pooled buffer leaking bytes across requests would corrupt the hash
// input and surface as a 401, so all-200 means the sync.Pool is race-clean.
func TestHMAC_PooledBufferConcurrencySafe(t *testing.T) {
	t.Parallel()
	secret := "topsecret"
	r := hmacRouter(map[string]string{"OP1": secret})

	const n = 64
	var wg sync.WaitGroup
	fail := make(chan string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"i":%d,"pad":%q}`, i, strings.Repeat("x", i*11))
			req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
			req.Header.Set(HeaderOperatorCode, "OP1")
			req.Header.Set(HeaderSignature, sign(secret, body))
			req.Header.Set(HeaderTimestamp, testTimestamp)
			req.Header.Set(HeaderNonce, testNonce)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				fail <- fmt.Sprintf("i=%d: got %d (pool corruption ⇒ HMAC mismatch)", i, w.Code)
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for e := range fail {
		t.Error(e)
	}
}

// BenchmarkHMACMiddleware documents the hot-path allocation profile after
// pooling the body buffer. (httptest overhead dominates wall time; ReportAllocs
// is the signal of interest.)
func BenchmarkHMACMiddleware(b *testing.B) {
	secret := "topsecret"
	r := hmacRouter(map[string]string{"OP1": secret})
	body := `{"operator_transaction_id":"op-bench","amount":"10.0000","game_id":"G1"}`
	sig := sign(secret, body)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
		req.Header.Set(HeaderOperatorCode, "OP1")
		req.Header.Set(HeaderSignature, sig)
		req.Header.Set(HeaderTimestamp, testTimestamp)
		req.Header.Set(HeaderNonce, testNonce)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status %d", w.Code)
		}
	}
}

// ----------------------------------------------------------------------------
// Replay middleware
// ----------------------------------------------------------------------------

func replayRouter(t *testing.T) (*gin.Engine, *miniredis.Miniredis) {
	t.Helper()
	return replayRouterAt(t, time.Time{})
}

// replayRouterAt builds the replay-guard router with an optional FIXED clock.
//
// Pass a non-zero `at` when the test asserts a window BOUNDARY. With the real
// clock, a case built as "now - 299s" is only 299s old at construction time;
// by the time a parallel subtest actually executes, real time has advanced and
// the age can cross the 300s limit, flipping an expected 200 into a 401. That
// was a genuine intermittent failure in TestReplay_TimestampWindow (~40% of
// runs), not a bug in the guard itself. Freezing the guard's clock makes the
// boundary exact.
func replayRouterAt(t *testing.T, at time.Time) (*gin.Engine, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	r := gin.New()
	guard := NewReplayGuard(client)
	if !at.IsZero() {
		guard.now = func() time.Time { return at }
	}
	r.POST("/t", guard.Middleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r, mr
}

func replayReq(ts int64, nonce string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader("{}"))
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	if nonce != "" {
		req.Header.Set(HeaderNonce, nonce)
	}
	return req
}

func TestReplay_ValidPasses(t *testing.T) {
	t.Parallel()
	r, _ := replayRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, replayReq(time.Now().Unix(), "nonce-1"))
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestReplay_NonceReuseRejected(t *testing.T) {
	t.Parallel()
	r, _ := replayRouter(t)
	ts := time.Now().Unix()

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, replayReq(ts, "dup-nonce"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: got %d want 200", w1.Code)
	}
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, replayReq(ts, "dup-nonce"))
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("replay: got %d want 401", w2.Code)
	}
}

func TestReplay_TimestampWindow(t *testing.T) {
	t.Parallel()
	// FROZEN reference instant. Every case is expressed relative to it and the
	// guard is pinned to it, so boundary cases are exact regardless of how long
	// a parallel subtest waits to run.
	frozen := time.Unix(1_700_000_000, 0)
	now := frozen.Unix()

	cases := []struct {
		name   string
		ts     int64
		status int
	}{
		{"now", now, http.StatusOK},
		{"5min_minus_1s_ok", now - 299, http.StatusOK},
		{"exactly_5min_ok", now - 300, http.StatusOK},
		{"5min_plus_1s_stale", now - 301, http.StatusUnauthorized},
		{"6min_old_stale", now - 360, http.StatusUnauthorized},
		{"within_skew_future", now + 10, http.StatusOK},
		{"beyond_skew_future", now + 120, http.StatusUnauthorized},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := replayRouterAt(t, frozen)
			w := httptest.NewRecorder()
			// Unique nonce per case so only the timestamp is under test.
			r.ServeHTTP(w, replayReq(tc.ts, "ts-nonce-"+strconv.Itoa(i)))
			if w.Code != tc.status {
				t.Errorf("status: got %d want %d", w.Code, tc.status)
			}
		})
	}
}

func TestReplay_MissingHeaders(t *testing.T) {
	t.Parallel()
	r, _ := replayRouter(t)

	// Missing timestamp.
	noTS := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader("{}"))
	noTS.Header.Set(HeaderNonce, "n")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, noTS)
	if w1.Code != http.StatusBadRequest {
		t.Errorf("missing timestamp: got %d want 400", w1.Code)
	}

	// Non-numeric timestamp.
	badTS := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader("{}"))
	badTS.Header.Set(HeaderTimestamp, "not-a-number")
	badTS.Header.Set(HeaderNonce, "n")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, badTS)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("bad timestamp: got %d want 400", w2.Code)
	}

	// Missing nonce.
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, replayReq(time.Now().Unix(), ""))
	if w3.Code != http.StatusBadRequest {
		t.Errorf("missing nonce: got %d want 400", w3.Code)
	}
}

func TestReplay_FailClosedOnRedisDown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	r := gin.New()
	r.POST("/t", NewReplayGuard(client).Middleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	mr.Close() // Redis outage
	w := httptest.NewRecorder()
	r.ServeHTTP(w, replayReq(time.Now().Unix(), "n"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("FAIL CLOSED: got %d want 500", w.Code)
	}
}

// ----------------------------------------------------------------------------
// Recovery middleware
// ----------------------------------------------------------------------------

func TestRecovery_TurnsPanicInto500(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.Use(Recovery(discardLogger()))
	r.GET("/boom", func(_ *gin.Context) {
		panic("kaboom")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d want 500", w.Code)
	}
}

// ----------------------------------------------------------------------------
// Canonical-signature binding (audit fix: replay protection was decorative)
// ----------------------------------------------------------------------------

// TestHMAC_LegacyBodyOnlySignatureRejected is the core regression guard.
//
// Before the fix the signature covered ONLY the body, so an attacker who
// captured one valid request could replay it forever: reuse the body and its
// signature, attach a fresh timestamp and a brand-new nonce, and every check
// passed — the nonce could not help because the ATTACKER chose it.
//
// The body-only form is now rejected unconditionally. There is no verifier
// option, router config field, or environment variable that admits it — the
// fallback was deleted, not merely defaulted off.
func TestHMAC_LegacyBodyOnlySignatureRejected(t *testing.T) {
	t.Parallel()
	const secret = "topsecret"
	const body = `{"x":1}`

	cases := []struct {
		name      string
		signature string
		timestamp string
		nonce     string
	}{
		{
			// The captured request replayed verbatim.
			name:      "body-only signature",
			signature: signLegacy(secret, body),
			timestamp: testTimestamp,
			nonce:     testNonce,
		},
		{
			// The signature is hex-DECODED before comparison, so casing must
			// not become a laundering route around the rejection.
			name:      "body-only signature in uppercase hex",
			signature: strings.ToUpper(signLegacy(secret, body)),
			timestamp: testTimestamp,
			nonce:     testNonce,
		},
		{
			// The actual attack: a captured body-only signature re-presented
			// with attacker-chosen freshness headers.
			name:      "body-only signature with attacker-chosen timestamp and nonce",
			signature: signLegacy(secret, body),
			timestamp: "1700009999",
			nonce:     "attacker-nonce",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := hmacRouter(map[string]string{"OP1": secret})
			req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
			req.Header.Set(HeaderOperatorCode, "OP1")
			req.Header.Set(HeaderSignature, tc.signature)
			req.Header.Set(HeaderTimestamp, tc.timestamp)
			req.Header.Set(HeaderNonce, tc.nonce)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("body-only signature must be rejected: got %d, body=%s",
					w.Code, w.Body.String())
			}
		})
	}
}

// TestHMAC_SignatureIsBoundToTimestampAndNonce proves the replay headers are
// genuinely covered: a signature valid for one (timestamp, nonce) pair must not
// validate when either is swapped — which is exactly the attacker's move.
func TestHMAC_SignatureIsBoundToTimestampAndNonce(t *testing.T) {
	t.Parallel()
	const secret = "topsecret"
	body := `{"x":1}`
	captured := signAt(secret, testTimestamp, testNonce, body)

	cases := []struct {
		name      string
		timestamp string
		nonce     string
	}{
		{"fresh timestamp, same nonce", "1700009999", testNonce},
		{"same timestamp, fresh nonce", testTimestamp, "attacker-nonce"},
		{"both refreshed", "1700009999", "attacker-nonce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := hmacRouter(map[string]string{"OP1": secret})
			req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
			req.Header.Set(HeaderOperatorCode, "OP1")
			req.Header.Set(HeaderSignature, captured) // replayed signature
			req.Header.Set(HeaderTimestamp, tc.timestamp)
			req.Header.Set(HeaderNonce, tc.nonce)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("replayed signature with %s must be rejected: got %d", tc.name, w.Code)
			}
		})
	}
}

// TestHMAC_MalformedReplayHeadersRejected covers the canonical-string
// ambiguity guard: a nonce containing the '.' separator could otherwise let an
// attacker shift bytes between fields while keeping the same digest.
func TestHMAC_MalformedReplayHeadersRejected(t *testing.T) {
	t.Parallel()
	const secret = "topsecret"
	body := `{"x":1}`

	cases := []struct{ name, timestamp, nonce string }{
		{"missing timestamp", "", testNonce},
		{"missing nonce", testTimestamp, ""},
		{"non-numeric timestamp", "not-a-number", testNonce},
		{"nonce contains separator", testTimestamp, "aaa.bbb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := hmacRouter(map[string]string{"OP1": secret})
			req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
			req.Header.Set(HeaderOperatorCode, "OP1")
			req.Header.Set(HeaderSignature, signAt(secret, tc.timestamp, tc.nonce, body))
			if tc.timestamp != "" {
				req.Header.Set(HeaderTimestamp, tc.timestamp)
			}
			if tc.nonce != "" {
				req.Header.Set(HeaderNonce, tc.nonce)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s must be rejected: got %d", tc.name, w.Code)
			}
		})
	}
}

// TestHMAC_BodyHashExposedToHandlers confirms the verified body hash reaches
// the handler, which is what binds an idempotency key to its request.
func TestHMAC_BodyHashExposedToHandlers(t *testing.T) {
	t.Parallel()
	const secret = "topsecret"
	body := `{"x":1}`

	var seen string
	r := gin.New()
	v := NewHMACVerifier(map[string]string{"OP1": secret})
	r.POST("/t", v.Middleware(), func(c *gin.Context) {
		seen = BodyHashFromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
	req.Header.Set(HeaderOperatorCode, "OP1")
	req.Header.Set(HeaderSignature, sign(secret, body))
	req.Header.Set(HeaderTimestamp, testTimestamp)
	req.Header.Set(HeaderNonce, testNonce)
	r.ServeHTTP(httptest.NewRecorder(), req)

	want := sha256.Sum256([]byte(body))
	if seen != hex.EncodeToString(want[:]) {
		t.Fatalf("body hash: got %q want %q", seen, hex.EncodeToString(want[:]))
	}
}
