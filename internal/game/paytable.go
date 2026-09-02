// Package game holds the server-authoritative game model: the reel strips,
// the paytable, the cryptographic RNG, and the pure outcome evaluator.
//
// SECURITY CONTRACT (the reason this package exists):
//
//	The player — and the game client — MUST NOT be able to influence the
//	outcome or the win amount. Before this package existed, `win_amount`
//	arrived over the wire from an external party and was booked to the
//	ledger verbatim, with no upper bound. Anyone holding a webhook secret
//	could mint redeemable currency.
//
//	Now the flow is inverted: the caller supplies ONLY a bet amount. The
//	server draws the reels from crypto/rand, evaluates them against a
//	version-pinned paytable, and derives the win itself. There is no wire
//	field a caller can set that reaches the credit amount.
//
// This package is I/O-free and deterministic given an RNG: no database, no
// HTTP, no clock. That is what makes the RTP model unit-testable and what
// lets an auditor re-derive the published return from the source.
package game

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Symbol is one reel face. Weight drives frequency on the strip; the two
// payout multipliers are applied to the player's stake.
//
// Payouts are decimal multipliers of the bet, never absolute amounts, so a
// paytable can never be read as "credit N coins" independent of the wager.
type Symbol struct {
	// ID is the stable wire identifier. Part of the audit record.
	ID string
	// Weight is this symbol's relative frequency on every reel strip.
	// Probability of landing it on one reel is Weight / TotalWeight.
	Weight int
	// PayThree multiplies the stake when all three reels show this symbol.
	PayThree decimal.Decimal
	// PayTwo multiplies the stake when reels 1 and 2 (only) show it.
	PayTwo decimal.Decimal
}

// Paytable is a complete, self-contained game math model. Everything needed
// to compute the theoretical return lives here, which is what makes
// TheoreticalRTP() an exact derivation rather than an estimate.
//
// IMMUTABILITY: a Paytable is never mutated after construction. Changing the
// math means publishing a NEW Version — the version is written into every
// ledger transaction's metadata so any historical spin can be re-evaluated
// against the exact model that produced it.
type Paytable struct {
	// GameID is the wire identifier operators and the ledger reference.
	GameID string
	// Version pins the math model. Bump on ANY change to symbols or payouts.
	Version string
	// DeclaredRTP is the published theoretical return. It is asserted against
	// the computed TheoreticalRTP() in CI, so a payout edit that moves the
	// real return without updating this constant fails the build.
	DeclaredRTP decimal.Decimal
	// Symbols is the reel strip definition, shared by all three reels.
	Symbols []Symbol
	// MaxWinMultiplier is the hard ceiling on any single spin's payout,
	// expressed as a multiple of the stake. It is enforced by the evaluator
	// as a belt-and-braces bound: even a corrupted paytable cannot emit a win
	// beyond this. Must be >= the largest PayThree.
	MaxWinMultiplier decimal.Decimal
}

// ReelCount is the number of reels evaluated per spin. Fixed at 3 for this
// model; the evaluator's line rules are written against it explicitly.
const ReelCount = 3

// TotalWeight is the denominator of every symbol probability.
func (p Paytable) TotalWeight() int {
	total := 0
	for _, s := range p.Symbols {
		total += s.Weight
	}
	return total
}

// Validate checks the structural invariants the evaluator relies on. Called
// at construction (and in tests) so a malformed table fails at boot rather
// than mid-spin.
func (p Paytable) Validate() error {
	if p.GameID == "" {
		return fmt.Errorf("paytable: empty game_id")
	}
	if p.Version == "" {
		return fmt.Errorf("paytable: empty version")
	}
	if len(p.Symbols) == 0 {
		return fmt.Errorf("paytable %s: no symbols", p.GameID)
	}
	if p.TotalWeight() <= 0 {
		return fmt.Errorf("paytable %s: total weight must be > 0", p.GameID)
	}
	if !p.MaxWinMultiplier.IsPositive() {
		return fmt.Errorf("paytable %s: max_win_multiplier must be > 0", p.GameID)
	}

	seen := make(map[string]struct{}, len(p.Symbols))
	for _, s := range p.Symbols {
		if s.ID == "" {
			return fmt.Errorf("paytable %s: symbol with empty id", p.GameID)
		}
		if _, dup := seen[s.ID]; dup {
			return fmt.Errorf("paytable %s: duplicate symbol %q", p.GameID, s.ID)
		}
		seen[s.ID] = struct{}{}

		if s.Weight <= 0 {
			return fmt.Errorf("paytable %s: symbol %s weight must be > 0", p.GameID, s.ID)
		}
		if s.PayThree.IsNegative() || s.PayTwo.IsNegative() {
			return fmt.Errorf("paytable %s: symbol %s has a negative payout", p.GameID, s.ID)
		}
		// The ceiling must actually bound the table, otherwise it is theatre.
		if s.PayThree.GreaterThan(p.MaxWinMultiplier) {
			return fmt.Errorf(
				"paytable %s: symbol %s pays %s which exceeds max_win_multiplier %s",
				p.GameID, s.ID, s.PayThree, p.MaxWinMultiplier)
		}
	}
	return nil
}

