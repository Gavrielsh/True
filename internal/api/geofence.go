package api

// geofence.go enforces US sweepstakes jurisdiction restrictions: requests
// originating from blocked ISO-3166-2 regions (e.g. US-WA) are rejected with
// 403 GEO_BLOCKED. The check runs AFTER HMAC + replay verification so an
// unauthenticated caller can never use it as a region-probing oracle, and
// only on /api/v1 (operator traffic).
//
// ─────────────────────────────────────────────────────────────────────────────
// FAIL CLOSED (audit fix)
// ─────────────────────────────────────────────────────────────────────────────
// This middleware previously admitted the request on EVERY failure path —
// unparseable IP, resolver error, and (most commonly) a GeoLite2 record with
// no subdivision data, which is routine for mobile and CGNAT ranges. Combined
// with an empty GEOIP_DB_PATH disabling the fence entirely, the blocklist
// looked configured and enforced nothing.
//
// A jurisdiction control that fails open is not a control. Every path that
// cannot POSITIVELY establish a permitted region now returns 403. The cost of
// a false reject is a support ticket; the cost of a false admit is serving a
// prohibited state.
//
// ─────────────────────────────────────────────────────────────────────────────
// X-FORWARDED-FOR IS NOT TRUSTED BY DEFAULT
// ─────────────────────────────────────────────────────────────────────────────
// The header was previously read unconditionally, so any caller could set
// `X-Forwarded-For: 8.8.8.8` and choose their own jurisdiction. It is now
// honoured ONLY when the socket peer is a configured trusted proxy, and the
// client is resolved by walking the chain right-to-left past known proxies.
//
// ─────────────────────────────────────────────────────────────────────────────
// SCOPE — READ THIS BEFORE RELYING ON IT
// ─────────────────────────────────────────────────────────────────────────────
// The engine is called SERVER-TO-SERVER by the gateway and by aggregators, so
// the socket peer is a datacenter address, not a player. This fence therefore
// only sees a real player IP when a trusted upstream forwards one. It is a
// BACKSTOP. Primary enforcement belongs at the gateway, where the player's
// connection actually terminates — see the gateway's own geo middleware.

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/oschwald/geoip2-golang"

	"github.com/Gavrielsh/True/internal/metrics"
	apperrs "github.com/Gavrielsh/True/pkg/errors"
)

// DefaultBlockedRegions are the US states this platform does not serve.
//
// WA and ID prohibit or effectively prohibit online sweepstakes play. TN and
// WY are included at the operator's direction. This list is a DEFAULT, not a
// legal opinion: it is overridable via BLOCKED_REGIONS precisely so counsel
// can adjust it without a code change, and it should be reviewed by counsel
// before launch and whenever a state's position changes.
var DefaultBlockedRegions = []string{
	"US-WA", // Washington
	"US-ID", // Idaho
	"US-TN", // Tennessee
	"US-WY", // Wyoming
}

// Sentinel reasons for a fail-closed rejection. Surfaced in logs and metrics,
// never to the caller (who gets one opaque message).
var (
	errNoClientIP     = errors.New("client ip could not be determined")
	errRegionUnknown  = errors.New("region could not be resolved")
	errResolverFailed = errors.New("region lookup failed")
)

// RegionResolver resolves a client IP to an ISO-3166-2 region code (e.g.
// "US-WA"). An empty region with nil error means "unknown" — which now
// results in a REJECT, not an admit.
type RegionResolver interface {
	Region(ip net.IP) (string, error)
	Close() error
}

// GeoFence blocks requests from restricted jurisdictions.
type GeoFence struct {
	resolver RegionResolver
	blocked  map[string]struct{}
	logger   *slog.Logger

	// trustedProxies are the networks whose X-Forwarded-For header is
	// honoured. Empty means the header is IGNORED entirely and the socket
	// peer is the client — the safe default.
	trustedProxies []*net.IPNet

	// disabled short-circuits the middleware. Set ONLY via the explicit
	// dev-mode escape hatch, never by a missing database.
	disabled bool
}

