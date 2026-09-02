package game

// Gate B — structural enforcement of the currency-agnostic outcome layer.
//
// THE INVARIANT
//
// Outcome generation MUST NOT know which currency a spin is wagered in. The
// reels a player sees, the line that pays, and the multiplier it pays are drawn
// and evaluated identically whether the stake is Gold Coins or Sweeps Coins.
// Currency enters exactly one layer above, when the domain allocates the debit
// and the credit against the wallet.
//
// WHY THIS IS THE HIGHEST-PRIORITY INVARIANT IN THE CODEBASE
//
// A US sweepstakes operator's legal position rests on the promotional entry
// (GC) and the sweepstakes entry (SC) being the SAME GAME. If the engine paid
// SC even slightly differently from GC — a shorter reel strip, a trimmed
// multiplier, one extra losing symbol — then the promotional play is no longer
// a fair depiction of the sweepstakes play, and the sweepstakes is no longer a
// sweepstakes. That is not a bug that costs money; it is a bug that ends the
// licence. It is also exactly the kind of change that looks harmless in review:
// a `family` parameter threaded "just for logging" is one refactor away from a
// `if family == FamilySC` in the draw.
//
// WHAT ENFORCES IT
//
// Three independent mechanisms, deliberately overlapping, because a convention
// documented in prose is not an invariant:
//
//  1. THIS FILE — compile-time signature pins. The declarations below fail
//     `go build` if a currency parameter is ever added to a draw or evaluation
//     function. Not a test that can be skipped; the package stops compiling.
//
//  2. TestGamePackageIsCurrencyBlind (currency_agnostic_test.go) — asserts this
//     package imports nothing that defines a currency type, so the invariant
//     cannot be routed around by importing domain and reading a package-level
//     value instead of taking a parameter.
//
//  3. TestCurrencyModelIdentity (internal/repository) — replays one seeded RNG
//     stream through the GC and the SC paths and requires the outcomes to be
//     byte-identical, which is the property all of this exists to protect.
//
// The pins are `var _ func(...) = f` rather than a test because the failure
// should arrive at the moment the signature changes, in the compiler, in the
// editor of whoever changed it — not later, in CI, as a red test somebody might
// read as flaky.

import "github.com/shopspring/decimal"

// The currency-agnostic surface of this package, pinned.
//
// If you are here because one of these stopped compiling: you have added a
// parameter to a function that decides what a spin pays. Before changing the
// pin to match, be certain the new parameter cannot carry currency, family, or
// account information — directly or inside a struct. If it can, the change is
// the defect, not the pin.
//
// Deliberately NOT a compile-time assertion on Lookup: it takes a game id, and
// a game id legitimately selects a paytable. The protection there is that GC
// and SC resolve the SAME id, which TestCurrencyModelIdentity checks.
var (
	// Spin draws the reels. Paytable and RNG only — no family, no currency,
	// no wallet, no player.
	_ func(Paytable, RNG) (Outcome, error) = Spin

	// Evaluate scores already-drawn reels. Paytable and symbols only.
	_ func(Paytable, []string) (Outcome, error) = Evaluate

	// WinFor converts an outcome into a win amount. It takes the stake as a
	// bare decimal precisely so it cannot be handed a currency-tagged Money:
	// the conversion from stake to payout is pure arithmetic on the
	// multiplier, and the currency of that stake is not its business.
	_ func(decimal.Decimal, Outcome, int32) decimal.Decimal = WinFor
)
