package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Gavrielsh/True/internal/metrics"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// fakeResolver answers from a static IP→region table — no MaxMind file needed.
type fakeResolver struct {
	regions map[string]string // ip string → region
	err     error
	closed  bool
}

func (f *fakeResolver) Region(ip net.IP) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.regions[ip.String()], nil
}

func (f *fakeResolver) Close() error {
	f.closed = true
	return nil
}

// geoRouter mounts ONLY the fence in front of a trivial 200 handler so each
// case isolates the middleware decision.
func geoRouter(gf *GeoFence) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gf.Middleware())
	r.GET("/probe", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func TestGeoFence_Middleware(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{
		"203.0.113.7":  "US-WA", // blocked
		"203.0.113.8":  "US-NJ", // allowed
		"198.51.100.1": "US-UT", // blocked (via XFF)
	}}
	blocked := []string{"US-WA", "US-UT", "US-ID", "US-MI", "US-NV"}

	cases := []struct {
		name       string
		xff        string
		remoteAddr string
		wantStatus int
	}{
		{"blocked_via_remote_addr", "", "203.0.113.7:4444", http.StatusForbidden},
		{"allowed_via_remote_addr", "", "203.0.113.8:4444", http.StatusOK},
		{"blocked_via_xff_first_ip", "198.51.100.1, 10.0.0.1, 10.0.0.2", "10.9.9.9:1", http.StatusForbidden},
		{"xff_first_ip_wins_over_remote", "203.0.113.8", "203.0.113.7:4444", http.StatusOK},
		{"unknown_region_fails_open", "", "192.0.2.200:9", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gf := NewGeoFenceWithResolver(resolver, blocked, discardLogger())
			r := geoRouter(gf)

			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden {
				var resp errorResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp.Code != errs.CodeGeoBlocked {
					t.Errorf("code: got %s want GEO_BLOCKED", resp.Code)
				}
				if resp.Message != "service not available in your region" {
					t.Errorf("message: got %q", resp.Message)
				}
			}
		})
	}
}

// Resolver failures must FAIL OPEN (this fence is a backstop behind the
// gateway) — never reject traffic because GeoLite2 hiccupped.
func TestGeoFence_ResolverErrorFailsOpen(t *testing.T) {
	t.Parallel()
	gf := NewGeoFenceWithResolver(
		&fakeResolver{err: errors.New("corrupt mmdb segment")},
		[]string{"US-WA"}, discardLogger())
	r := geoRouter(gf)

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = "203.0.113.7:1"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d want 200 (fail open)", w.Code)
	}
}

// Empty GEOIP_DB_PATH yields a disabled fence: Enabled() is false and the
// middleware passes everything through.
func TestGeoFence_DisabledDevMode(t *testing.T) {
	t.Parallel()
	gf, err := NewGeoFence("", []string{"US-WA"}, discardLogger())
	if err != nil {
		t.Fatalf("NewGeoFence: %v", err)
	}
	if gf.Enabled() {
		t.Error("fence with empty db path must be disabled")
	}
	r := geoRouter(gf)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = "203.0.113.7:1"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d want 200 (disabled)", w.Code)
	}
}

// A block must increment engine_geo_blocked_requests_total{region=...}.
func TestGeoFence_BlockIncrementsCounter(t *testing.T) {
	t.Parallel()
	const region = "US-ID" // distinct from other tests to isolate the series
	resolver := &fakeResolver{regions: map[string]string{"203.0.113.99": region}}
	gf := NewGeoFenceWithResolver(resolver, []string{region}, discardLogger())
	r := geoRouter(gf)

	before := testutil.ToFloat64(metrics.GeoBlockedRequests.WithLabelValues(region))
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = "203.0.113.99:1"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", w.Code)
	}
	if got := testutil.ToFloat64(metrics.GeoBlockedRequests.WithLabelValues(region)); got != before+1 {
		t.Errorf("counter: got %v want %v", got, before+1)
	}
}
