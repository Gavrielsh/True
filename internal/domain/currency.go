package domain

// Currency mirrors the Postgres `currency_type` ENUM. Values must match
// the DB representation exactly so pgx can bind/scan without a custom codec.
type Currency string

const (
	CurrencyGC           Currency = "GC"
	CurrencySCUnplayed   Currency = "SC_UNPLAYED"
	CurrencySCRedeemable Currency = "SC_REDEEMABLE"
)

// Valid reports whether c is a known currency. Use at trust boundaries
// (e.g. when decoding a webhook body) before passing into allocators.
func (c Currency) Valid() bool {
	switch c {
	case CurrencyGC, CurrencySCUnplayed, CurrencySCRedeemable:
		return true
	}
	return false
}

// CurrencyFamily is the high-level wagering bucket the operator addresses
// at the API surface (GC or SC). Routing within a family is engine-enforced:
//
//   - Bet, SC family: consume SC_UNPLAYED first, overflow into SC_REDEEMABLE.
//   - Win, SC family: credit SC_REDEEMABLE exclusively (US sweepstakes rule).
//   - GC family: identity routing (single bucket).
//
// Keeping families distinct from raw currencies prevents the API from
// accidentally exposing the sub-bucket split — operators must not be able
// to "request" a bet from SC_REDEEMABLE only and skip the wagering rule.
type CurrencyFamily uint8

const (
	// FamilyUnknown is the zero value; explicit so we can detect uninitialized
	// receivers in allocator switches and return ErrUnsupportedCurrency.
	FamilyUnknown CurrencyFamily = 0
	FamilyGC      CurrencyFamily = 1
	FamilySC      CurrencyFamily = 2
)

func (f CurrencyFamily) String() string {
	switch f {
	case FamilyGC:
		return "GC"
	case FamilySC:
		return "SC"
	default:
		return "UNKNOWN"
	}
}

func (f CurrencyFamily) Valid() bool {
	return f == FamilyGC || f == FamilySC
}

// FamilyFromString parses the operator-facing wire value into a family.
// Used at the API decode boundary. Returns FamilyUnknown for any other input.
func FamilyFromString(s string) CurrencyFamily {
	switch s {
	case "GC":
		return FamilyGC
	case "SC":
		return FamilySC
	default:
		return FamilyUnknown
	}
}
