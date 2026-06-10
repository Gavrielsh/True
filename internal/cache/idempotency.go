// Package cache implements the Redis-backed idempotency barrier sitting in
// front of every state-mutating wallet endpoint (/bet, /win, /rollback).
//
// The two-phase contract (architecture §4):
//
//	Phase 1 — Acquire:  atomic GET-then-conditional-SET (single Lua script)
//	Phase 2 — Store:    SET key=<json> XX PX 86400000  (after DB commit)
//
// Phase 1 runs a server-side Lua script so the "is the key present?" read and
// the "claim it with PROCESSING" write are ONE atomic step. This eliminates the
// SETNX-then-GET race where the key's TTL could fire in the window between the
// two round-trips (a concurrent retry would then observe a phantom miss). The
// script returns one of three discriminated outcomes — Acquired / Pending /
// Cached — with no second network call.
//
// FAIL CLOSED: any Redis error short-circuits the request with HTTP 5xx;
// proceeding without idempotency could double-charge a player on retry.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/telemetry"
)

// AcquireStatus enumerates the three outcomes of an Acquire call.
type AcquireStatus int

const (
	// StatusUnknown is the zero value — never returned alongside a nil error.
	StatusUnknown AcquireStatus = iota
	// StatusAcquired: lock taken, caller MUST proceed to the DB transaction
	// and MUST eventually call Store (on commit) or Release (on rollback).
	StatusAcquired
	// StatusPending: a concurrent in-flight request holds the PROCESSING
	// marker. Caller should return HTTP 409 to the operator (it will retry).
	StatusPending
	// StatusCached: a prior request already completed. Payload is the cached
	// success body — return it verbatim to the operator.
	StatusCached
)

// ProcessingMarker is the canonical "lock held" sentinel stored under the
// idempotency key during Phase 1. Compared exactly so that cached JSON
// payloads (always start with '{' or '[') are never confused for it.
const ProcessingMarker = "PROCESSING"

const (
	keyPrefix       = "api:idempotency:"
	defaultLockTTL  = 10 * time.Second
	defaultCacheTTL = 24 * time.Hour
)

// Store is the contract the repository depends on. Defined here so the
// repository can be unit-tested with an in-memory fake while production
// uses Redis.
type Store interface {
	Acquire(ctx context.Context, opTxID string) (AcquireStatus, string, error)
	Store(ctx context.Context, opTxID string, payload string) error
	Release(ctx context.Context, opTxID string) error
}

// Redis is the concrete Store backed by github.com/redis/go-redis/v9.
type Redis struct {
	client   redis.UniversalClient
	lockTTL  time.Duration
	cacheTTL time.Duration
}

// NewRedis constructs a Redis-backed idempotency store using the architecture
// defaults (10s acquire window, 24h cache lifetime).
func NewRedis(client redis.UniversalClient) *Redis {
	return &Redis{
		client:   client,
		lockTTL:  defaultLockTTL,
		cacheTTL: defaultCacheTTL,
	}
}

// NewRedisWithTTLs is the test-friendly constructor: callers can shorten
// the TTLs to deterministic values (e.g. 100ms) without race-prone sleeps.
func NewRedisWithTTLs(client redis.UniversalClient, lockTTL, cacheTTL time.Duration) *Redis {
	return &Redis{client: client, lockTTL: lockTTL, cacheTTL: cacheTTL}
}

func keyFor(opTxID string) string { return keyPrefix + opTxID }

// Lua return codes for acquireScript. Kept in lock-step with the script body.
const (
	luaAcquired int64 = 1 // we set the PROCESSING marker
	luaPending  int64 = 2 // marker already held by a concurrent request
	luaCached   int64 = 3 // a JSON payload is already cached
)

// acquireScript performs the entire Acquire decision in ONE atomic, server-side
// step (Redis executes a script with no interleaving). Logic:
//
//	missing       → SET PROCESSING with the lock TTL, return {acquired, ""}
//	== PROCESSING → return {pending, ""}
//	otherwise     → return {cached, <payload>}
//
// PX (millisecond TTL) is used so sub-second lock windows (tests) are exact;
// EX would truncate a 100ms TTL to 0 and error.
var acquireScript = redis.NewScript(`
local existing = redis.call('GET', KEYS[1])
if existing == false then
	redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
	return {1, ''}
end
if existing == ARGV[1] then
	return {2, ''}
end
return {3, existing}
`)

