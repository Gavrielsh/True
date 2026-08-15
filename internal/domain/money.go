// Package domain holds the pure business rules for the seamless wallet
// engine: Money arithmetic, currency typing, wallet entities, and the bet/
// win allocation algorithms.
//
// This package is I/O-free. No file in it may import database/sql, redis,
// net/http, log/slog, or any driver. All inputs arrive as values; all
// outputs are values or errors. The repository layer (STEP 3) is the only
// place that talks to Postgres or Redis, and it calls into here exclusively
// inside the `SELECT ... FOR UPDATE` window — which means everything in
// this file must be allocation-light and predictable.
package domain

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

// MoneyScale is the fixed decimal precision used throughout the engine.
// It matches the DB column type NUMERIC(18,4) exactly so that a Money
// value can round-trip Postgres without truncation.
const MoneyScale int32 = 4

// MaxMoneyUnits is the largest whole-coin magnitude the ledger can store.
// NUMERIC(18,4) holds 18 significant digits with 4 after the point, so the
// integer part is bounded by 10^14.
//
// WHY THIS EXISTS: without it, an amount like "99999999999999999999" passes
// every application check (it is positive and has no fractional digits) and
// only fails at the UPDATE, as Postgres 22003 numeric_field_overflow — after
// the transaction has taken the wallet's FOR UPDATE lock and held it across
// several round-trips. That turns a malformed request into lock contention
// and an opaque 500. Bounding the magnitude here rejects it at the edge with
// a clean 400 instead.
//
// It is also load-bearing for the server-authoritative win cap: a payout is
// bet × multiplier, so an unbounded stake is an unbounded win no matter how
// tightly the paytable is capped.
const MaxMoneyUnits = 100_000_000_000_000 // 10^14

var maxMoneyDecimal = decimal.NewFromInt(MaxMoneyUnits)

// CheckAmountBound rejects amounts the ledger column cannot represent.
// Callers run this at request validation, before any lock is taken.
//
// Wraps ErrInvalidAmount so it maps to INVALID_AMOUNT / HTTP 400 through the
// existing error-code table — an out-of-range amount is a malformed request,
// not an internal fault.
func CheckAmountBound(m Money) error {
	if m.v.Abs().GreaterThanOrEqual(maxMoneyDecimal) {
		return fmt.Errorf("%w: amount %s exceeds maximum %d", errs.ErrInvalidAmount, m.String(), MaxMoneyUnits)
	}
	return nil
}

// ErrMoneyScaleExceeded is returned when an input decimal carries more than
// MoneyScale fractional digits. Silently truncating sub-cent fractions would
// violate the financial-precision invariant, so we reject instead.
var ErrMoneyScaleExceeded = errors.New("money: scale exceeds 4 decimal places")

// Money is a fixed-precision currency value. The zero value is the additive
// identity (0.0000) and is safe to use without construction. All arithmetic
// methods return new Money values; Money is immutable.
//
// Money has no nil/invalid state — once constructed it is always a valid
// decimal at scale 4. Comparisons across currencies are the caller's
// responsibility; Money carries amount, not unit.
type Money struct {
	v decimal.Decimal
}

// NewMoney constructs a Money from a decimal.Decimal, rejecting any value
// whose precision is finer than MoneyScale.
//
// We deliberately do NOT round. Silent rounding here would be a financial
// bug — the caller would think they bet 1.00005 SC, but the engine would
// debit 1.0000 SC and pocket the 0.00005 mismatch into rounding error over
// millions of spins. Force the caller to choose an explicit policy upstream
// (typically at the JSON-decode boundary).
func NewMoney(d decimal.Decimal) (Money, error) {
	if d.Exponent() < -MoneyScale {
		return Money{}, fmt.Errorf("%w: got %s", ErrMoneyScaleExceeded, d.String())
	}
	// Truncate stamps the canonical exponent (-4) without changing the value,
	// so that Equal and String are deterministic across construction paths.
	return Money{v: d.Truncate(MoneyScale)}, nil
}

// MoneyFromString parses a decimal string ("12.3400") into Money.
// Convenience wrapper over decimal.NewFromString + NewMoney.
func MoneyFromString(s string) (Money, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Money{}, fmt.Errorf("money: parse %q: %w", s, err)
	}
	return NewMoney(d)
}

// MoneyFromInt constructs a Money from a whole-unit integer (e.g. 5 → 5.0000).
// Used by the repository layer to convert DB INTEGER columns or by tests.
func MoneyFromInt(n int64) Money {
	return Money{v: decimal.NewFromInt(n)}
}

// ZeroMoney is the additive identity. Equivalent to the zero value.
func ZeroMoney() Money { return Money{} }

// Decimal returns the underlying value at the canonical 4-decimal scale.
// Used by the repository layer to bind into pgx parameters.
func (m Money) Decimal() decimal.Decimal { return m.v.Truncate(MoneyScale) }

// String renders the value at fixed 4-decimal precision: "12.3400".
func (m Money) String() string { return m.v.StringFixed(MoneyScale) }

func (m Money) Add(o Money) Money        { return Money{v: m.v.Add(o.v)} }
func (m Money) Sub(o Money) Money        { return Money{v: m.v.Sub(o.v)} }
func (m Money) IsZero() bool             { return m.v.IsZero() }
func (m Money) IsPositive() bool         { return m.v.Sign() > 0 }
func (m Money) IsNegative() bool         { return m.v.Sign() < 0 }
func (m Money) Equal(o Money) bool       { return m.v.Equal(o.v) }
func (m Money) LessThan(o Money) bool    { return m.v.LessThan(o.v) }
func (m Money) GreaterThan(o Money) bool { return m.v.GreaterThan(o.v) }
func (m Money) GTE(o Money) bool         { return m.v.GreaterThanOrEqual(o.v) }
func (m Money) LTE(o Money) bool         { return m.v.LessThanOrEqual(o.v) }

// MinMoney returns the smaller of a and b. Used by the SC allocator to
// clamp the SC_UNPLAYED draw to the available unplayed balance.
func MinMoney(a, b Money) Money {
	if a.LessThan(b) {
		return a
	}
	return b
}

// MarshalJSON renders Money as a JSON string ("12.3400") so the precision
// survives a round-trip through any JSON parser (JS numbers lose precision
// above 15 significant digits). Used by the repository's response cache.
func (m Money) MarshalJSON() ([]byte, error) {
	s := m.String()
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	out = append(out, s...)
	out = append(out, '"')
	return out, nil
}

// UnmarshalJSON parses the string form ("12.3400") back into Money.
func (m *Money) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return fmt.Errorf("money: expected JSON string, got %s", data)
	}
	parsed, err := MoneyFromString(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
