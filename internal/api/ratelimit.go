package api

// ratelimit.go implements Redis-backed sliding-window rate limiting at two
// scopes:
//
//	operator — middleware between HMAC and the replay guard; caps each
//	           verified operator_code at RATE_LIMIT_RPS req/s.
//	player   — invoked inside each money handler AFTER the body is parsed
//	           (so the player_id is HMAC-authenticated, never guessed);
//	           caps each player at 100 req/s per endpoint.
//
// The window check is ONE atomic Lua script (ZREMRANGEBYSCORE + ZCARD +
// ZADD + PEXPIRE) so concurrent requests can never both observe "one slot
// left" and both claim it.
//
// AVAILABILITY OVER STRICTNESS: unlike the idempotency barrier (which fails
// closed — correctness), a Redis outage here FAILS OPEN with a WARN. Rate
// limiting is a protection control, not a correctness control; rejecting
// every paying request because the limiter's control plane is down would
// convert a partial outage into a total one.

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Gavrielsh/True/internal/metrics"
)

const (
	defaultOperatorRPS = 5000
	defaultPlayerRPS   = 100
	rateLimitWindow    = time.Second

	rlScopeOperator = "operator"
	rlScopePlayer   = "player"
)

// rateLimitScript is the atomic sliding-window check.
//
//	KEYS[1] = window key (ratelimit:<scope>:...)
//	ARGV[1] = now (unix ms)
//	ARGV[2] = window length (ms)
//	ARGV[3] = limit (max entries per window)
//	ARGV[4] = unique member for this request
//
// Returns {1, 0} when admitted, or {0, retry_after_ms} when over the limit,
// where retry_after_ms is when the OLDEST entry leaves the window.
var rateLimitScript = redis.NewScript(`
local cutoff = tonumber(ARGV[1]) - tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, cutoff)
local count = redis.call('ZCARD', KEYS[1])
if count >= tonumber(ARGV[3]) then
	local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
	local retry = tonumber(ARGV[2])
	if oldest[2] then
		retry = math.ceil(tonumber(oldest[2]) + tonumber(ARGV[2]) - tonumber(ARGV[1]))
		if retry < 1 then retry = 1 end
	end
	return {0, retry}
end
redis.call('ZADD', KEYS[1], tonumber(ARGV[1]), ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return {1, 0}
`)

// RateLimiter enforces both scopes against one Redis backend.
type RateLimiter struct {
	client redis.UniversalClient
	logger *slog.Logger

	operatorRPS int
	playerRPS   int
	window      time.Duration
	// now is injectable so tests can walk the window deterministically.
	now func() time.Time
}

// RateLimitOption configures the RateLimiter.
type RateLimitOption func(*RateLimiter)

// WithOperatorRPS caps requests/second per operator (ignored if <= 0).
func WithOperatorRPS(n int) RateLimitOption {
	return func(rl *RateLimiter) {
		if n > 0 {
			rl.operatorRPS = n
		}
	}
}

// WithPlayerRPS caps requests/second per player per endpoint (ignored if <= 0).
func WithPlayerRPS(n int) RateLimitOption {
	return func(rl *RateLimiter) {
		if n > 0 {
			rl.playerRPS = n
		}
	}
}

// NewRateLimiter builds a limiter. A nil logger falls back to slog.Default().
func NewRateLimiter(client redis.UniversalClient, logger *slog.Logger, opts ...RateLimitOption) *RateLimiter {
	if logger == nil {
		logger = slog.Default()
	}
	rl := &RateLimiter{
		client:      client,
		logger:      logger,
		operatorRPS: defaultOperatorRPS,
		playerRPS:   defaultPlayerRPS,
		window:      rateLimitWindow,
		now:         time.Now,
	}
	for _, o := range opts {
		o(rl)
	}
	return rl
}

// allow runs one sliding-window check. Returns admitted=true and a zero
// retry on success; admitted=false with the computed backoff when over the
// limit. ANY Redis failure admits the request (fail open) after a WARN.
func (rl *RateLimiter) allow(ctx context.Context, key string, limit int) (admitted bool, retryAfter time.Duration) {
	nowMillis := rl.now().UnixMilli()
	// The ZADD member only needs to be unique within one key's window across
	// every engine replica sharing this Redis: timestamp + 64 random bits is
	// ample, far cheaper on this 10k-TPS path than a UUID (crypto/rand) and
	// fmt.Sprintf (reflection).
	//nolint:gosec // G404: this suffix only disambiguates two sliding-window members
	// landing in the same millisecond. It is not a secret, a token, or a game outcome —
	// nothing about rate limiting is weakened if it is predictable. The engine's actual
	// randomness (game RNG) uses crypto/rand and has no config knob for anything weaker.
	member := strconv.FormatInt(nowMillis, 36) + "-" + strconv.FormatUint(rand.Uint64(), 36)

	res, err := rateLimitScript.Run(ctx, rl.client, []string{key},
		nowMillis, rl.window.Milliseconds(), limit, member).Result()
	if err != nil {
		rl.logger.WarnContext(ctx, "rate limiter unavailable, admitting request",
			slog.String("error", err.Error()))
		return true, 0
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		rl.logger.WarnContext(ctx, "rate limiter malformed reply, admitting request",
			slog.String("type", fmt.Sprintf("%T", res)))
		return true, 0
	}
	admittedCode, _ := arr[0].(int64)
	retryMillis, _ := arr[1].(int64)
	if admittedCode == 1 {
		return true, 0
	}
	return false, time.Duration(retryMillis) * time.Millisecond
}

// OperatorMiddleware enforces the per-operator window. It MUST be mounted
// after HMAC (operator_code is verified, not client-claimed) and before the
// replay guard (a rate-limited retry must not burn its nonce).
func (rl *RateLimiter) OperatorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		op := OperatorCodeFromContext(c.Request.Context())
		if op == "" {
			// HMAC not mounted ahead of us — nothing trustworthy to key on.
			c.Next()
			return
		}
		admitted, retryAfter := rl.allow(c.Request.Context(), "ratelimit:op:"+op, rl.operatorRPS)
		if !admitted {
			rejectRateLimited(c, rlScopeOperator, retryAfter)
			return
		}
		c.Next()
	}
}

// AllowPlayer enforces the per-player window for one endpoint. Called by the
// handlers AFTER body validation, so playerID came from a signed payload.
func (rl *RateLimiter) AllowPlayer(ctx context.Context, endpoint string, playerID uuid.UUID) (bool, time.Duration) {
	key := "ratelimit:player:" + endpoint + ":" + playerID.String()
	return rl.allow(ctx, key, rl.playerRPS)
}

// rejectRateLimited writes the canonical 429 envelope and aborts.
func rejectRateLimited(c *gin.Context, scope string, retryAfter time.Duration) {
	metrics.RateLimitedRequests.WithLabelValues(scope).Inc()
	retryMillis := retryAfter.Milliseconds()
	if retryMillis < 1 {
		retryMillis = 1
	}
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"code":           "RATE_LIMITED",
		"retry_after_ms": retryMillis,
	})
}

// playerLimited runs the player-scope check inside a handler. Returns true
// (and writes the 429) when the request must stop. A nil limiter admits all.
func playerLimited(c *gin.Context, rl *RateLimiter, endpoint string, playerID uuid.UUID) bool {
	if rl == nil {
		return false
	}
	admitted, retryAfter := rl.AllowPlayer(c.Request.Context(), endpoint, playerID)
	if admitted {
		return false
	}
	rejectRateLimited(c, rlScopePlayer, retryAfter)
	return true
}
