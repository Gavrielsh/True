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
// An IP absent from the table resolves to "" (unknown), which the fence must
// now treat as a REJECT.
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

// newFence builds a fence for tests, failing the test on a construction error.
func newFence(t *testing.T, res RegionResolver, blocked, proxies []string) *GeoFence {
	t.Helper()
	gf, err := NewGeoFenceWithResolver(res, blocked, proxies, discardLogger())
	if err != nil {
		t.Fatalf("NewGeoFenceWithResolver: %v", err)
	}
	return gf
}

// probe issues a request from remoteAddr with an optional XFF header.
func probe(t *testing.T, gf *GeoFence, remoteAddr, xff string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	geoRouter(gf).ServeHTTP(w, req)
	return w
}

// ----------------------------------------------------------------------------
// Blocking behaviour
// ----------------------------------------------------------------------------

func TestGeoFence_BlocksAndAdmits(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{
		"203.0.113.7":  "US-WA", // blocked
		"203.0.113.8":  "US-ID", // blocked
		"203.0.113.9":  "US-TN", // blocked
		"203.0.113.10": "US-WY", // blocked
		"203.0.113.20": "US-NJ", // permitted
		"203.0.113.21": "US-CA", // permitted
	}}
	gf := newFence(t, resolver, nil, nil) // nil blocked → DefaultBlockedRegions

	cases := []struct {
		ip     string
		status int
	}{
		{"203.0.113.7", http.StatusForbidden},
		{"203.0.113.8", http.StatusForbidden},
		{"203.0.113.9", http.StatusForbidden},
		{"203.0.113.10", http.StatusForbidden},
		{"203.0.113.20", http.StatusOK},
		{"203.0.113.21", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			w := probe(t, gf, tc.ip+":1234", "")
			if w.Code != tc.status {
				t.Fatalf("got %d want %d (body=%s)", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

// The four states named by the operator must be in the shipped default list.
func TestDefaultBlockedRegionsCoversRequiredStates(t *testing.T) {
	t.Parallel()
	required := []string{"US-WA", "US-ID", "US-TN", "US-WY"}
	have := map[string]bool{}
	for _, r := range DefaultBlockedRegions {
		have[r] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Errorf("DefaultBlockedRegions is missing %s", r)
		}
	}
}

// ----------------------------------------------------------------------------
// FAIL CLOSED — the core of this fix
// ----------------------------------------------------------------------------

// Every path that cannot POSITIVELY establish a permitted region must reject.
// Each of these previously called c.Next() and admitted the request.
func TestGeoFence_FailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		resolver   RegionResolver
		remoteAddr string
		xff        string
	}{
		{
			// GeoLite2 has no subdivision for this address — routine for
			// mobile/CGNAT and formerly the most common silent admit.
			name:       "unknown region",
			resolver:   &fakeResolver{regions: map[string]string{}},
			remoteAddr: "203.0.113.50:1234",
		},
		{
			name:       "resolver error",
			resolver:   &fakeResolver{err: errors.New("geoip corrupted")},
			remoteAddr: "203.0.113.50:1234",
		},
		{
			name:       "unparseable remote address",
			resolver:   &fakeResolver{regions: map[string]string{}},
			remoteAddr: "not-an-ip",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gf := newFence(t, tc.resolver, nil, nil)
			w := probe(t, gf, tc.remoteAddr, tc.xff)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s must fail CLOSED: got %d want 403 (body=%s)", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// A rejection must be indistinguishable from a blocked-region rejection —
// otherwise the fence is a probing oracle that tells an attacker whether their
// spoofed IP geolocated at all.
func TestGeoFence_RejectionIsOpaque(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{"203.0.113.7": "US-WA"}}
	gf := newFence(t, resolver, nil, nil)

	blocked := probe(t, gf, "203.0.113.7:1234", "")  // known blocked state
	unknown := probe(t, gf, "203.0.113.99:1234", "") // unresolvable

	if blocked.Code != unknown.Code {
		t.Fatalf("status differs: blocked=%d unknown=%d", blocked.Code, unknown.Code)
	}
	var b, u map[string]any
	_ = json.Unmarshal(blocked.Body.Bytes(), &b)
	_ = json.Unmarshal(unknown.Body.Bytes(), &u)
	if b["code"] != u["code"] || b["message"] != u["message"] {
		t.Fatalf("response distinguishes the two cases: blocked=%v unknown=%v", b, u)
	}
	if b["code"] != string(errs.CodeGeoBlocked) {
		t.Errorf("code: got %v want %s", b["code"], errs.CodeGeoBlocked)
	}
}

// ----------------------------------------------------------------------------
// X-Forwarded-For trust
// ----------------------------------------------------------------------------

// THE SPOOFING FIX: with no trusted proxies configured, the header is ignored
// entirely. Previously any caller could set it and pick their jurisdiction.
func TestGeoFence_IgnoresXFFFromUntrustedPeer(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{
		"203.0.113.7": "US-WA", // the real peer — blocked
		"8.8.8.8":     "US-NJ", // what the attacker claims — permitted
	}}
	gf := newFence(t, resolver, nil, nil) // no trusted proxies

	w := probe(t, gf, "203.0.113.7:1234", "8.8.8.8")
	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed XFF from an untrusted peer must be ignored: got %d want 403", w.Code)
	}
}

// Behind a trusted proxy the header IS honoured, so real players are geolocated.
func TestGeoFence_HonoursXFFFromTrustedProxy(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{
		"10.0.0.5":     "US-NJ", // the proxy itself
		"203.0.113.7":  "US-WA", // real client — blocked
		"203.0.113.20": "US-NJ", // real client — permitted
	}}
	gf := newFence(t, resolver, nil, []string{"10.0.0.0/8"})

	if w := probe(t, gf, "10.0.0.5:1234", "203.0.113.7"); w.Code != http.StatusForbidden {
		t.Errorf("client in a blocked state must be rejected: got %d want 403", w.Code)
	}
	if w := probe(t, gf, "10.0.0.5:1234", "203.0.113.20"); w.Code != http.StatusOK {
		t.Errorf("client in a permitted state must be admitted: got %d want 200", w.Code)
	}
}

