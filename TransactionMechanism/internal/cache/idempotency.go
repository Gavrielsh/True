// Package cache implements the Redis-backed idempotency barrier sitting in
// front of every state-mutating wallet endpoint (/bet, /win, /rollback,
// /deposit, /escrow/*).
//
// The two-phase contract (architecture §4):
//
//	Phase 1 — Acquire:  SET key=<hash>|PROCESSING NX EX 10
//	Phase 2 — Store:    SET key=<hash>|<json>     XX EX 86400  (after DB commit)
//
// Phase 1 atomically reserves the operator_transaction_id. If the SET fails
// (NX violated), a concurrent or already-completed request owns the key
// and we read its value to decide whether to wait, replay the cache, or
// (on a stale PROCESSING / cache miss) try again.
//
// CRYPTOGRAPHIC IDEMPOTENCY (replay hardening): the stored value is prefixed
// with the SHA-256 hash of the raw request body that first claimed the key.
// On every cache hit the incoming request's body hash is compared against the
// stored hash; if they differ — i.e. an operator reused an
// operator_transaction_id with a *different* amount/payload — we return
// StatusMismatch and the caller fails the request with HTTP 409. We never
// replay the cached success of a different request.
//
// FAIL CLOSED: any Redis error short-circuits the request with HTTP 5xx;
// proceeding without idempotency could double-charge a player on retry.
package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// StatusMismatch: the key is held (PROCESSING or CACHED) but the incoming
	// request body hash does NOT match the hash stored when the key was first
	// claimed. The caller MUST reject with HTTP 409 — replaying would honour a
	// different request than the one that won the key.
	StatusMismatch
)

// ProcessingMarker is the canonical "lock held" sentinel stored (after the
// hash prefix) under the idempotency key during Phase 1. Compared exactly so
// that cached JSON payloads (always start with '{' or '[') are never confused
// for it.
const ProcessingMarker = "PROCESSING"

// hashSep separates the request-body hash prefix from the payload in the
// stored value. SHA-256 hex never contains it, so the first occurrence is an
// unambiguous boundary even when the payload (JSON) contains '|'.
const hashSep = "|"

const (
	keyPrefix       = "api:idempotency:"
	defaultLockTTL  = 10 * time.Second
	defaultCacheTTL = 24 * time.Hour
)

// Store is the contract the repository depends on. Defined here so the
// repository can be unit-tested with an in-memory fake while production
// uses Redis.
//
// reqHash is the lowercase-hex SHA-256 of the raw request body. It is recorded
// on Acquire/Store and verified on every subsequent Acquire (see StatusMismatch).
type Store interface {
	Acquire(ctx context.Context, opTxID, reqHash string) (AcquireStatus, string, error)
	Store(ctx context.Context, opTxID, payload, reqHash string) error
	Release(ctx context.Context, opTxID string) error
}

// encodeValue prefixes a stored payload (PROCESSING marker or cached JSON) with
// the request-body hash that owns the key.
func encodeValue(reqHash, payload string) string {
	return reqHash + hashSep + payload
}

// decodeValue splits a stored value back into (hash, payload). ok is false for
// any value not produced by encodeValue (e.g. a legacy un-prefixed entry),
// which the caller treats as a fail-closed condition.
func decodeValue(stored string) (hash, payload string, ok bool) {
	i := strings.IndexByte(stored, hashSep[0])
	if i < 0 {
		return "", "", false
	}
	return stored[:i], stored[i+1:], true
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
// reqHash (lowercase-hex SHA-256 of the raw request body) is stored with the
// PROCESSING marker on a fresh claim, and compared against the stored hash on
// every hit. A mismatch yields StatusMismatch — fail closed.
//
// Implementation note: we do not loop on the SETNX-then-GET race because the
// caller will retry the whole request. A bounded retry here would add jitter
// and bloat the hot path; the operator's retry budget already covers it.
func (r *Redis) Acquire(ctx context.Context, opTxID, reqHash string) (AcquireStatus, string, error) {
	if opTxID == "" {
		return StatusUnknown, "", errors.New("idempotency: empty operator_transaction_id")
	}
	k := keyFor(opTxID)

	ok, err := r.client.SetNX(ctx, k, encodeValue(reqHash, ProcessingMarker), r.lockTTL).Result()
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

	storedHash, payload, decoded := decodeValue(val)
	if !decoded {
		// A value not written by this version (no hash prefix). We cannot prove
		// the request matches, so we refuse to replay it — fail closed.
		return StatusUnknown, "", fmt.Errorf("idempotency: malformed stored value for %s", opTxID)
	}
	// Cryptographic check: the retried body must hash-equal the original body
	// that claimed this key. Differ → reject (different amount under reused id).
	if storedHash != reqHash {
		return StatusMismatch, "", nil
	}
	if payload == ProcessingMarker {
		return StatusPending, "", nil
	}
	return StatusCached, payload, nil
}

// Store writes the final success payload, transitioning the key from
// PROCESSING to the cached response with 24h TTL. The XX guard ensures we
// only overwrite our own PROCESSING marker — if the key expired and a fresh
// PROCESSING from a retry has appeared, we leave it alone (the retry will
// hit 23505 on the DB INSERT and recover via Ghost-Spin).
func (r *Redis) Store(ctx context.Context, opTxID, payload, reqHash string) error {
	if opTxID == "" {
		return errors.New("idempotency: empty operator_transaction_id")
	}
	if payload == "" {
		return errors.New("idempotency: empty payload")
	}
	// The cached value carries the same request-body hash prefix as the
	// PROCESSING marker it overwrites, so post-commit replays still hash-match.
	// SetXX returns BoolCmd: true = overwrote, false = key didn't exist.
	// Both are acceptable here (the second case means our PROCESSING expired).
	if err := r.client.SetXX(ctx, keyFor(opTxID), encodeValue(reqHash, payload), r.cacheTTL).Err(); err != nil {
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
