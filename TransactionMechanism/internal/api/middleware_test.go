package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func sign(secret, body string) string {
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
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d want 413", w.Code)
	}
}

// ----------------------------------------------------------------------------
// Replay middleware
// ----------------------------------------------------------------------------

func replayRouter(t *testing.T) (*gin.Engine, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	r := gin.New()
	guard := NewReplayGuard(client)
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
	now := time.Now().Unix()
	cases := []struct {
		name   string
		ts     int64
		status int
	}{
		{"now", now, http.StatusOK},
		{"5min_minus_1s_ok", now - 299, http.StatusOK},
		{"6min_old_stale", now - 360, http.StatusUnauthorized},
		{"within_skew_future", now + 10, http.StatusOK},
		{"beyond_skew_future", now + 120, http.StatusUnauthorized},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := replayRouter(t)
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
