package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/Gavrielsh/True/pkg/errors"
)

// signHMAC computes HMAC-SHA256(secret, payload).
func signHMAC(secret, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return mac.Sum(nil)
}

// Headers consumed by the HMAC middleware. Names are case-insensitive at
// the HTTP layer but we use the canonical capitalisation in tests.
const (
	HeaderOperatorCode = "X-Operator-Code"
	HeaderSignature    = "X-Signature"
)

// maxBodyBytes caps the raw request body the middleware will buffer for
// hashing. 64 KiB is comfortably above any legitimate webhook payload
// (the largest documented Pragmatic /win is ~2 KiB) and protects against
// an attacker streaming GBs to exhaust memory.
const maxBodyBytes = 64 * 1024

// maxPooledBodyBuf bounds the capacity of a buffer kept in hmacBodyPool. A
// buffer that had to grow past this (only near maxBodyBytes) is discarded
// rather than pinning tens of KiB per worker for the process lifetime.
const maxPooledBodyBuf = maxBodyBytes

// hmacBodyPool recycles the buffers used to stage the raw request body for
// signature verification. On the 50k-TPS hot path a fresh io.ReadAll per
// request is pure GC pressure; pooling the backing array amortises it to near
// zero. A buffer is returned to the pool only AFTER the downstream chain
// (c.Next) has finished reading the restored body — every handler in this
// service consumes the body synchronously within the request, so the staged
// bytes are never aliased beyond it.
var hmacBodyPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// HMACVerifier holds the per-operator shared secrets used to verify the
// X-Signature header of inbound webhooks.
//
// SECURITY contract (architecture §5):
//  1. The signature is computed over a CANONICAL STRING that binds the raw
//     body to the replay headers — see canonicalPayload. The body portion is
//     the RAW bytes, hashed BEFORE any unmarshal: unmarshalling and
//     re-marshalling alters JSON whitespace and breaks the signature.
//  2. The client X-Signature is hex-DECODED to raw bytes and compared to the
//     computed HMAC-SHA256 with subtle.ConstantTimeCompare. Comparing decoded
//     bytes (not hex strings) is both case-insensitive — "AB" and "ab" are the
//     same MAC — and free of timing-oracle leakage.
//  3. On verification failure the response is HTTP 401 with code
//     "AUTHENTICATION_FAILED" — no further detail is leaked (don't
//     tell an attacker which check failed).
//
// WHY THE CANONICAL STRING IS THE ONLY ACCEPTED FORM (audit finding):
//
//	The signature once covered ONLY the body. X-Timestamp and X-Nonce were
//	then validated by ReplayGuard as unauthenticated headers — which meant
//	they protected nothing. An attacker who captured one valid request could
//	replay it forever: reuse the body and its still-valid signature, attach a
//	fresh timestamp and a brand-new random nonce, and every check passed. The
//	nonce could not detect the replay because the ATTACKER chose the nonce.
//
//	Binding all three into the signed material makes the freshness window and
//	the single-use nonce real: an attacker cannot re-sign a new timestamp or
//	nonce without the operator secret, and the captured pair is bound to the
//	instant it was issued.
//
//	The body-only form is NOT accepted, and there is deliberately no option,
//	flag, or environment variable to re-admit it. config.Load refuses to boot
//	if the retired HMAC_ACCEPT_LEGACY_SIGNATURE switch is still set to a
//	truthy value, so a stale manifest fails loudly at deploy time instead of
//	quietly running without replay protection.
type HMACVerifier struct {
	secrets map[string][]byte
}

// NewHMACVerifier builds a verifier from a map of operator code to shared
// secret. The secret strings are copied to []byte once and never logged.
func NewHMACVerifier(secrets map[string]string) *HMACVerifier {
	out := make(map[string][]byte, len(secrets))
	for k, v := range secrets {
		out[k] = []byte(v)
	}
	return &HMACVerifier{secrets: out}
}

// canonicalPayload builds the exact byte string that is signed:
//
//	<X-Timestamp> "." <X-Nonce> "." <raw body>
//
// The two dot separators are unambiguous because neither a unix-seconds
// timestamp nor a nonce may contain a dot — ReplayGuard enforces the
// timestamp shape, and nonceIsWellFormed enforces the nonce's. Without that
// guarantee the concatenation would be ambiguous and an attacker could shift
// bytes between fields while keeping the same digest.
func canonicalPayload(timestamp, nonce string, body []byte) []byte {
	out := make([]byte, 0, len(timestamp)+len(nonce)+len(body)+2)
	out = append(out, timestamp...)
	out = append(out, '.')
	out = append(out, nonce...)
	out = append(out, '.')
	out = append(out, body...)
	return out
}

