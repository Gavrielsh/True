package game

// Gate A — Monte-Carlo RTP convergence (Milestone 1.2).
//
// WHAT THIS GATE IS FOR
//
// TestExhaustiveReturnMatchesTheory already proves the evaluator and the
// closed-form RTP agree, by enumerating every reel triple. What it cannot see
// is the RNG: it never draws anything. A weighted draw that walks the strip
// incorrectly, a modulo bias reintroduced into Uint64N, a reel that silently
// reuses the previous roll — none of those change the paytable, so none of them
// move the exhaustive test. They move the REALISED return, and only sampling
// finds them.
//
// So this gate runs the production Spin path end to end and asks whether the
// return it actually produces is consistent with the return the model predicts.
//
// THE TOLERANCE IS DERIVED, NOT CHOSEN
//
// The test this replaces compared the empirical mean against theory with a
// hardcoded 0.08 band at 200,000 spins. That number was a guess, and it was a
// bad one in both directions: it is ~6.5σ at that sample size, so it would sit
// quiet through a real defect, and it carried no relationship to the sample
// size at all — running more spins made the test slower without making it
// stricter.
//
// The band here is the one the mathematics dictates. For n independent spins of
// a variable with per-spin standard deviation σ, the sample mean has standard
// error σ/√n, so:
//
//	tolerance = sigmaMultiplier · σ / √n
//
// σ comes from Paytable.TheoreticalStdDev(), which is derived from the weights
// and payouts alone and is verified against exhaustive enumeration by
// TestTheoreticalVarianceMatchesExhaustive. Nothing in this file is tuned.
// Raising the spin count now automatically tightens the gate.
//
// ON sigmaMultiplier = 4, HONESTLY
//
// Four is a policy choice — the confidence level — not a fitted constant. Under
// a normal approximation P(|Z| > 4) ≈ 6.3e-5.
//
// That figure should not be quoted as this gate's false-failure rate, and the
// reason is worth recording. The per-spin distribution is heavy-tailed: CROWN
// pays ×400 at probability 6.4e-5, so a single outcome carries a large share of
// the variance. The Berry-Esseen bound on the CLT approximation error is ~7.1e-3
// at n = 5e6 — two orders of magnitude LARGER than the nominal tail. Berry-Esseen
// is a worst-case sup over the whole distribution and is notoriously loose in
// the tails, so the true rate is far better than that bound; but it is not
// pinned by theory alone, and claiming 6.3e-5 would be overstating what is known.
//
// What makes the gate useful anyway is the size of the defects it is aimed at.
// A miswired draw does not shift RTP by a fraction of a standard error; it shifts
// it by percentage points. At 5,000,000 spins the band is ~0.98pp and at
// 100,000,000 spins it is ~0.22pp, and both are far tighter than any plausible
// wiring bug is subtle.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

const (
	// sigmaMultiplier is the width of the acceptance band in standard errors.
	// See the note above: a policy choice, stated once, applied in both
	// directions.
	sigmaMultiplier = 4

	// defaultGateSpins is what an ordinary `go test ./...` runs. CI overrides
	// it via RTP_SPINS — 5,000,000 on a pull request, 100,000,000 nightly.
	//
	// It is deliberately not tiny. This test carries NO testing.Short() guard
	// (see TestMonteCarloRTPConvergence), so whatever is set here is a cost every
	// contributor pays on every run, and also the weakest form of the gate that
	// can ship. One million spins costs well under a second and still bounds the
	// realised RTP to ±2.2pp.
	defaultGateSpins = 1_000_000

	// envSpins / envReport let CI drive the gate without editing the test.
	envSpins  = "RTP_SPINS"
	envReport = "RTP_REPORT_PATH"

	// defaultReportName is written into the package directory when CI does not
	// name a path. Gitignored.
	defaultReportName = "rtp-report.json"
)

// ----------------------------------------------------------------------------
// The decision, isolated so it can be tested without spinning anything
// ----------------------------------------------------------------------------

