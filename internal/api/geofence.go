package api

// geofence.go enforces US sweepstakes jurisdiction restrictions: requests
// originating from blocked ISO-3166-2 regions (e.g. US-WA) are rejected with
// 403 GEO_BLOCKED. The check runs AFTER HMAC + replay verification so an
// unauthenticated caller can never use it as a region-probing oracle, and
// only on /api/v1 (operator traffic).
//
// X-Forwarded-For is trusted here because the engine sits behind the
// operator gateway, which strips and rewrites the header. This middleware is
// a compliance backstop, not the only enforcement layer.

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/oschwald/geoip2-golang"

	"github.com/Gavrielsh/True/internal/metrics"
	"github.com/Gavrielsh/True/pkg/errors"
)

// RegionResolver resolves a client IP to an ISO-3166-2 region code (e.g.
// "US-WA"). An empty region with nil error means "unknown". Implemented by
// the MaxMind reader in production and by fakes in tests.
type RegionResolver interface {
	Region(ip net.IP) (string, error)
	Close() error
}

// GeoFence blocks requests from restricted jurisdictions. A nil *GeoFence or
// one without a resolver is a no-op (dev mode).
type GeoFence struct {
	resolver RegionResolver
	blocked  map[string]struct{}
	logger   *slog.Logger
}

// NewGeoFence opens the MaxMind database at dbPath. An EMPTY dbPath disables
// geo-checking entirely (dev mode) and logs a single WARN at startup — the
// engine must never crash because a dev box has no GeoLite2 file.
func NewGeoFence(dbPath string, blockedRegions []string, logger *slog.Logger) (*GeoFence, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if dbPath == "" {
		logger.Warn("geofence disabled: GEOIP_DB_PATH is empty (dev mode) — " +
			"jurisdiction blocking is NOT enforced")
		return &GeoFence{logger: logger}, nil
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open geoip database: %w", err)
	}
	return NewGeoFenceWithResolver(&maxmindResolver{db: db}, blockedRegions, logger), nil
}

// NewGeoFenceWithResolver wires an explicit resolver — the seam tests use so
// they never need a real MaxMind file.
func NewGeoFenceWithResolver(res RegionResolver, blockedRegions []string, logger *slog.Logger) *GeoFence {
	if logger == nil {
		logger = slog.Default()
	}
	blocked := make(map[string]struct{}, len(blockedRegions))
	for _, r := range blockedRegions {
		r = strings.ToUpper(strings.TrimSpace(r))
		if r != "" {
			blocked[r] = struct{}{}
		}
	}
	return &GeoFence{resolver: res, blocked: blocked, logger: logger}
}

// Enabled reports whether geo-checking is active.
func (g *GeoFence) Enabled() bool {
	return g != nil && g.resolver != nil && len(g.blocked) > 0
}

// Close releases the underlying database handle.
func (g *GeoFence) Close() error {
	if g == nil || g.resolver == nil {
		return nil
	}
	return g.resolver.Close()
}

// Middleware returns the gin handler enforcing the fence. Resolution
// failures and unknown regions FAIL OPEN with a WARN: this layer is a
// compliance backstop behind the gateway's primary enforcement, and a
// GeoLite2 gap must not take down paying traffic in permitted states.
func (g *GeoFence) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !g.Enabled() {
			c.Next()
			return
		}
		ip := clientIPFrom(c)
		if ip == nil {
			g.logger.WarnContext(c.Request.Context(), "geofence: unparseable client ip",
				slog.String("remote_addr", c.Request.RemoteAddr))
			c.Next()
			return
		}
		region, err := g.resolver.Region(ip)
		if err != nil {
			g.logger.WarnContext(c.Request.Context(), "geofence: region lookup failed",
				slog.String("error", err.Error()))
			c.Next()
			return
		}
		if _, isBlocked := g.blocked[region]; isBlocked {
			metrics.GeoBlockedRequests.WithLabelValues(region).Inc()
			respondErrorCode(c, http.StatusForbidden, errors.CodeGeoBlocked,
				"service not available in your region")
			return
		}
		c.Next()
	}
}

// clientIPFrom extracts the client IP: the FIRST entry of X-Forwarded-For
// (the original client; later hops are proxies), falling back to the socket
// peer in RemoteAddr.
func clientIPFrom(c *gin.Context) net.IP {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		host = c.Request.RemoteAddr // bare IP without port
	}
	return net.ParseIP(host)
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
		return "", nil // not enough data — treated as unknown (fail open)
	}
	return rec.Country.IsoCode + "-" + rec.Subdivisions[0].IsoCode, nil
}

func (m *maxmindResolver) Close() error { return m.db.Close() }