// TheoreticalRTP computes the exact expected return per unit staked, derived
// from the weights and payouts alone.
//
// For three independent reels sharing one strip, with p = weight/total:
//
//	E[return] = Σ_s [ p_s³ · PayThree_s ]            (three of a kind)
//	          + Σ_s [ p_s² · (1 - p_s) · PayTwo_s ]  (reels 1+2 only)
//
// The second term requires reel 3 to differ, which is why it carries the
// (1 - p_s) factor — a three-of-a-kind is already counted by the first term
// and must not be double-counted here.
//
// This is an exact rational computation (decimal, 28-digit division), not a
// simulation. The Monte-Carlo test in paytable_test.go independently
// confirms the RNG converges to this number.
func (p Paytable) TheoreticalRTP() decimal.Decimal {
	total := decimal.NewFromInt(int64(p.TotalWeight()))
	if total.IsZero() {
		return decimal.Zero
	}

	one := decimal.NewFromInt(1)
	rtp := decimal.Zero

	for _, s := range p.Symbols {
		prob := decimal.NewFromInt(int64(s.Weight)).Div(total)

		// Three of a kind: p³ · PayThree
		three := prob.Pow(decimal.NewFromInt(3)).Mul(s.PayThree)

		// Exactly two (reels 1 and 2, reel 3 differs): p² · (1-p) · PayTwo
		two := prob.Pow(decimal.NewFromInt(2)).
			Mul(one.Sub(prob)).
			Mul(s.PayTwo)

		rtp = rtp.Add(three).Add(two)
	}
	return rtp
}

// TheoreticalVariance computes the exact variance of the per-spin return
// multiplier X, derived from the weights and payouts alone.
//
// X is the multiplier a single spin returns per unit staked: PayThree_s for a
// three-of-a-kind, PayTwo_s for a leading pair, 0 otherwise. Those three events
// are mutually exclusive, so the second moment has the same shape as the first
// with the payouts squared:
//
//	E[X²] = Σ_s [ p_s³ · PayThree_s² ]
//	      + Σ_s [ p_s² · (1 - p_s) · PayTwo_s² ]
//
//	Var(X) = E[X²] − E[X]²          where E[X] = TheoreticalRTP()
//
// WHY THIS EXISTS. It is the input to the Monte-Carlo gate's tolerance. A
// convergence test needs a band, and a band picked by eye is a magic number
// that either hides real drift or flakes; σ/√n is the band the mathematics
// actually dictates. Deriving σ here — next to the RTP it belongs with, and
// exhaustively verified by TestTheoreticalVarianceMatchesExhaustive — keeps the
// gate's threshold a consequence of the paytable rather than a constant someone
// tuned until CI went quiet.
//
// Exact rational computation, like TheoreticalRTP: no sampling, no float.
func (p Paytable) TheoreticalVariance() decimal.Decimal {
	total := decimal.NewFromInt(int64(p.TotalWeight()))
	if total.IsZero() {
		return decimal.Zero
	}

	one := decimal.NewFromInt(1)
	second := decimal.Zero

	for _, s := range p.Symbols {
		prob := decimal.NewFromInt(int64(s.Weight)).Div(total)

		// Three of a kind: p³ · PayThree²
		three := prob.Pow(decimal.NewFromInt(3)).Mul(s.PayThree.Mul(s.PayThree))

		// Exactly two (reels 1 and 2, reel 3 differs): p² · (1-p) · PayTwo²
		two := prob.Pow(decimal.NewFromInt(2)).
			Mul(one.Sub(prob)).
			Mul(s.PayTwo.Mul(s.PayTwo))

		second = second.Add(three).Add(two)
	}

	mean := p.TheoreticalRTP()
	return second.Sub(mean.Mul(mean))
}

