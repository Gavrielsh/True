// Package cache implements the Redis-backed idempotency barrier sitting in
// front of every state-mutating wallet endpoint (/bet, /win, /rollback).
//
// The two-phase contract (architecture §4):
//
//	Phase 1 — Acquire:  SET key=PROCESSING NX EX 10
//	Phase 2 — Store:    SET key=<json>     XX EX 86400  (after DB commit)
//
// Phase 1 atomically reserves the operator_transaction_id. If the SET fails
// (NX violated), a concurrent or already-completed request owns the key
// and we read its value to decide whether to wait, replay the cache, or
// (on a stale PROCESSING / cache miss) try again.
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

// Acquire attempts to claim the idempotency key. See AcquireStatus for the
// possible outcomes. The returned payload is non-empty only for StatusCached.
//
// Implementation note: we do not loop on the SETNX-then-GET race because the
// caller will retry the whole request. A bounded retry here would add jitter
// and bloat the hot path; the operator's retry budget already covers it.
func (r *Redis) Acquire(ctx context.Context, opTxID string) (AcquireStatus, string, error) {
	if opTxID == "" {
		return StatusUnknown, "", errors.New("idempotency: empty operator_transaction_id")
	}
	k := keyFor(opTxID)

	ok, err := r.client.SetNX(ctx, k, ProcessingMarker, r.lockTTL).Result()
	if err != nil {
		return StatusUnknown, "", fmt.Errorf("idempotency SETNX: %w", err)
	}
	if ok {
		return StatusAcquired, "", nil
	}

	// SETNX failed — read the current value to disambiguate Pending vs Cached.
	val, err := r.client.Get(ctx, k).Result()
	if errors.Is(err, redis.Nil) {
		// Race: TTL fired between SETNX and GET. Treat as Pending — operator
		// will retry, and the retry's SETNX will succeed.
		return StatusPending, "", nil
	}
	if err != nil {
		return StatusUnknown, "", fmt.Errorf("idempotency GET: %w", err)
	}
	if val == ProcessingMarker {
		return StatusPending, "", nil
	}
	return StatusCached, val, nil
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