// The chain is walked RIGHT-TO-LEFT past trusted hops. A client that prepends
// its own fake entry sits to the LEFT of the real one and must be ignored.
func TestGeoFence_WalksProxyChainRightToLeft(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{
		"203.0.113.7": "US-WA", // the REAL client, appended by our edge proxy
		"8.8.8.8":     "US-NJ", // attacker-prepended decoy
	}}
	gf := newFence(t, resolver, nil, []string{"10.0.0.0/8", "172.16.0.0/12"})

	// Attacker sent "8.8.8.8"; edge appended the real peer; inner proxy appended the edge.
	w := probe(t, gf, "10.0.0.5:1234", "8.8.8.8, 203.0.113.7, 172.16.0.9")
	if w.Code != http.StatusForbidden {
		t.Fatalf("rightmost untrusted hop is the client: got %d want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestGeoFence_TrustedProxyChainEdgeCases(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{"203.0.113.20": "US-NJ"}}
	gf := newFence(t, resolver, nil, []string{"10.0.0.0/8"})

	cases := []struct {
		name string
		xff  string
	}{
		// A trusted proxy that forwarded nothing: we know the hop, not the
		// client. Geolocating our own load balancer would admit everyone.
		{"trusted proxy sent no XFF", ""},
		// Every hop is ours → no client address anywhere in the chain.
		{"chain is all trusted proxies", "10.0.0.1, 10.0.0.2"},
		// A malformed hop means the chain's structure is untrustworthy.
		{"malformed entry", "not-an-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := probe(t, gf, "10.0.0.5:1234", tc.xff)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s must fail CLOSED: got %d want 403", tc.name, w.Code)
			}
		})
	}
}