// NewGeoFence opens the MaxMind database at dbPath and builds the fence.
//
// Unlike the previous behaviour, an empty dbPath is an ERROR, not a silent
// disable: a fence that quietly turns itself off is how a blocklist ends up
// looking configured while enforcing nothing. Use NewDisabledGeoFence for the
// explicit dev-mode opt-out.
func NewGeoFence(dbPath string, blockedRegions, trustedProxyCIDRs []string, logger *slog.Logger) (*GeoFence, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(dbPath) == "" {
		return nil, errors.New("geofence: GEOIP_DB_PATH is required " +
			"(set GEOFENCE_MODE=disabled to explicitly run without jurisdiction enforcement)")
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("geofence: open geoip database: %w", err)
	}
	return NewGeoFenceWithResolver(&maxmindResolver{db: db}, blockedRegions, trustedProxyCIDRs, logger)
}

// NewDisabledGeoFence returns an explicitly-disabled fence for local dev.
// Logs at ERROR (not WARN) because a running system with jurisdiction
// enforcement off is a compliance incident if it ever reaches production.
func NewDisabledGeoFence(logger *slog.Logger) *GeoFence {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("GEOFENCE DISABLED — jurisdiction blocking is NOT enforced. " +
		"This is a dev-only mode (GEOFENCE_MODE=disabled) and MUST NOT run in production.")
	return &GeoFence{logger: logger, disabled: true}
}

// NewGeoFenceWithResolver wires an explicit resolver — the seam tests use so
// they never need a real MaxMind file.
func NewGeoFenceWithResolver(
	res RegionResolver,
	blockedRegions, trustedProxyCIDRs []string,
	logger *slog.Logger,
) (*GeoFence, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if res == nil {
		return nil, errors.New("geofence: resolver is required")
	}

	regions := blockedRegions
	if len(regions) == 0 {
		regions = DefaultBlockedRegions
	}
	blocked := make(map[string]struct{}, len(regions))
	for _, r := range regions {
		if r = strings.ToUpper(strings.TrimSpace(r)); r != "" {
			blocked[r] = struct{}{}
		}
	}
	if len(blocked) == 0 {
		return nil, errors.New("geofence: blocked region list resolved to empty")
	}

	proxies, err := parseTrustedProxies(trustedProxyCIDRs)
	if err != nil {
		return nil, err
	}

	logger.Info("geofence enabled",
		slog.Int("blocked_regions", len(blocked)),
		slog.String("regions", strings.Join(sortedKeys(blocked), ",")),
		slog.Int("trusted_proxies", len(proxies)),
	)

	return &GeoFence{
		resolver:       res,
		blocked:        blocked,
		logger:         logger,
		trustedProxies: proxies,
	}, nil
}

// parseTrustedProxies accepts CIDRs ("10.0.0.0/8") and bare IPs ("10.1.2.3",
// normalised to a /32 or /128). A malformed entry is a boot error — silently
// dropping one would leave the header trusted from an unintended network.
func parseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if _, block, err := net.ParseCIDR(e); err == nil {
			out = append(out, block)
			continue
		}
		ip := net.ParseIP(e)
		if ip == nil {
			return nil, fmt.Errorf("geofence: TRUSTED_PROXIES entry %q is neither an IP nor a CIDR", e)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small, bounded set; a simple insertion sort keeps this dependency-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Enabled reports whether geo-checking is active.
func (g *GeoFence) Enabled() bool {
	return g != nil && !g.disabled && g.resolver != nil && len(g.blocked) > 0
}

// Close releases the underlying database handle.
func (g *GeoFence) Close() error {
	if g == nil || g.resolver == nil {
		return nil
	}
	return g.resolver.Close()
}

// Middleware returns the gin handler enforcing the fence.
//
// FAIL CLOSED: every path that cannot positively establish a permitted region
// rejects with 403. There is no admit-on-error branch.
func (g *GeoFence) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if g == nil || g.disabled {
			c.Next()
			return
		}
		// A fence with no resolver reaching this point is a wiring bug. Reject
		// rather than admit — the whole point is that failure is not an admit.
		if g.resolver == nil || len(g.blocked) == 0 {
			g.reject(c, "misconfigured", errors.New("geofence enabled without resolver or blocklist"))
			return
		}

		ip, err := g.clientIP(c)
		if err != nil {
			g.reject(c, "no_client_ip", err)
			return
		}

		region, err := g.resolver.Region(ip)
		if err != nil {
			g.reject(c, "lookup_failed", fmt.Errorf("%w: %v", errResolverFailed, err))
			return
		}
		if region == "" {
			// GeoLite2 had no subdivision for this address. Common for mobile
			// and CGNAT ranges — and previously the most frequent silent
			// admit. We cannot prove the request is from a permitted state, so
			// we do not serve it.
			g.reject(c, "region_unknown", errRegionUnknown)
			return
		}
		if _, isBlocked := g.blocked[strings.ToUpper(region)]; isBlocked {
			metrics.GeoBlockedRequests.WithLabelValues(region).Inc()
			g.logger.InfoContext(c.Request.Context(), "geofence: blocked region",
				slog.String("region", region))
			respondErrorCode(c, http.StatusForbidden, apperrs.CodeGeoBlocked,
				"service not available in your region")
			return
		}
		c.Next()
	}
}

