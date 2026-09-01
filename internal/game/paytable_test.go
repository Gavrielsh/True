package game

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

// TestDeclaredRTPMatchesModel is the guard that makes the published return
// trustworthy: the DeclaredRTP constant must equal the return derived from
// the weights and payouts. Any payout edit that moves the real RTP without
// updating the declaration fails here.
func TestDeclaredRTPMatchesModel(t *testing.T) {
	for id, p := range registry {
		computed := p.TheoreticalRTP()
		// Compare at 7 dp — finer than any published RTP figure.
		if !computed.Round(7).Equal(p.DeclaredRTP.Round(7)) {
			t.Errorf("%s: DeclaredRTP=%s but model computes %s (delta %s)",
				id, p.DeclaredRTP, computed, computed.Sub(p.DeclaredRTP))
		}
	}
}

// TestRTPInRegulatoryBand asserts the shipped game returns within a plausible
// social-casino band. A table accidentally set to 40% or 150% is caught here
// even if DeclaredRTP was updated to match it.
func TestRTPInRegulatoryBand(t *testing.T) {
	lo := decimal.RequireFromString("0.85")
	hi := decimal.RequireFromString("0.99")

	for id, p := range registry {
		rtp := p.TheoreticalRTP()
		if rtp.LessThan(lo) || rtp.GreaterThan(hi) {
			t.Errorf("%s: RTP %s outside band [%s, %s]", id, rtp, lo, hi)
		}
	}
}