// rtpAssessment is the full arithmetic of one convergence check. Every field is
// carried so the JSON report can be reconstructed and re-checked by hand.
type rtpAssessment struct {
	Spins       int64
	Theoretical decimal.Decimal
	Empirical   decimal.Decimal
	Delta       decimal.Decimal // signed: empirical − theoretical
	AbsDelta    decimal.Decimal
	StdDev      decimal.Decimal // σ, per spin
	StdError    decimal.Decimal // σ/√n
	Tolerance   decimal.Decimal // sigmaMultiplier · σ/√n
	ZScore      decimal.Decimal // delta / stdError, signed
	Within      bool
}

// assessRTP derives the acceptance band for `spins` samples and decides.
//
// TWO-SIDED BY CONSTRUCTION. The comparison is on |delta|, and the signed delta
// and z-score are both retained for the report. An engine paying too much and an
// engine paying too little are the same class of defect — one loses money and
// the other is a licensing failure — so neither direction is privileged.
func assessRTP(p Paytable, empirical decimal.Decimal, spins int64) (rtpAssessment, error) {
	if spins <= 0 {
		return rtpAssessment{}, fmt.Errorf("rtp gate: spins must be > 0, got %d", spins)
	}

	sigma, err := p.TheoreticalStdDev()
	if err != nil {
		return rtpAssessment{}, err
	}

	rootN, err := decimal.NewFromInt(spins).PowWithPrecision(decimal.New(5, -1), stdDevPrecision)
	if err != nil {
		return rtpAssessment{}, fmt.Errorf("rtp gate: sqrt(%d): %w", spins, err)
	}

	theory := p.TheoreticalRTP()
	delta := empirical.Sub(theory)
	stdErr := sigma.Div(rootN)
	tolerance := decimal.NewFromInt(sigmaMultiplier).Mul(stdErr)

	z := decimal.Zero
	if !stdErr.IsZero() {
		z = delta.Div(stdErr)
	}

	return rtpAssessment{
		Spins:       spins,
		Theoretical: theory,
		Empirical:   empirical,
		Delta:       delta,
		AbsDelta:    delta.Abs(),
		StdDev:      sigma,
		StdError:    stdErr,
		Tolerance:   tolerance,
		ZScore:      z,
		// Strictly greater: landing exactly on the band is inside it.
		Within: !delta.Abs().GreaterThan(tolerance),
	}, nil
}

// ----------------------------------------------------------------------------
// The machine-readable artifact
// ----------------------------------------------------------------------------

// rtpReport is the JSON emitted on every run, passing or failing.
//
// Every number is a STRING. These are decimals whose exactness is the whole
// point; serialising them as JSON numbers would push them through a float64 on
// the way in and out, which is precisely the rounding this codebase refuses
// everywhere else money is involved.
type rtpReport struct {
	SchemaVersion   int               `json:"schema_version"`
	GeneratedAt     string            `json:"generated_at"`
	GameID          string            `json:"game_id"`
	PaytableVersion string            `json:"paytable_version"`
	Spins           int64             `json:"spins"`
	TheoreticalRTP  string            `json:"theoretical_rtp"`
	EmpiricalRTP    string            `json:"empirical_rtp"`
	Delta           string            `json:"delta"`
	AbsDelta        string            `json:"abs_delta"`
	PerSpinStdDev   string            `json:"per_spin_stddev"`
	StandardError   string            `json:"standard_error"`
	SigmaMultiplier int               `json:"sigma_multiplier"`
	Tolerance       string            `json:"tolerance"`
	ZScore          string            `json:"z_score"`
	WithinTolerance bool              `json:"within_tolerance"`
	TotalStaked     string            `json:"total_staked"`
	TotalReturned   string            `json:"total_returned"`
	OutcomeCounts   map[string]int64  `json:"outcome_counts"`
	SymbolPayouts   map[string]string `json:"symbol_payouts"`
}

func writeRTPReport(t *testing.T, path string, r rtpReport) {
	t.Helper()
	blob, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("rtp gate: marshal report: %v", err)
	}
	blob = append(blob, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("rtp gate: create report directory %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("rtp gate: write report %s: %v", path, err)
	}
	t.Logf("rtp gate: report written to %s", path)
}

// ----------------------------------------------------------------------------
// Gate A
// ----------------------------------------------------------------------------

