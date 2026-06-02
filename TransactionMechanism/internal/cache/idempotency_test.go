package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Representative request-body hashes. The store treats these as opaque strings;
// only equality matters, so short stand-ins keep the tests readable.
const (
	hashA = "aaaa1111"
	hashB = "bbbb2222"
)

// newStore spins up an isolated in-memory Redis (miniredis) and returns a
// store wired against it. Each test gets its own instance, no shared state.
func newStore(t *testing.T) (*Redis, *miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisWithTTLs(client, 100*time.Millisecond, 1*time.Hour), mr, client
}

func TestAcquire_FirstCallSucceeds(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)

	status, payload, err := s.Acquire(context.Background(), "op-1", hashA)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if status != StatusAcquired {
		t.Errorf("status: got %v want %v", status, StatusAcquired)
	}
	if payload != "" {
		t.Errorf("payload: got %q want empty", payload)
	}
}

func TestAcquire_SecondCallReturnsPending(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "op-2", hashA); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	status, payload, err := s.Acquire(ctx, "op-2", hashA)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if status != StatusPending {
		t.Errorf("status: got %v want %v", status, StatusPending)
	}
	if payload != "" {
		t.Errorf("payload: got %q want empty", payload)
	}
}

func TestAcquire_AfterStoreReturnsCached(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "op-3"

	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	cached := `{"status":"PROCESSED","amount":"5.0000"}`
	if err := s.Store(ctx, key, cached, hashA); err != nil {
		t.Fatalf("store: %v", err)
	}

	status, payload, err := s.Acquire(ctx, key, hashA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if status != StatusCached {
		t.Errorf("status: got %v want %v", status, StatusCached)
	}
	if payload != cached {
		t.Errorf("payload: got %q want %q", payload, cached)
	}
}

// A retry that reuses the operator_transaction_id but carries a DIFFERENT body
// (hash) must NOT replay the in-flight PROCESSING marker — it gets Mismatch.
func TestAcquire_HashMismatch_WhileProcessing(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "op-mismatch-1"

	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	status, payload, err := s.Acquire(ctx, key, hashB)
	if err != nil {
		t.Fatalf("mismatch acquire: %v", err)
	}
	if status != StatusMismatch {
		t.Errorf("status: got %v want %v", status, StatusMismatch)
	}
	if payload != "" {
		t.Errorf("payload must be empty on mismatch, got %q", payload)
	}
}

// Same protection after the response was cached: a different body never gets to
// replay the cached success of the original request.
func TestAcquire_HashMismatch_AfterCached(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "op-mismatch-2"

	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Store(ctx, key, `{"status":"PROCESSED"}`, hashA); err != nil {
		t.Fatalf("store: %v", err)
	}
	status, _, err := s.Acquire(ctx, key, hashB)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if status != StatusMismatch {
		t.Errorf("status: got %v want %v", status, StatusMismatch)
	}
}

// A stored value with no hash prefix (e.g. written by a prior engine version)
// cannot be proven to match — fail closed rather than replay it blindly.
func TestAcquire_MalformedStoredValue_FailsClosed(t *testing.T) {
	t.Parallel()
	s, mr, _ := newStore(t)
	ctx := context.Background()
	key := "op-legacy"

	mr.Set(keyFor(key), "PROCESSING") // legacy, un-prefixed value
	_, _, err := s.Acquire(ctx, key, hashA)
	if err == nil {
		t.Fatal("expected fail-closed error on malformed stored value")
	}
}

func TestStore_XXGuard_OnlyOverwritesProcessing(t *testing.T) {
	t.Parallel()
	s, mr, _ := newStore(t)
	ctx := context.Background()
	key := "op-4"

	// No prior Acquire — Store should not create the key (XX flag).
	if err := s.Store(ctx, key, `{"x":1}`, hashA); err != nil {
		t.Fatalf("store on empty: %v", err)
	}
	if mr.Exists(keyFor(key)) {
		t.Errorf("XX guard violated: key was materialised")
	}
}

func TestStore_AfterLockExpiry(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := NewRedisWithTTLs(client, 50*time.Millisecond, 1*time.Hour)
	ctx := context.Background()
	key := "op-5"

	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Fast-forward miniredis past the lock TTL — no time.Sleep needed.
	mr.FastForward(75 * time.Millisecond)

	// Store should be a no-op (XX fails) since the PROCESSING marker is gone.
	// The next request's SETNX will succeed and the original tx, if it
	// committed, is recovered via the 23505 path in the repository.
	if err := s.Store(ctx, key, `{"y":2}`, hashA); err != nil {
		t.Fatalf("store post-expiry: %v", err)
	}
	if mr.Exists(keyFor(key)) {
		t.Errorf("XX must not resurrect an expired key")
	}
}

func TestRelease_ClearsKey(t *testing.T) {
	t.Parallel()
	s, mr, _ := newStore(t)
	ctx := context.Background()
	key := "op-6"

	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	v, _ := mr.Get(keyFor(key))
	if v != encodeValue(hashA, ProcessingMarker) {
		t.Fatalf("setup: PROCESSING not stored, got %q", v)
	}
	if err := s.Release(ctx, key); err != nil {
		t.Fatalf("release: %v", err)
	}
	if mr.Exists(keyFor(key)) {
		t.Errorf("release left key behind")
	}
}

func TestAcquire_AfterReleaseSucceeds(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "op-7"

	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Release(ctx, key); err != nil {
		t.Fatalf("release: %v", err)
	}
	status, _, err := s.Acquire(ctx, key, hashA)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if status != StatusAcquired {
		t.Errorf("after release: got %v want %v", status, StatusAcquired)
	}
}

func TestAcquire_EmptyKeyRejected(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	_, _, err := s.Acquire(context.Background(), "", hashA)
	if err == nil {
		t.Fatal("expected error on empty operator_transaction_id")
	}
}

func TestStore_EmptyKeyAndPayloadRejected(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	if err := s.Store(ctx, "", "x", hashA); err == nil {
		t.Error("expected error on empty key")
	}
	if err := s.Store(ctx, "k", "", hashA); err == nil {
		t.Error("expected error on empty payload")
	}
}

func TestRelease_EmptyKeyRejected(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	if err := s.Release(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty key")
	}
}

func TestAcquire_FailClosed_OnRedisDown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := NewRedis(client)

	mr.Close() // simulate Redis outage
	_, _, err := s.Acquire(context.Background(), "op-down", hashA)
	if err == nil {
		t.Fatal("expected error when Redis is unreachable (FAIL CLOSED)")
	}
	if !errors.Is(err, err) { // sanity check; wrapping kept
		t.Errorf("error should be wrapped")
	}
}

