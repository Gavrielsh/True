package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	apperrs "github.com/Gavrielsh/TransactionMechanism/pkg/errors"
)

// Replay-protection headers (architecture §5).
const (
	HeaderTimestamp = "X-Timestamp"
	HeaderNonce     = "X-Nonce"
)

const (
	// defaultMaxAge: requests older than this are rejected as stale.
	defaultMaxAge = 5 * time.Minute
	// defaultClockSkew: tolerance for clocks slightly ahead of ours.
	defaultClockSkew = 30 * time.Second
	// defaultNonceTTL: how long a nonce remains "used". Must be at least
	// maxAge+clockSkew so a nonce can't be replayed via a freshly-stamped
	// request after the original nonce entry expired.
	defaultNonceTTL = 10 * time.Minute
)

// ReplayGuard prevents both wall-clock replay (X-Timestamp too old) and
// signature replay (X-Nonce reused). Nonces are stored in Redis with NX
// semantics so the check and insert are atomic.
//
// FAIL CLOSED — any Redis error returns HTTP 500. A degraded nonce store
// is indistinguishable from an unbounded replay window.
type ReplayGuard struct {
	redis     redis.UniversalClient
	maxAge    time.Duration
	clockSkew time.Duration
	nonceTTL  time.Duration
	now       func() time.Time // injectable for tests
}

// NewReplayGuard builds a guard with the architecture defaults.
func NewReplayGuard(client redis.UniversalClient) *ReplayGuard {
	return &ReplayGuard{
		redis:     client,
		maxAge:    defaultMaxAge,
		clockSkew: defaultClockSkew,
		nonceTTL:  defaultNonceTTL,
		now:       time.Now,
	}
}

// Middleware returns the gin.HandlerFunc enforcing both the timestamp and
// nonce checks. Must be registered AFTER the HMAC middleware so the nonce
// key can be scoped to the verified operator (a single rogue caller cannot
// invalidate another operator's nonces).
func (r *ReplayGuard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Timestamp ---------------------------------------------------------
		tsStr := c.GetHeader(HeaderTimestamp)
		if tsStr == "" {
			respondErrorCode(c, http.StatusBadRequest, apperrs.Code("MISSING_TIMESTAMP"), "X-Timestamp header is required")
			return
		}
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			respondErrorCode(c, http.StatusBadRequest, apperrs.Code("INVALID_TIMESTAMP"), "X-Timestamp must be a unix-seconds integer")
			return
		}
		reqTime := time.Unix(ts, 0)
		now := r.now()
		age := now.Sub(reqTime)
		// Reject if the request is too old OR is too far in the future
		// (clock-skew tolerance only — a request stamped a year ahead is
		// almost certainly a replay attempt).
		if age > r.maxAge || age < -r.clockSkew {
			respondErrorCode(c, http.StatusUnauthorized, apperrs.Code("STALE_REQUEST"), "X-Timestamp outside acceptable window")
			return
		}

		// Nonce -------------------------------------------------------------
		nonce := c.GetHeader(HeaderNonce)
		if nonce == "" {
			respondErrorCode(c, http.StatusBadRequest, apperrs.Code("MISSING_NONCE"), "X-Nonce header is required")
			return
		}

		// Operator-scoped key (HMAC middleware already verified the operator
		// header — context value is the trusted source).
		operator := OperatorCodeFromContext(c.Request.Context())
		key := nonceKey(operator, nonce)

		ok, err := r.redis.SetNX(c.Request.Context(), key, "1", r.nonceTTL).Result()
		if err != nil {
			// FAIL CLOSED. Surface as 500 so retries can succeed once Redis
			// recovers, rather than 401 (which would tell a bot they should
			// stop retrying).
			respondErrorCode(c, http.StatusInternalServerError, apperrs.CodeInternal, "nonce store unavailable")
			return
		}
		if !ok {
			respondErrorCode(c, http.StatusUnauthorized, apperrs.Code("REPLAY_DETECTED"), "X-Nonce already used")
			return
		}

		c.Next()
	}
}

// nonceKey scopes the nonce by operator so one operator cannot burn another's
// nonces. We intentionally do NOT scope per-endpoint: aggregators reuse a
// single nonce sequence across all their calls, so a nonce must be unique
// across the operator's entire request stream.
func nonceKey(operator, nonce string) string {
	return "api:nonce:" + operator + ":" + nonce
}