func TestPaytablesValidate(t *testing.T) {
	for id, p := range registry {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	base := func() Paytable {
		return Paytable{
			GameID: "t", Version: "1", MaxWinMultiplier: d("10"),
			Symbols: []Symbol{{ID: "A", Weight: 1, PayThree: d("2"), PayTwo: d("1")}},
		}
	}

	tests := []struct {
		name   string
		mutate func(*Paytable)
	}{
		{"empty game id", func(p *Paytable) { p.GameID = "" }},
		{"empty version", func(p *Paytable) { p.Version = "" }},
		{"no symbols", func(p *Paytable) { p.Symbols = nil }},
		{"zero weight", func(p *Paytable) { p.Symbols[0].Weight = 0 }},
		{"negative payout", func(p *Paytable) { p.Symbols[0].PayThree = d("-1") }},
		{"payout above ceiling", func(p *Paytable) { p.Symbols[0].PayThree = d("999") }},
		{"zero ceiling", func(p *Paytable) { p.MaxWinMultiplier = decimal.Zero }},
		{"duplicate symbol", func(p *Paytable) {
			p.Symbols = append(p.Symbols, Symbol{ID: "A", Weight: 1, PayThree: d("1"), PayTwo: d("1")})
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Monte-Carlo convergence: the RNG + evaluator must actually realise the
// theoretical RTP. This is the check that catches a mismatch between the
// weighted draw and the probability model TheoreticalRTP() assumes.
// ----------------------------------------------------------------------------

// TestExhaustiveReturnMatchesTheory enumerates every possible reel triple,
// weights each by its probability, and confirms the realised return equals
// TheoreticalRTP() exactly. This is stronger than sampling: it proves the
// evaluator's line rules and the closed-form RTP formula agree.
func TestExhaustiveReturnMatchesTheory(t *testing.T) {
	p := ClassicThreeReel
	total := decimal.NewFromInt(int64(p.TotalWeight()))

	realised := decimal.Zero
	for _, a := range p.Symbols {
		for _, b := range p.Symbols {
			for _, c := range p.Symbols {
				out, err := Evaluate(p, []string{a.ID, b.ID, c.ID})
				if err != nil {
					t.Fatalf("evaluate %s/%s/%s: %v", a.ID, b.ID, c.ID, err)
				}
				// P(this exact triple) = (wa/W)(wb/W)(wc/W)
				prob := decimal.NewFromInt(int64(a.Weight)).Div(total).
					Mul(decimal.NewFromInt(int64(b.Weight)).Div(total)).
					Mul(decimal.NewFromInt(int64(c.Weight)).Div(total))
				realised = realised.Add(prob.Mul(out.Multiplier))
			}
		}
	}

	theory := p.TheoreticalRTP()
	if !realised.Round(9).Equal(theory.Round(9)) {
		t.Fatalf("exhaustive return %s != theoretical %s (delta %s)",
			realised.Round(9), theory.Round(9), realised.Sub(theory))
	}
	t.Logf("exhaustive realised RTP = %s (theory %s)", realised.Round(7), theory.Round(7))
}

// TestSpinConvergesToRTP runs the real crypto RNG over many spins and checks
// the empirical return lands near theory. Tolerance is wide because this is a
// smoke test for wiring, not a statistical proof — TestExhaustiveReturn is
// the exact check.
func TestSpinConvergesToRTP(t *testing.T) {
	if testing.Short() {
		t.Skip("monte-carlo skipped in -short")
	}
	p := ClassicThreeReel
	rng := NewCryptoRNG()

	const spins = 200_000
	bet := decimal.NewFromInt(1)
	totalWon := decimal.Zero

	for i := 0; i < spins; i++ {
		out, err := Spin(p, rng)
		if err != nil {
			t.Fatalf("spin %d: %v", i, err)
		}
		totalWon = totalWon.Add(WinFor(bet, out, 4))
	}

	empirical := totalWon.Div(decimal.NewFromInt(spins))
	theory := p.TheoreticalRTP()
	delta := empirical.Sub(theory).Abs()

	// The distribution is heavy-tailed (CROWN ×400 at p=6.4e-5), so 200k
	// spins carry real variance. 8pp catches a wiring bug without flaking.
	tolerance := decimal.RequireFromString("0.08")
	if delta.GreaterThan(tolerance) {
		t.Errorf("empirical RTP %s deviates from theory %s by %s (tolerance %s)",
			empirical.Round(5), theory.Round(5), delta.Round(5), tolerance)
	}
	t.Logf("empirical RTP over %d spins = %s (theory %s)", spins, empirical.Round(5), theory.Round(5))
}

// ----------------------------------------------------------------------------
// Evaluator rules
// ----------------------------------------------------------------------------

func TestEvaluateLineRules(t *testing.T) {
	p := ClassicThreeReel

	tests := []struct {
		name     string
		reels    []string
		wantLine LineResult
		wantMult string
	}{
		{"three crowns", []string{"CROWN", "CROWN", "CROWN"}, LineThree, "400"},
		{"three cherries", []string{"CHERRY", "CHERRY", "CHERRY"}, LineThree, "5"},
		{"two leading bells", []string{"BELL", "BELL", "CHERRY"}, LineTwo, "2"},
		{"trailing pair pays nothing", []string{"CHERRY", "BELL", "BELL"}, LineNone, "0"},
		{"outer pair pays nothing", []string{"BELL", "CHERRY", "BELL"}, LineNone, "0"},
		{"all different", []string{"CHERRY", "LEMON", "BELL"}, LineNone, "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Evaluate(p, tc.reels)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if out.Line != tc.wantLine {
				t.Errorf("line: got %s want %s", out.Line, tc.wantLine)
			}
			if !out.Multiplier.Equal(d(tc.wantMult)) {
				t.Errorf("multiplier: got %s want %s", out.Multiplier, tc.wantMult)
			}
		})
	}
}

func TestEvaluateRejectsWrongReelCount(t *testing.T) {
	if _, err := Evaluate(ClassicThreeReel, []string{"CHERRY", "CHERRY"}); err == nil {
		t.Fatal("expected error for 2 reels")
	}
}

func TestEvaluateRejectsUnknownSymbol(t *testing.T) {
	if _, err := Evaluate(ClassicThreeReel, []string{"WILD", "WILD", "WILD"}); err == nil {
		t.Fatal("expected error for unknown symbol")
	}
}

// TestWinForTruncatesNeverRounds pins the truncation policy: a fractional
// sub-unit is dropped, never rounded up into currency the model did not
// generate.
func TestWinForTruncatesNeverRounds(t *testing.T) {
	out := Outcome{Line: LineTwo, WinSymbol: "BELL", Multiplier: d("2")}

	// 0.00005 × 2 = 0.0001 exactly — kept.
	if got := WinFor(d("0.00005"), out, 4); !got.Equal(d("0.0001")) {
		t.Errorf("exact-scale win: got %s want 0.0001", got)
	}
	// 0.000049 × 2 = 0.000098 → truncates to 0.0000, not 0.0001.
	if got := WinFor(d("0.000049"), out, 4); !got.Equal(decimal.Zero) {
		t.Errorf("sub-scale win should truncate to zero, got %s", got)
	}
}

func TestWinForLossIsZero(t *testing.T) {
	if got := WinFor(d("100"), Outcome{Line: LineNone, Multiplier: decimal.Zero}, 4); !got.IsZero() {
		t.Errorf("loss must pay zero, got %s", got)
	}
}

func TestLookupUnknownGameFails(t *testing.T) {
	if _, ok := Lookup("does-not-exist"); ok {
		t.Fatal("unknown game must not resolve")
	}
	if _, ok := Lookup(DefaultGameID); !ok {
		t.Fatal("default game must resolve")
	}
}

// ----------------------------------------------------------------------------
// RNG
// ----------------------------------------------------------------------------

func TestUint64NRejectsZero(t *testing.T) {
	if _, err := NewCryptoRNG().Uint64N(0); err == nil {
		t.Fatal("expected ErrRNGRange")
	}
}

func TestUint64NStaysInRange(t *testing.T) {
	rng := NewCryptoRNG()
	for _, n := range []uint64{1, 2, 3, 7, 64, 100, 1023} {
		for i := 0; i < 500; i++ {
			v, err := rng.Uint64N(n)
			if err != nil {
				t.Fatalf("n=%d: %v", n, err)
			}
			if v >= n {
				t.Fatalf("n=%d: got %d, out of range", n, v)
			}
		}
	}
}

// TestUint64NRejectsBiasedDraw proves the rejection branch actually fires:
// with n=3, threshold = 2^64 mod 3 = 1, so a draw of 0 must be rejected and
// the next value used instead.
func TestUint64NRejectsBiasedDraw(t *testing.T) {
	var buf bytes.Buffer
	write := func(v uint64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v)
		buf.Write(b[:])
	}
	write(0)                    // below threshold (1) → rejected
	write(math.MaxUint64 - 100) // accepted

	rng := NewCryptoRNGWithSource(&buf)
	got, err := rng.Uint64N(3)
	if err != nil {
		t.Fatalf("Uint64N: %v", err)
	}
	want := uint64(math.MaxUint64-100) % 3
	if got != want {
		t.Fatalf("got %d, want %d — rejection sampling did not skip the biased draw", got, want)
	}
}

// TestUint64NPowerOfTwoSkipsRejection documents the fast path: powers of two
// divide 2^64 exactly, so the first draw is always accepted.
func TestUint64NPowerOfTwoSkipsRejection(t *testing.T) {
	var buf bytes.Buffer
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 0)
	buf.Write(b[:])

	got, err := NewCryptoRNGWithSource(&buf).Uint64N(64)
	if err != nil {
		t.Fatalf("Uint64N: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

// TestSpinFailsClosedOnEntropyError is the critical safety property: if the
// entropy source fails, the spin errors out. It must NEVER fall back to a
// weaker source or return a default outcome.
func TestSpinFailsClosedOnEntropyError(t *testing.T) {
	// Empty reader → io.ReadFull returns EOF immediately.
	rng := NewCryptoRNGWithSource(bytes.NewReader(nil))
	if _, err := Spin(ClassicThreeReel, rng); err == nil {
		t.Fatal("spin must fail when entropy is unavailable")
	}
}

// TestDrawDistributionMatchesWeights confirms the weighted walk in
// symbolForRoll assigns exactly Weight/TotalWeight to each symbol, by
// enumerating every roll in [0, TotalWeight).
func TestDrawDistributionMatchesWeights(t *testing.T) {
	p := ClassicThreeReel
	counts := map[string]int{}
	//nolint:gosec // G115: validated paytable weights, as in spin.go.
	for roll := uint64(0); roll < uint64(p.TotalWeight()); roll++ {
		s, err := symbolForRoll(p, roll)
		if err != nil {
			t.Fatalf("roll %d: %v", roll, err)
		}
		counts[s.ID]++
	}
	for _, s := range p.Symbols {
		if counts[s.ID] != s.Weight {
			t.Errorf("symbol %s: got %d slots, want weight %d", s.ID, counts[s.ID], s.Weight)
		}
	}
}