// TestMonteCarloRTPConvergence is Gate A.
//
// NO -short GUARD, DELIBERATELY. The test it replaces began with
//
//	if testing.Short() { t.Skip("monte-carlo skipped in -short") }
//
// which means the one check that exercises the RNG against the payout model
// disappeared silently under a flag, and a job that passed -short reported green
// having verified nothing about the engine's actual return. That is the same
// shape as every other defect Milestone 0 found: a control that looks enforced
// and is not.
//
// The cost of removing it is bounded by defaultGateSpins, which is why that
// constant is set where it is. CI proves the absence behaviourally by running
// this test WITH -short and asserting nothing skipped — see the rtp job in
// .github/workflows/engine.yml.
func TestMonteCarloRTPConvergence(t *testing.T) {
	p := ClassicThreeReel
	spins := gateSpins(t)

	// The production RNG, unbuffered and unmodified. Wrapping crypto/rand in a
	// bufio.Reader measures ~17% faster, and it is declined on purpose: the
	// subject of this test is the path that draws real spins, and 100,000,000
	// spins complete in well under two minutes either way.
	rng := NewCryptoRNG()

	symbolIndex := make(map[string]int, len(p.Symbols))
	for i, s := range p.Symbols {
		symbolIndex[s.ID] = i
	}
	threeCounts := make([]int64, len(p.Symbols))
	twoCounts := make([]int64, len(p.Symbols))
	var noneCount int64

	// Tally outcomes as integers and do the decimal arithmetic once at the end.
	// Accumulating a decimal per spin would dominate the runtime and buy nothing:
	// counting is exact, and the payout attached to each bucket is fixed.
	start := time.Now()
	for i := int64(0); i < spins; i++ {
		out, err := Spin(p, rng)
		if err != nil {
			t.Fatalf("rtp gate: spin %d: %v", i, err)
		}
		switch out.Line {
		case LineThree:
			threeCounts[symbolIndex[out.WinSymbol]]++
		case LineTwo:
			twoCounts[symbolIndex[out.WinSymbol]]++
		case LineNone:
			noneCount++
		default:
			t.Fatalf("rtp gate: spin %d produced unknown line %q", i, out.Line)
		}
	}
	elapsed := time.Since(start)

	// Σ (count · payout), exact.
	returned := decimal.Zero
	counts := make(map[string]int64, len(p.Symbols)*2+1)
	payouts := make(map[string]string, len(p.Symbols)*2)
	for i, s := range p.Symbols {
		returned = returned.
			Add(decimal.NewFromInt(threeCounts[i]).Mul(s.PayThree)).
			Add(decimal.NewFromInt(twoCounts[i]).Mul(s.PayTwo))
		counts["THREE_"+s.ID] = threeCounts[i]
		counts["TWO_"+s.ID] = twoCounts[i]
		payouts["THREE_"+s.ID] = s.PayThree.String()
		payouts["TWO_"+s.ID] = s.PayTwo.String()
	}
	counts["NONE"] = noneCount

	staked := decimal.NewFromInt(spins)
	empirical := returned.Div(staked)

	assessment, err := assessRTP(p, empirical, spins)
	if err != nil {
		t.Fatalf("rtp gate: %v", err)
	}

	writeRTPReport(t, reportPath(), rtpReport{
		SchemaVersion:   1,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		GameID:          p.GameID,
		PaytableVersion: p.Version,
		Spins:           assessment.Spins,
		TheoreticalRTP:  assessment.Theoretical.String(),
		EmpiricalRTP:    assessment.Empirical.Round(12).String(),
		Delta:           assessment.Delta.Round(12).String(),
		AbsDelta:        assessment.AbsDelta.Round(12).String(),
		PerSpinStdDev:   assessment.StdDev.Round(12).String(),
		StandardError:   assessment.StdError.Round(12).String(),
		SigmaMultiplier: sigmaMultiplier,
		Tolerance:       assessment.Tolerance.Round(12).String(),
		ZScore:          assessment.ZScore.Round(6).String(),
		WithinTolerance: assessment.Within,
		TotalStaked:     staked.String(),
		TotalReturned:   returned.String(),
		OutcomeCounts:   counts,
		SymbolPayouts:   payouts,
	})

	t.Logf("rtp gate: %d spins in %s (%.0f spins/sec)",
		spins, elapsed.Round(time.Millisecond), float64(spins)/elapsed.Seconds())
	t.Logf("rtp gate: empirical %s vs theoretical %s | delta %s | z %s",
		assessment.Empirical.Round(8), assessment.Theoretical,
		assessment.Delta.Round(8), assessment.ZScore.Round(3))
	t.Logf("rtp gate: sigma %s | standard error %s | tolerance (%dσ) %s",
		assessment.StdDev.Round(9), assessment.StdError.Round(9),
		sigmaMultiplier, assessment.Tolerance.Round(9))

	if !assessment.Within {
		direction := "ABOVE"
		if assessment.Delta.IsNegative() {
			direction = "BELOW"
		}
		t.Errorf("RTP DRIFT %s theory: empirical %s, theoretical %s, delta %s (%s σ), "+
			"outside the %dσ band of %s over %d spins",
			direction, assessment.Empirical.Round(8), assessment.Theoretical,
			assessment.Delta.Round(8), assessment.ZScore.Round(3),
			sigmaMultiplier, assessment.Tolerance.Round(8), spins)
	}
}

