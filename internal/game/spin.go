package game

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// LineResult classifies what the reels paid.
type LineResult string

const (
	// LineNone: no paying combination.
	LineNone LineResult = "NONE"
	// LineTwo: reels 1 and 2 match, reel 3 differs.
	LineTwo LineResult = "TWO_OF_A_KIND"
	// LineThree: all three reels match.
	LineThree LineResult = "THREE_OF_A_KIND"
)

// Outcome is the complete, auditable record of one spin. It is written to
// the ledger transaction's metadata so any historical spin can be replayed
// against the paytable version that produced it and independently verified.
type Outcome struct {
	GameID          string     `json:"game_id"`
	PaytableVersion string     `json:"paytable_version"`
	Reels           []string   `json:"reels"`
	Line            LineResult `json:"line"`
	// WinSymbol is the paying symbol, empty when Line is LineNone.
	WinSymbol string `json:"win_symbol,omitempty"`
	// Multiplier is the stake multiple this outcome pays (0 for a loss).
	Multiplier decimal.Decimal `json:"multiplier"`
}

// IsWin reports whether the outcome pays anything.
func (o Outcome) IsWin() bool { return o.Multiplier.IsPositive() }

// Spin draws ReelCount symbols from the paytable's strip using rng and
// evaluates the line. Pure apart from the RNG: given the same draws it
// always produces the same Outcome.
//
// The draw is weighted: each reel picks a uniform value in [0, TotalWeight)
// and walks the symbol list accumulating weights. That makes the symbol
// probability exactly Weight/TotalWeight, which is the assumption
// TheoreticalRTP() is built on — the two must stay in lock-step, and the
// Monte-Carlo test is what keeps them honest.
func Spin(p Paytable, rng RNG) (Outcome, error) {
	if err := p.Validate(); err != nil {
		return Outcome{}, err
	}
	//nolint:gosec // G115: TotalWeight sums the paytable's symbol weights, which
	// Validate() constrains to small positive integers, so the sum is far below the
	// int64 range and the conversion cannot wrap.
	total := uint64(p.TotalWeight())

	reels := make([]string, ReelCount)
	for i := 0; i < ReelCount; i++ {
		roll, err := rng.Uint64N(total)
		if err != nil {
			return Outcome{}, fmt.Errorf("spin: draw reel %d: %w", i+1, err)
		}
		sym, err := symbolForRoll(p, roll)
		if err != nil {
			return Outcome{}, err
		}
		reels[i] = sym.ID
	}
	return Evaluate(p, reels)
}

// symbolForRoll maps a weighted roll in [0, TotalWeight) to its symbol.
func symbolForRoll(p Paytable, roll uint64) (Symbol, error) {
	var acc uint64
	for _, s := range p.Symbols {
		//nolint:gosec // G115: as above, a validated small positive weight.
		acc += uint64(s.Weight)
		if roll < acc {
			return s, nil
		}
	}
	// Unreachable while roll < TotalWeight, which Uint64N guarantees.
	return Symbol{}, fmt.Errorf("spin: roll %d out of range for total weight %d", roll, p.TotalWeight())
}

// Evaluate scores an already-drawn set of reels. Split out from Spin so the
// paytable can be scored against fixed reels in tests, and so a historical
// spin can be re-verified from its recorded Outcome without an RNG.
//
// Line rules (deliberately explicit, not generalised):
//   - all three equal          → PayThree for that symbol
//   - reels 1 and 2 equal only → PayTwo for that symbol
//   - anything else            → no win
//
// Note that reels 2+3 matching pays nothing: the line is read left-to-right
// from reel 1, which is the standard convention and is what TheoreticalRTP()
// models.
func Evaluate(p Paytable, reels []string) (Outcome, error) {
	if len(reels) != ReelCount {
		return Outcome{}, fmt.Errorf("spin: expected %d reels, got %d", ReelCount, len(reels))
	}

	out := Outcome{
		GameID:          p.GameID,
		PaytableVersion: p.Version,
		Reels:           reels,
		Line:            LineNone,
		Multiplier:      decimal.Zero,
	}

	switch {
	case reels[0] == reels[1] && reels[1] == reels[2]:
		sym, ok := p.SymbolByID(reels[0])
		if !ok {
			return Outcome{}, fmt.Errorf("spin: unknown symbol %q", reels[0])
		}
		out.Line = LineThree
		out.WinSymbol = sym.ID
		out.Multiplier = sym.PayThree

	case reels[0] == reels[1]:
		sym, ok := p.SymbolByID(reels[0])
		if !ok {
			return Outcome{}, fmt.Errorf("spin: unknown symbol %q", reels[0])
		}
		out.Line = LineTwo
		out.WinSymbol = sym.ID
		out.Multiplier = sym.PayTwo
	}

	// Zero-paying symbols still read as a loss, so the wire contract stays
	// consistent: Line == NONE iff Multiplier == 0.
	if !out.Multiplier.IsPositive() {
		out.Line = LineNone
		out.WinSymbol = ""
		out.Multiplier = decimal.Zero
	}

	// Hard ceiling. Validate() already proves no symbol exceeds it, so this
	// can only fire if the table were mutated at runtime — but this is the
	// last line before a number becomes a credit, so it is checked here too.
	if out.Multiplier.GreaterThan(p.MaxWinMultiplier) {
		return Outcome{}, fmt.Errorf(
			"spin: multiplier %s exceeds max_win_multiplier %s for game %s",
			out.Multiplier, p.MaxWinMultiplier, p.GameID)
	}
	return out, nil
}

// WinFor computes the payout for a stake, truncated to scale decimal places
// (4, matching the ledger's NUMERIC(18,4)).
//
// TRUNCATION, NOT ROUNDING: rounding a fractional sub-unit up would credit
// currency the math model never generated, and over millions of spins that
// drift accrues against the house with no ledger entry explaining it.
// Truncating keeps every credited coin traceable to the paytable.
func WinFor(bet decimal.Decimal, o Outcome, scale int32) decimal.Decimal {
	if !o.IsWin() {
		return decimal.Zero
	}
	return bet.Mul(o.Multiplier).Truncate(scale)
}