// A bare IP (no CIDR) is accepted as a trusted proxy entry.
func TestGeoFence_TrustedProxyAcceptsBareIP(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{"203.0.113.20": "US-NJ"}}
	gf := newFence(t, resolver, nil, []string{"10.0.0.5"})

	if w := probe(t, gf, "10.0.0.5:1234", "203.0.113.20"); w.Code != http.StatusOK {
		t.Fatalf("bare-IP trusted proxy should honour XFF: got %d want 200", w.Code)
	}
}

func TestGeoFence_MalformedTrustedProxyFailsConstruction(t *testing.T) {
	t.Parallel()
	_, err := NewGeoFenceWithResolver(
		&fakeResolver{regions: map[string]string{}}, nil, []string{"not-a-cidr"}, discardLogger())
	if err == nil {
		t.Fatal("a malformed TRUSTED_PROXIES entry must fail the boot, not be silently dropped")
	}
}

// ----------------------------------------------------------------------------
// Construction / disable semantics
// ----------------------------------------------------------------------------

// An absent database must NOT silently disable enforcement.
func TestNewGeoFence_EmptyPathIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := NewGeoFence("", nil, nil, discardLogger()); err == nil {
		t.Fatal("an empty GEOIP_DB_PATH must be an error, not a silent disable")
	}
}

// The explicit dev-mode opt-out admits everything — and is the ONLY way to
// run without enforcement.
func TestGeoFence_ExplicitlyDisabledAdmits(t *testing.T) {
	t.Parallel()
	gf := NewDisabledGeoFence(discardLogger())
	if gf.Enabled() {
		t.Error("a disabled fence must report Enabled() == false")
	}
	if w := probe(t, gf, "203.0.113.7:1234", ""); w.Code != http.StatusOK {
		t.Fatalf("disabled fence must admit: got %d want 200", w.Code)
	}
}

// A fence that is "enabled" but has no resolver is a wiring bug — and must
// still reject rather than admit.
func TestGeoFence_MisconfiguredRejects(t *testing.T) {
	t.Parallel()
	gf := &GeoFence{logger: discardLogger(), blocked: map[string]struct{}{"US-WA": {}}}
	if w := probe(t, gf, "203.0.113.7:1234", ""); w.Code != http.StatusForbidden {
		t.Fatalf("a fence without a resolver must reject: got %d want 403", w.Code)
	}
}

func TestGeoFence_CustomBlockListOverridesDefault(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{
		"203.0.113.7":  "US-WA", // in the DEFAULT list
		"203.0.113.30": "US-NV", // only in the custom list
	}}
	gf := newFence(t, resolver, []string{"US-NV"}, nil)

	if w := probe(t, gf, "203.0.113.30:1234", ""); w.Code != http.StatusForbidden {
		t.Errorf("custom blocked region must be rejected: got %d want 403", w.Code)
	}
	// US-WA is NOT in the custom list, so it is admitted — proving the override
	// replaces the default rather than merging with it.
	if w := probe(t, gf, "203.0.113.7:1234", ""); w.Code != http.StatusOK {
		t.Errorf("custom list must replace the default: got %d want 200", w.Code)
	}
}

func TestGeoFence_BlockIncrementsCounter(t *testing.T) {
	t.Parallel()
	const region = "US-ZZ"
	resolver := &fakeResolver{regions: map[string]string{"198.51.100.1": region}}
	gf := newFence(t, resolver, []string{region}, nil)

	before := testutil.ToFloat64(metrics.GeoBlockedRequests.WithLabelValues(region))
	if w := probe(t, gf, "198.51.100.1:1234", ""); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if after := testutil.ToFloat64(metrics.GeoBlockedRequests.WithLabelValues(region)); after != before+1 {
		t.Errorf("counter: got %v want %v", after, before+1)
	}
}

func TestGeoFence_CloseReleasesResolver(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{regions: map[string]string{}}
	gf := newFence(t, resolver, nil, nil)
	if err := gf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !resolver.closed {
		t.Error("Close must release the underlying resolver")
	}
}