// Acquire atomically claims the idempotency key. See AcquireStatus for the
// possible outcomes. The returned payload is non-empty only for StatusCached.
//
// Because the GET and the conditional SET happen inside one Lua invocation,
// there is no SETNX-then-GET window for the TTL to expire through — the race
// the previous implementation papered over with a "treat phantom miss as
// Pending" branch simply cannot occur.
func (r *Redis) Acquire(ctx context.Context, opTxID string) (status AcquireStatus, payload string, err error) {
	// The idempotency key is operator_code:operator_transaction_id — an
	// identifier, not PII (§9), and the single most useful pivot when
	// debugging a Ghost-Spin.
	ctx, span := telemetry.StartSpan(ctx, "redis.idempotency_acquire",
		attribute.String("idempotency_key", keyFor(opTxID)))
	defer func() { telemetry.EndSpan(span, err) }()

	if opTxID == "" {
		return StatusUnknown, "", errors.New("idempotency: empty operator_transaction_id")
	}
	k := keyFor(opTxID)

	// PX demands an integer >= 1ms.
	ttlMillis := r.lockTTL.Milliseconds()
	if ttlMillis < 1 {
		ttlMillis = 1
	}

	res, err := acquireScript.Run(ctx, r.client, []string{k}, ProcessingMarker, ttlMillis).Result()
	if err != nil {
		return StatusUnknown, "", fmt.Errorf("idempotency acquire script: %w", err)
	}

	code, payload, err := parseAcquireResult(res)
	if err != nil {
		return StatusUnknown, "", err
	}
	switch code {
	case luaAcquired:
		return StatusAcquired, "", nil
	case luaPending:
		return StatusPending, "", nil
	case luaCached:
		return StatusCached, payload, nil
	default:
		return StatusUnknown, "", fmt.Errorf("idempotency: unexpected acquire code %d", code)
	}
}

// parseAcquireResult unpacks the {code, payload} table the Lua script returns.
// go-redis decodes a Lua array into []any with an int64 head; the payload is a
// string (or []byte on some transports) and may be empty.
func parseAcquireResult(res any) (int64, string, error) {
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return 0, "", fmt.Errorf("idempotency: malformed acquire result %T", res)
	}
	code, ok := arr[0].(int64)
	if !ok {
		return 0, "", fmt.Errorf("idempotency: acquire code not int64 (%T)", arr[0])
	}
	switch v := arr[1].(type) {
	case string:
		return code, v, nil
	case []byte:
		return code, string(v), nil
	case nil:
		return code, "", nil
	default:
		return 0, "", fmt.Errorf("idempotency: acquire payload bad type %T", arr[1])
	}
}

// Store writes the final success payload, transitioning the key from
// PROCESSING to the cached response with 24h TTL. The XX guard ensures we
// only overwrite our own PROCESSING marker — if the key expired and a fresh
// PROCESSING from a retry has appeared, we leave it alone (the retry will
// hit 23505 on the DB INSERT and recover via Ghost-Spin).
func (r *Redis) Store(ctx context.Context, opTxID string, payload string) error {
	if opTxID == "" {
		return errors.New("idempotency: empty operator_transaction_id")
	}
	if payload == "" {
		return errors.New("idempotency: empty payload")
	}
	// SetXX returns BoolCmd: true = overwrote, false = key didn't exist.
	// Both are acceptable here (the second case means our PROCESSING expired).
	if err := r.client.SetXX(ctx, keyFor(opTxID), payload, r.cacheTTL).Err(); err != nil {
		return fmt.Errorf("idempotency SET XX: %w", err)
	}
	return nil
}

// Release deletes the PROCESSING marker so operator retries don't have to
// wait for the 10s TTL. Called on the failure paths only — on commit, Store
// overwrites the marker with the payload.
func (r *Redis) Release(ctx context.Context, opTxID string) error {
	if opTxID == "" {
		return errors.New("idempotency: empty operator_transaction_id")
	}
	if err := r.client.Del(ctx, keyFor(opTxID)).Err(); err != nil {
		return fmt.Errorf("idempotency DEL: %w", err)
	}
	return nil
}