// nonceIsWellFormed keeps the canonical string unambiguous: the nonce must be
// non-empty and free of the '.' separator. Rejecting here (rather than in
// ReplayGuard) means a malformed nonce can never produce a signature that
// validates against a DIFFERENT field split.
func nonceIsWellFormed(nonce string) bool {
	if nonce == "" || len(nonce) > 128 {
		return false
	}
	return !strings.Contains(nonce, ".")
}

// timestampIsWellFormed mirrors nonceIsWellFormed for the timestamp field:
// unix seconds, digits only. Freshness is still ReplayGuard's job.
func timestampIsWellFormed(ts string) bool {
	if ts == "" || len(ts) > 20 {
		return false
	}
	for _, r := range ts {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Middleware returns the gin.HandlerFunc enforcing the HMAC contract above.
//
// On success the verified operator code is stashed on both gin.Context and
// the request's context.Context (via the operatorCodeKey type) so downstream
// handlers consume it from the trusted source instead of trusting the
// (operator-supplied) HTTP header again.
func (v *HMACVerifier) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		operator := c.GetHeader(HeaderOperatorCode)
		// Single error code on every auth failure so attackers can't
		// distinguish "unknown operator" from "wrong signature" by
		// response shape or status.
		const authFailMsg = "authentication failed"

		secret, known := v.secrets[operator]
		if !known {
			respondErrorCode(c, http.StatusUnauthorized, errors.Code("AUTHENTICATION_FAILED"), authFailMsg)
			return
		}

		clientSig := c.GetHeader(HeaderSignature)
		if clientSig == "" {
			respondErrorCode(c, http.StatusUnauthorized, errors.Code("AUTHENTICATION_FAILED"), authFailMsg)
			return
		}

		// The replay headers are part of the SIGNED material now, so they must
		// be present and well-formed BEFORE the MAC is computed. ReplayGuard
		// still owns freshness and single-use; this is purely about making the
		// canonical string unambiguous.
		timestamp := c.GetHeader(HeaderTimestamp)
		nonce := c.GetHeader(HeaderNonce)
		if !timestampIsWellFormed(timestamp) || !nonceIsWellFormed(nonce) {
			respondErrorCode(c, http.StatusUnauthorized, errors.Code("AUTHENTICATION_FAILED"), authFailMsg)
			return
		}

		// 1. Stage the body into a POOLED buffer (no per-request io.ReadAll
		//    allocation). http.MaxBytesReader bounds it so a hostile stream
		//    can't OOM us — ReadFrom surfaces its error past maxBodyBytes.
		buf := hmacBodyPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer func() {
			// Returned only after c.Next() below has consumed the restored body.
			if buf.Cap() <= maxPooledBodyBuf {
				hmacBodyPool.Put(buf)
			}
		}()

		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		if _, err := buf.ReadFrom(c.Request.Body); err != nil {
			respondErrorCode(c, http.StatusRequestEntityTooLarge,
				errors.Code("REQUEST_TOO_LARGE"), "request body exceeds size limit")
			return
		}
		body := buf.Bytes()

		// 2. Restore the body so handlers can read it (architecture §5). The
		//    reader aliases the pooled buffer; safe because the buffer is not
		//    recycled until this request's chain completes (see pool docs).
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		// 3. Compute HMAC over the CANONICAL STRING (timestamp.nonce.body).
		//    The body portion is the raw bytes — NEVER unmarshal-then-rehash.
		expectedMAC := signHMAC(secret, canonicalPayload(timestamp, nonce, body))

		// 4. Decode the client signature from hex to RAW BYTES, then compare
		//    with subtle.ConstantTimeCompare. Decoding (rather than comparing
		//    hex strings) makes the check case-insensitive — a valid signature
		//    in upper/mixed-case hex is accepted — while staying constant-time
		//    wrt content. A non-hex signature fails closed.
		clientMAC, err := hex.DecodeString(clientSig)
		if err != nil {
			respondErrorCode(c, http.StatusUnauthorized, errors.Code("AUTHENTICATION_FAILED"), authFailMsg)
			return
		}

		if subtle.ConstantTimeCompare(clientMAC, expectedMAC) != 1 {
			respondErrorCode(c, http.StatusUnauthorized, errors.Code("AUTHENTICATION_FAILED"), authFailMsg)
			return
		}

		// Trusted. Stash the operator code and the body hash — the latter binds
		// the idempotency key to this exact request (see cache.Fingerprint), so
		// a key replayed with a different body is refused instead of returning
		// the first request's cached result.
		bodyHash := sha256.Sum256(body)
		bodyHashHex := hex.EncodeToString(bodyHash[:])

		c.Set(ginKeyOperator, operator)
		c.Set(ginKeyBodyHash, bodyHashHex)
		ctx := withOperator(c.Request.Context(), operator)
		ctx = withBodyHash(ctx, bodyHashHex)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