func gateSpins(t *testing.T) int64 {
	t.Helper()
	raw, ok := os.LookupEnv(envSpins)
	if !ok || raw == "" {
		return defaultGateSpins
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// Fail rather than silently fall back: a typo in the CI job must not
		// quietly downgrade the gate to its default.
		t.Fatalf("rtp gate: %s=%q is not an integer: %v", envSpins, raw, err)
	}
	if n <= 0 {
		t.Fatalf("rtp gate: %s=%d must be > 0", envSpins, n)
	}
	return n
}

func reportPath() string {
	if p := os.Getenv(envReport); p != "" {
		return p
	}
	return defaultReportName
}

// ----------------------------------------------------------------------------
// The gate's own tests — the threshold is only trustworthy if it is checked
// ----------------------------------------------------------------------------

// TestTheoreticalVarianceMatchesExhaustive proves the closed-form variance
// against brute force, exactly as TestExhaustiveReturnMatchesTheory does for the
// mean. Without this, σ would be an unverified number feeding every tolerance in
// this file — a magic constant with a derivation attached.
func TestTheoreticalVarianceMatchesExhaustive(t *testing.T) {
	p := ClassicThreeReel
	total := decimal.NewFromInt(int64(p.TotalWeight()))

	mean, second := decimal.Zero, decimal.Zero
	for _, a := range p.Symbols {
		for _, b := range p.Symbols {
			for _, c := range p.Symbols {
				out, err := Evaluate(p, []string{a.ID, b.ID, c.ID})
				if err != nil {
					t.Fatalf("evaluate %s/%s/%s: %v", a.ID, b.ID, c.ID, err)
				}
				prob := decimal.NewFromInt(int64(a.Weight)).Div(total).
					Mul(decimal.NewFromInt(int64(b.Weight)).Div(total)).
					Mul(decimal.NewFromInt(int64(c.Weight)).Div(total))
				mean = mean.Add(prob.Mul(out.Multiplier))
				second = second.Add(prob.Mul(out.Multiplier.Mul(out.Multiplier)))
			}
		}
	}
	exhaustive := second.Sub(mean.Mul(mean))
	closed := p.TheoreticalVariance()

	if !exhaustive.Round(12).Equal(closed.Round(12)) {
		t.Fatalf("variance mismatch: exhaustive %s != closed form %s (delta %s)",
			exhaustive.Round(12), closed.Round(12), exhaustive.Sub(closed))
	}

	sigma, err := p.TheoreticalStdDev()
	if err != nil {
		t.Fatalf("stddev: %v", err)
	}
	// σ² must reproduce the variance it came from.
	if !sigma.Mul(sigma).Round(12).Equal(closed.Round(12)) {
		t.Errorf("stddev does not square back to variance: σ=%s σ²=%s var=%s",
			sigma, sigma.Mul(sigma).Round(12), closed.Round(12))
	}
	t.Logf("variance %s, sigma %s (exhaustive agreement at 12dp)", closed.Round(12), sigma.Round(12))
}

