package api

// auth_middleware.go — Unified authentication + replay-protection stack.
//
// This file composes the two security layers (hmac.go + replay.go) into a
// single entry-point so the router only needs one call site.
//
// Security contract (Architecture §5):
//
//  Layer 1 — HMACVerifier (hmac.go)
//    • Reads and caps the raw request body (max 64 KiB).
//    • Computes HMAC-SHA256 over the raw bytes with the operator's secret.
//    • Constant-time comparison (hmac.Equal wraps subtle.ConstantTimeCompare).
//    • Stashes the verified operator code on gin.Context and context.Context.
//    • HTTP 401 on any failure (uniform error — no oracle on which check failed).
//
//  Layer 2 — ReplayGuard (replay.go)
//    • Validates X-Timestamp is within ±5 min (30 s clock-skew tolerance).
//    • Atomically sets X-Nonce in Redis (SETNX, TTL 10 min) — already-seen
//      nonces return HTTP 401 REPLAY_DETECTED.
//    • Scopes the nonce key to the verified operator (from Layer 1 context).
//    • FAIL CLOSED: Redis unavailable → HTTP 500 (not 200, not 401).
//
// Ordering invariant: HMAC MUST precede ReplayGuard.
//   Rationale: the nonce key is scoped to the *verified* operator identity.
//   If ReplayGuard ran first, an unauthenticated caller could burn another
//   operator's nonce budget by sending arbitrary X-Nonce values.

import (
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// AuthMiddlewareStack returns the ordered slice of gin.HandlerFunc that
// enforces the full two-layer authentication contract.
//
// Register it on a route group with:
//
//	v1 := r.Group("/api/v1")
//	v1.Use(AuthMiddlewareStack(secrets, redisClient)...)
func AuthMiddlewareStack(secrets map[string]string, redisClient redis.UniversalClient) []gin.HandlerFunc {
	hmacVerifier := NewHMACVerifier(secrets)
	replayGuard := NewReplayGuard(redisClient)

	return []gin.HandlerFunc{
		hmacVerifier.Middleware(), // Layer 1: authenticate & body-buffer
		replayGuard.Middleware(),  // Layer 2: timestamp + nonce dedup
	}
}
