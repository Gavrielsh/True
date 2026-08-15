package game

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrRNGRange is returned when a caller asks for a value in an empty range.
var ErrRNGRange = errors.New("game/rng: n must be > 0")

// RNG is the randomness seam. Production uses CryptoRNG (crypto/rand);
// tests inject a deterministic sequence so outcomes are reproducible without
// weakening the production path.
//
// Uint64N returns a uniformly distributed value in [0, n).
type RNG interface {
	Uint64N(n uint64) (uint64, error)
}

// CryptoRNG draws from a cryptographically secure entropy source.
//
// WHY NOT math/rand: math/rand (and math/rand/v2) are deterministic PRNGs.
// Given a handful of observed outcomes an attacker can recover the internal
// state and predict every subsequent spin — the canonical way slot systems
// get robbed. Game outcomes therefore MUST come from a CSPRNG. The only
// math/rand use left in this repository is the rate limiter's ZADD member,
// where the value is a uniqueness tag and carries no security weight.
type CryptoRNG struct {
	// src defaults to crypto/rand.Reader. Overridable so tests can drive
	// rejection-sampling edge cases deterministically.
	src io.Reader
}

// NewCryptoRNG returns an RNG backed by crypto/rand.Reader.
func NewCryptoRNG() *CryptoRNG { return &CryptoRNG{src: rand.Reader} }

// NewCryptoRNGWithSource returns an RNG reading from src. Test-only seam —
// production always uses NewCryptoRNG.
func NewCryptoRNGWithSource(src io.Reader) *CryptoRNG { return &CryptoRNG{src: src} }

// Uint64N returns a uniformly distributed value in [0, n).
//
// MODULO BIAS: the naive `read() % n` is NOT uniform unless n divides 2^64.
// With n = 100 (our reel strip weight), the low residues occur marginally
// more often — a bias small enough to be invisible in testing and large
// enough to shift RTP over millions of spins, in the house's favour or the
// player's depending on where the heavy symbols sit. Either direction is a
// certification failure.
//
// We remove it by rejection sampling: values in the final, incomplete block
// of size (2^64 mod n) are discarded and redrawn. The expected number of
// draws is < 2 for any n, and for our n = 100 the rejection zone is 16 of
// 2^64 values — effectively never taken, but correct when it is.
func (r *CryptoRNG) Uint64N(n uint64) (uint64, error) {
	if n == 0 {
		return 0, ErrRNGRange
	}
	// A power of two divides 2^64 exactly, so masking is already uniform and
	// no rejection is needed.
	if n&(n-1) == 0 {
		v, err := r.read()
		if err != nil {
			return 0, err
		}
		return v & (n - 1), nil
	}

	// In unsigned arithmetic -n wraps to 2^64 - n, so (-n) % n == 2^64 % n.
	// That is exactly the size of the trailing incomplete block: any draw
	// below it would over-represent the low residues, so we redraw.
	threshold := (-n) % n

	for attempt := 0; attempt < maxRNGAttempts; attempt++ {
		v, err := r.read()
		if err != nil {
			return 0, err
		}
		if v >= threshold {
			return v % n, nil
		}
	}
	// Statistically unreachable (probability < 2^-1000 for any realistic n);
	// failing loudly beats looping forever or silently biasing the result.
	return 0, fmt.Errorf("game/rng: rejection sampling failed after %d attempts", maxRNGAttempts)
}

// maxRNGAttempts bounds the rejection loop so a pathological entropy source
// can never hang a request holding a wallet lock.
const maxRNGAttempts = 128

// read pulls 8 bytes of entropy and decodes them as a uint64.
//
// A short read or an error from the entropy source is FATAL to the spin: we
// return the error and the caller aborts. Never fall back to a weaker source
// — a spin that cannot be drawn securely must not be drawn at all.
func (r *CryptoRNG) read() (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r.src, buf[:]); err != nil {
		return 0, fmt.Errorf("game/rng: entropy source: %w", err)
	}
	return binary.BigEndian.Uint64(buf[:]), nil
}