// TestRTPGateIsTwoSided is the mutation test for the gate's decision.
//
// It drives assessRTP with synthetic empirical values placed just inside and
// just outside the band on BOTH sides, so a gate that only catches underpaying —
// or only overpaying — cannot pass. Requirement: drift in either direction fails.
func TestRTPGateIsTwoSided(t *testing.T) {
	p := ClassicThreeReel
	const spins = 5_000_000

	base, err := assessRTP(p, p.TheoreticalRTP(), spins)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	tol := base.Tolerance
	theory := p.TheoreticalRTP()
	// A nudge far smaller than the band, used to sit either side of the edge.
	epsilon := tol.Div(decimal.NewFromInt(1000))

	cases := []struct {
		name       string
		empirical  decimal.Decimal
		wantWithin bool
	}{
		{"exactly on theory", theory, true},
		{"just inside, above", theory.Add(tol).Sub(epsilon), true},
		{"just inside, below", theory.Sub(tol).Add(epsilon), true},
		{"exactly on the band, above", theory.Add(tol), true},
		{"exactly on the band, below", theory.Sub(tol), true},
		{"just outside, above", theory.Add(tol).Add(epsilon), false},
		{"just outside, below", theory.Sub(tol).Sub(epsilon), false},
		{"grossly overpaying", theory.Add(tol.Mul(decimal.NewFromInt(50))), false},
		{"grossly underpaying", theory.Sub(tol.Mul(decimal.NewFromInt(50))), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := assessRTP(p, tc.empirical, spins)
			if err != nil {
				t.Fatalf("assess: %v", err)
			}
			if got.Within != tc.wantWithin {
				t.Errorf("within=%v want %v (delta %s, tolerance %s, z %s)",
					got.Within, tc.wantWithin, got.Delta.Round(9),
					got.Tolerance.Round(9), got.ZScore.Round(3))
			}
			// The sign of the reported delta must track the direction, so the
			// failure message and the report cannot mislabel which way it drifted.
			if tc.empirical.GreaterThan(theory) && !got.Delta.IsPositive() {
				t.Errorf("delta %s should be positive when empirical exceeds theory", got.Delta)
			}
			if tc.empirical.LessThan(theory) && !got.Delta.IsNegative() {
				t.Errorf("delta %s should be negative when empirical trails theory", got.Delta)
			}
		})
	}
}

// TestRTPToleranceScalesWithSampleSize proves the band is genuinely a function
// of n rather than a constant wearing a formula. Quadrupling the sample size
// must halve the tolerance, because the standard error carries 1/√n.
//
// This is what the replaced 0.08 could never do: it was identical at 200,000
// spins and at 100,000,000.
func TestRTPToleranceScalesWithSampleSize(t *testing.T) {
	p := ClassicThreeReel
	theory := p.TheoreticalRTP()

	small, err := assessRTP(p, theory, 1_000_000)
	if err != nil {
		t.Fatalf("small: %v", err)
	}
	large, err := assessRTP(p, theory, 4_000_000)
	if err != nil {
		t.Fatalf("large: %v", err)
	}

	ratio := small.Tolerance.Div(large.Tolerance)
	if !ratio.Round(6).Equal(decimal.NewFromInt(2)) {
		t.Errorf("quadrupling n must halve the tolerance: ratio %s (small %s, large %s)",
			ratio.Round(6), small.Tolerance.Round(9), large.Tolerance.Round(9))
	}

	// And it must be the stated multiple of the standard error, in both cases.
	for _, a := range []rtpAssessment{small, large} {
		want := a.StdError.Mul(decimal.NewFromInt(sigmaMultiplier))
		if !a.Tolerance.Equal(want) {
			t.Errorf("n=%d: tolerance %s != %d × standard error %s",
				a.Spins, a.Tolerance, sigmaMultiplier, want)
		}
	}
	t.Logf("1e6 → %s, 4e6 → %s (ratio %s)",
		small.Tolerance.Round(9), large.Tolerance.Round(9), ratio.Round(6))
}

// TestRTPGateRejectsInvalidSampleSize covers the guard that keeps a
// misconfigured job from dividing by zero and reporting an infinite band.
func TestRTPGateRejectsInvalidSampleSize(t *testing.T) {
	p := ClassicThreeReel
	for _, n := range []int64{0, -1, -5_000_000} {
		if _, err := assessRTP(p, p.TheoreticalRTP(), n); err == nil {
			t.Errorf("spins=%d must be rejected", n)
		}
	}
}