// stdDevPrecision is the number of decimal places carried through the square
// root. The variance itself is exact; only this root is approximate, and 24
// places is ~17 orders of magnitude finer than the tolerance it feeds.
const stdDevPrecision = 24

// TheoreticalStdDev returns σ = √Var(X), the per-spin standard deviation of the
// return multiplier.
//
// This is the ONLY inexact step in the derivation — a square root has no exact
// decimal representation in general. It is carried to stdDevPrecision places,
// which is far below any tolerance this feeds.
func (p Paytable) TheoreticalStdDev() (decimal.Decimal, error) {
	variance := p.TheoreticalVariance()
	if variance.IsNegative() {
		// Unreachable for a validated paytable: E[X²] ≥ E[X]² by Jensen. Report
		// it rather than return a NaN-shaped value if the model is ever broken.
		return decimal.Zero, fmt.Errorf("game: negative variance %s", variance)
	}
	root, err := variance.PowWithPrecision(decimal.New(5, -1), stdDevPrecision)
	if err != nil {
		return decimal.Zero, fmt.Errorf("game: variance square root: %w", err)
	}
	return root, nil
}

// SymbolByID returns the symbol with the given id.
func (p Paytable) SymbolByID(id string) (Symbol, bool) {
	for _, s := range p.Symbols {
		if s.ID == id {
			return s, true
		}
	}
	return Symbol{}, false
}

// ----------------------------------------------------------------------------
// The shipped game model.
// ----------------------------------------------------------------------------

func d(v string) decimal.Decimal { return decimal.RequireFromString(v) }

// ClassicThreeReel is the default first-party game.
//
// Weights sum to 100 so each symbol's probability reads directly as a
// percentage. Derived theoretical RTP is 95.6091% — see DeclaredRTP, which
// TestDeclaredRTPMatchesModel asserts against TheoreticalRTP() exactly.
//
//	symbol   weight   p       pay×3   pay×2
//	CHERRY   30       0.30    5       1
//	LEMON    25       0.25    10      1
//	BELL     20       0.20    20      2
//	DIAMOND  13       0.13    50      4
//	SEVEN     8       0.08    130     8
//	CROWN     4       0.04    400     15
var ClassicThreeReel = Paytable{
	GameID:      "classic-3reel",
	Version:     "1.0.0",
	DeclaredRTP: d("0.9560910"),
	// 400 is the largest single-spin multiplier the table can pay (CROWN ×3).
	// Holding the ceiling exactly there means the evaluator's clamp is a real
	// invariant, not slack.
	MaxWinMultiplier: d("400"),
	Symbols: []Symbol{
		{ID: "CHERRY", Weight: 30, PayThree: d("5"), PayTwo: d("1")},
		{ID: "LEMON", Weight: 25, PayThree: d("10"), PayTwo: d("1")},
		{ID: "BELL", Weight: 20, PayThree: d("20"), PayTwo: d("2")},
		{ID: "DIAMOND", Weight: 13, PayThree: d("50"), PayTwo: d("4")},
		{ID: "SEVEN", Weight: 8, PayThree: d("130"), PayTwo: d("8")},
		{ID: "CROWN", Weight: 4, PayThree: d("400"), PayTwo: d("15")},
	},
}

// registry maps game_id → paytable. Unexported so callers must go through
// Lookup, which cannot return an unvalidated table.
var registry = map[string]Paytable{
	ClassicThreeReel.GameID: ClassicThreeReel,
}

// Lookup returns the paytable for a game id. The bool is false for unknown
// games — callers MUST reject rather than fall back to a default, because a
// silent fallback would let a caller shop for a better-paying table.
func Lookup(gameID string) (Paytable, bool) {
	p, ok := registry[gameID]
	return p, ok
}

// DefaultGameID is used when a request omits game_id.
const DefaultGameID = "classic-3reel"

// init fails the process at boot if a shipped paytable is malformed. A bad
// table must never reach a player.
func init() {
	for id, p := range registry {
		if err := p.Validate(); err != nil {
			panic("game: invalid registered paytable " + id + ": " + err.Error())
		}
	}
}