// reject logs the specific reason and returns the SAME opaque 403 the blocked
// -region path returns. The caller must not be able to distinguish "you are in
// Washington" from "we could not geolocate you" — that difference is a probing
// oracle.
func (g *GeoFence) reject(c *gin.Context, reason string, err error) {
	metrics.GeoBlockedRequests.WithLabelValues(reason).Inc()
	g.logger.WarnContext(c.Request.Context(), "geofence: rejected (fail closed)",
		slog.String("reason", reason),
		slog.String("error", err.Error()),
		slog.String("remote_addr", c.Request.RemoteAddr),
	)
	respondErrorCode(c, http.StatusForbidden, apperrs.CodeGeoBlocked,
		"service not available in your region")
}

// clientIP resolves the caller's address.
//
// The socket peer is authoritative UNLESS it is a configured trusted proxy.
// Only then is X-Forwarded-For consulted, and it is walked RIGHT-TO-LEFT:
// each proxy appends the address it saw, so the rightmost entries are the
// ones added by infrastructure we control. The first entry from the right
// that is NOT a trusted proxy is the real client. Anything a client prepends
// itself sits to the LEFT of that and is correctly ignored.
//
// Returns an error (→ 403) rather than a best guess whenever the chain cannot
// be resolved.
func (g *GeoFence) clientIP(c *gin.Context) (net.IP, error) {
	peer, err := peerIP(c.Request.RemoteAddr)
	if err != nil {
		return nil, err
	}

	// Not behind a trusted proxy → the header is attacker-controlled noise.
	if !g.isTrustedProxy(peer) {
		return peer, nil
	}

	xff := c.GetHeader("X-Forwarded-For")
	if strings.TrimSpace(xff) == "" {
		// A trusted proxy that forwarded nothing: we know the hop, not the
		// client. Fail closed rather than geolocating our own load balancer.
		return nil, fmt.Errorf("%w: trusted proxy sent no X-Forwarded-For", errNoClientIP)
	}

	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(parts[i])
		if raw == "" {
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			// A malformed hop means we cannot trust the chain's structure.
			return nil, fmt.Errorf("%w: malformed X-Forwarded-For entry", errNoClientIP)
		}
		if !g.isTrustedProxy(ip) {
			return ip, nil
		}
	}
	// Every hop was a trusted proxy — no client address in the chain.
	return nil, fmt.Errorf("%w: X-Forwarded-For contained only trusted proxies", errNoClientIP)
}

func (g *GeoFence) isTrustedProxy(ip net.IP) bool {
	for _, block := range g.trustedProxies {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// peerIP extracts the socket peer address from RemoteAddr.
func peerIP(remoteAddr string) (net.IP, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // bare IP without a port
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return nil, fmt.Errorf("%w: unparseable remote address %q", errNoClientIP, remoteAddr)
	}
	return ip, nil
}

// maxmindResolver adapts geoip2.Reader to RegionResolver, formatting
// country + first subdivision as ISO-3166-2 ("US" + "-" + "WA").
type maxmindResolver struct {
	db *geoip2.Reader
}

func (m *maxmindResolver) Region(ip net.IP) (string, error) {
	rec, err := m.db.City(ip)
	if err != nil {
		return "", err
	}
	if rec.Country.IsoCode == "" || len(rec.Subdivisions) == 0 {
		// Not enough data. Returning "" now means REJECT upstream — this is
		// the single most important behaviour change in this file.
		return "", nil
	}
	return rec.Country.IsoCode + "-" + rec.Subdivisions[0].IsoCode, nil
}

func (m *maxmindResolver) Close() error { return m.db.Close() }
