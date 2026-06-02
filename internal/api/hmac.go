package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/Gavrielsh/True/pkg/errors"
)

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
//  1. The signature MUST be computed over the RAW request body bytes,
//     BEFORE any unmarshal. Unmarshalling and re-marshalling alters JSON
//     whitespace and breaks the signature.
//  2. The client X-Signature is hex-DECODED to raw bytes and compared to the
//     computed HMAC-SHA256 with subtle.ConstantTimeCompare. Comparing decoded
//     bytes (not hex strings) is both case-insensitive — "AB" and "ab" are the
//     same MAC — and free of timing-oracle leakage.
//  3. On verification failure the response is HTTP 401 with code
//     "AUTHENTICATION_FAILED" — no further detail is leaked (don't
//     tell an attacker which check failed).
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

		// 3. Compute HMAC over the raw bytes. NEVER unmarshal-then-rehash.
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		expectedMAC := mac.Sum(nil)

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

		// Trusted: stash the operator code for the handler.
		c.Set(ginKeyOperator, operator)
		c.Request = c.Request.WithContext(withOperator(c.Request.Context(), operator))
		c.Next()
	}
}