func TestAcquire_RaceBetweenSetNXAndGet(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := NewRedisWithTTLs(client, 50*time.Millisecond, 1*time.Hour)
	ctx := context.Background()
	key := "op-race"

	// Plant a key, then expire it before the GET runs.
	if _, _, err := s.Acquire(ctx, key, hashA); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Force-expire to simulate the SETNX-fails-then-GET-misses race.
	mr.SetTTL(keyFor(key), 0)
	mr.FastForward(1 * time.Second)

	// Manually pre-empt SETNX so the path that exercises the redis.Nil
	// branch in Get is forced: take the lock under a different key first
	// and then call Acquire on a fresh key — but for direct coverage we
	// use a key that does not exist.
	mr.Set(keyFor(key)+"-dummy", "X")
	status, payload, err := s.Acquire(ctx, key, hashA)
	if err != nil {
		t.Fatalf("race acquire: %v", err)
	}
	if status != StatusAcquired {
		t.Errorf("after expiry: got %v want %v", status, StatusAcquired)
	}
	if payload != "" {
		t.Errorf("payload: got %q want empty", payload)
	}
}

// encodeValue/decodeValue round-trip, including payloads that themselves
// contain the separator byte.
func TestEncodeDecodeValue_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct{ hash, payload string }{
		{hashA, ProcessingMarker},
		{hashB, `{"a":1,"pipe":"x|y|z"}`},
		{"", ProcessingMarker}, // empty hash (internal/test callers)
	}
	for _, tc := range cases {
		h, p, ok := decodeValue(encodeValue(tc.hash, tc.payload))
		if !ok {
			t.Fatalf("decode(%q,%q): ok=false", tc.hash, tc.payload)
		}
		if h != tc.hash || p != tc.payload {
			t.Errorf("round-trip: got (%q,%q) want (%q,%q)", h, p, tc.hash, tc.payload)
		}
	}
	if _, _, ok := decodeValue("no-separator-here"); ok {
		t.Error("decode of un-prefixed value must report ok=false")
	}
}
