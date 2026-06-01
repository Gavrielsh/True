package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Gavrielsh/TransactionMechanism/pkg/errors"
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

// HMACVerifier holds the per-operator shared secrets used to verify the
// X-Signature header of inbound webhooks.
//
// SECURITY contract (architecture §5):
//  1. The signature MUST be computed over the RAW request body bytes,
//     BEFORE any unmarshal. Unmarshalling and re-marshalling alters JSON
//     whitespace and breaks the signature.
//  2. The hex-encoded HMAC-SHA256 is compared with subtle.ConstantTimeCompare
//     so signature comparison is not a timing oracle.
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

		// 1. Buffer the body with a hard cap. http.MaxBytesReader is the
		//    canonical Go way to bound a request body; it returns an error
		//    on the (maxBodyBytes+1)-th read so we don't OOM.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			respondErrorCode(c, http.StatusRequestEntityTooLarge,
				errors.Code("REQUEST_TOO_LARGE"), "request body exceeds size limit")
			return
		}
		// 2. Restore the body so handlers can read it (architecture §5 says
		//    "read body, restore via io.NopCloser(bytes.NewBuffer(body))").
		c.Request.Body = io.NopCloser(bytes.NewBuffer(body))

		// 3. Compute HMAC over the raw bytes. NEVER unmarshal-then-rehash.
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))

		// 4. Constant-time compare. Both inputs must be the same length —
		//    subtle.ConstantTimeCompare returns 0 for length mismatch but
		//    still in constant time wrt input contents.
		if !hmac.Equal([]byte(clientSig), []byte(expected)) {
			// hmac.Equal wraps subtle.ConstantTimeCompare with the
			// length-equal short-circuit; safe to use here.
			respondErrorCode(c, http.StatusUnauthorized, errors.Code("AUTHENTICATION_FAILED"), authFailMsg)
			return
		}

		// Trusted: stash the operator code for the handler.
		c.Set(ginKeyOperator, operator)
		c.Request = c.Request.WithContext(withOperator(c.Request.Context(), operator))
		c.Next()
	}
}
