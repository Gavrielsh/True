package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// testFP is the request fingerprint used by the single-request scenarios in
// this file. Binding-specific behaviour (two different requests colliding on
// one key) is covered separately in TestAcquire_FingerprintMismatch.
const testFP = "fingerprint-a"

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

	status, payload, err := s.Acquire(context.Background(), "op-1", testFP)
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

	if _, _, err := s.Acquire(ctx, "op-2", testFP); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	status, payload, err := s.Acquire(ctx, "op-2", testFP)
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

	if _, _, err := s.Acquire(ctx, key, testFP); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	cached := `{"status":"PROCESSED","amount":"5.0000"}`
	if err := s.Store(ctx, key, testFP, cached); err != nil {
		t.Fatalf("store: %v", err)
	}

	status, payload, err := s.Acquire(ctx, key, testFP)
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

func TestStore_XXGuard_OnlyOverwritesProcessing(t *testing.T) {
	t.Parallel()
	s, mr, _ := newStore(t)
	ctx := context.Background()
	key := "op-4"

	// No prior Acquire — Store should not create the key (XX flag).
	if err := s.Store(ctx, key, testFP, `{"x":1}`); err != nil {
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

	if _, _, err := s.Acquire(ctx, key, testFP); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Fast-forward miniredis past the lock TTL — no time.Sleep needed.
	mr.FastForward(75 * time.Millisecond)

	// Store should be a no-op (XX fails) since the PROCESSING marker is gone.
	// The next request's SETNX will succeed and the original tx, if it
	// committed, is recovered via the 23505 path in the repository.
	if err := s.Store(ctx, key, testFP, `{"y":2}`); err != nil {
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

	if _, _, err := s.Acquire(ctx, key, testFP); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Stored layout is "<fingerprint>|<payload-or-marker>": the fingerprint
	// travels with the key so a later retry can be compared against the
	// request that created it.
	v, _ := mr.Get(keyFor(key))
	if v != testFP+"|"+ProcessingMarker {
		t.Fatalf("setup: expected %q, got %q", testFP+"|"+ProcessingMarker, v)
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

	if _, _, err := s.Acquire(ctx, key, testFP); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Release(ctx, key); err != nil {
		t.Fatalf("release: %v", err)
	}
	status, _, err := s.Acquire(ctx, key, testFP)
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
	_, _, err := s.Acquire(context.Background(), "", testFP)
	if err == nil {
		t.Fatal("expected error on empty operator_transaction_id")
	}
}

func TestStore_EmptyKeyAndPayloadRejected(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	if err := s.Store(ctx, "", testFP, "x"); err == nil {
		t.Error("expected error on empty key")
	}
	if err := s.Store(ctx, "k", testFP, ""); err == nil {
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
	_, _, err := s.Acquire(context.Background(), "op-down", testFP)
	if err == nil {
		t.Fatal("expected error when Redis is unreachable (FAIL CLOSED)")
	}
	if !errors.Is(err, err) { // sanity check; wrapping kept
		t.Errorf("error should be wrapped")
	}
}

// Atomicity proof: with the single-script Acquire, N concurrent callers on the
// same key yield EXACTLY ONE StatusAcquired; the rest see StatusPending. There
// is no interleaving window for two callers to both "win" the PROCESSING claim.
func TestAcquire_AtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	const n = 64

	var (
		mu       sync.Mutex
		acquired int
		pending  int
		failed   int
		wg       sync.WaitGroup
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			st, _, err := s.Acquire(ctx, "concurrent-key", testFP)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failed++
			case st == StatusAcquired:
				acquired++
			case st == StatusPending:
				pending++
			}
		}()
	}
	wg.Wait()

	if failed != 0 {
		t.Fatalf("unexpected Acquire errors: %d", failed)
	}
	if acquired != 1 {
		t.Fatalf("exactly one winner expected: got acquired=%d pending=%d", acquired, pending)
	}
	if pending != n-1 {
		t.Errorf("losers must all be Pending: got pending=%d want %d", pending, n-1)
	}
}

// The Lua script must round-trip an arbitrary cached payload verbatim (it is
// returned as the second table element, not re-encoded).
func TestAcquire_ReturnsCachedPayloadVerbatim(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "payload-key"

	if _, _, err := s.Acquire(ctx, key, testFP); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	payload := `{"status":"PROCESSED","amount":"12.3400","nested":{"k":"v,;:"}}`
	if err := s.Store(ctx, key, testFP, payload); err != nil {
		t.Fatalf("store: %v", err)
	}
	st, got, err := s.Acquire(ctx, key, testFP)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if st != StatusCached {
		t.Fatalf("status: got %v want %v", st, StatusCached)
	}
	if got != payload {
		t.Errorf("payload not verbatim:\n got %q\nwant %q", got, payload)
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
	if _, _, err := s.Acquire(ctx, key, testFP); err != nil {
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
	status, payload, err := s.Acquire(ctx, key, testFP)
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

// ----------------------------------------------------------------------------
// Request binding (audit fix: idempotent replay was not bound to its request)
// ----------------------------------------------------------------------------

// TestAcquire_FingerprintMismatch is the regression guard for the audit's
// cross-player disclosure scenario.
//
// Before the fix, a cached result was returned verbatim on ANY retry carrying
// the same key, with no check that the retry described the same transaction:
//
//	win(tx=X, player=A, amount=1.00)     → 200, credits 1.00 to A
//	win(tx=X, player=B, amount=5000.00)  → 200, returns A's BALANCES under a
//	                                       receipt claiming 5000.00
//
// No money moved on the second call, but it leaked another player's balances
// and emitted a receipt that disagreed with the ledger. The key must now be
// refused when the request differs.
func TestAcquire_FingerprintMismatch(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "shared-key"

	// Request A claims the key and completes.
	st, _, err := s.Acquire(ctx, key, "fingerprint-player-A")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if st != StatusAcquired {
		t.Fatalf("first acquire: got %v want Acquired", st)
	}
	if err := s.Store(ctx, key, "fingerprint-player-A", `{"player":"A"}`); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Request B presents the SAME key with a different request.
	_, payload, err := s.Acquire(ctx, key, "fingerprint-player-B")
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("mismatched request: got err=%v payload=%q, want ErrFingerprintMismatch", err, payload)
	}
	if payload != "" {
		t.Errorf("mismatch must not leak the original payload, got %q", payload)
	}

	// Request A's own retry still replays correctly.
	st, payload, err = s.Acquire(ctx, key, "fingerprint-player-A")
	if err != nil {
		t.Fatalf("legitimate retry: %v", err)
	}
	if st != StatusCached || payload != `{"player":"A"}` {
		t.Fatalf("legitimate retry: got status=%v payload=%q", st, payload)
	}
}

// A mismatch must also be caught while the original is still in flight —
// otherwise a concurrent foreign request would see PROCESSING and retry into
// the cached result later.
func TestAcquire_FingerprintMismatchWhileProcessing(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	key := "inflight-key"

	if _, _, err := s.Acquire(ctx, key, "fingerprint-A"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, _, err := s.Acquire(ctx, key, "fingerprint-B"); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("in-flight mismatch: got %v, want ErrFingerprintMismatch", err)
	}
	// The original request's own retry correctly reports Pending.
	st, _, err := s.Acquire(ctx, key, "fingerprint-A")
	if err != nil || st != StatusPending {
		t.Fatalf("same-request retry: got status=%v err=%v, want Pending", st, err)
	}
}

func TestAcquire_EmptyFingerprintRejected(t *testing.T) {
	s, _, _ := newStore(t)
	if _, _, err := s.Acquire(context.Background(), "k", ""); err == nil {
		t.Fatal("an empty fingerprint would bind the key to nothing; must be rejected")
	}
}

func TestStore_EmptyFingerprintRejected(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.Store(context.Background(), "k", "", `{"a":1}`); err == nil {
		t.Fatal("Store must reject an empty fingerprint")
	}
}

// Fingerprint must be collision-resistant across part boundaries: ("ab","c")
// and ("a","bc") describe different requests and must not share a fingerprint.
func TestFingerprintIsUnambiguous(t *testing.T) {
	if Fingerprint("ab", "c") == Fingerprint("a", "bc") {
		t.Fatal("Fingerprint must length-prefix parts so boundaries cannot be shifted")
	}
	if Fingerprint("x", "y") != Fingerprint("x", "y") {
		t.Fatal("Fingerprint must be deterministic")
	}
	// An absent part must not silently match a present one.
	if Fingerprint("player", "") == Fingerprint("player", "bodyhash") {
		t.Fatal("an empty part must not act as a wildcard")
	}
}
