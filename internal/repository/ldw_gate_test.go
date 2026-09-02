package repository

// Gate C, Zone 1 — no celebratory feedback on a net-negative or net-zero round.
//
// The classifier itself is tested in internal/domain. What this file adds is the
// enumeration: it walks EVERY outcome of every registered paytable, at several
// stake sizes, and checks the class the engine would ship.
//
// THE FINDING THAT MAKES THIS GATE NECESSARY
//
// classic-3reel has two outcomes that pay exactly ×1 — a leading CHERRY pair and
// a leading LEMON pair. Both return the stake and nothing more. Their combined
// probability is 10.99%, so on this paytable more than one spin in ten pays out
// while leaving the player exactly where they started.
//
// Any client that decides to celebrate by testing `win_amount > 0` celebrates
// all of them. That is the loss-disguised-as-a-win, and it is not a hypothetical
// risk on some other game: it is 11% of this one.

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/game"
)

// stakeLadder covers whole, fractional and large stakes. The class must depend
// on the SIGN of the net position and nothing else, so a multiplier of exactly 1
// has to be NEUTRAL whether the stake is 0.0001 or 10,000.
var stakeLadder = []string{"0.0001", "0.2500", "1.0000", "2.5000", "100.0000", "10000.0000"}

// TestNoCelebrationOnNetZeroOrNegative enumerates every reel triple of every
// registered game, at every stake on the ladder, and asserts the engine never
// classifies a non-positive round as WIN.
func TestNoCelebrationOnNetZeroOrNegative(t *testing.T) {
	games := game.RegisteredGames()
	if len(games) == 0 {
		t.Fatal("no registered games: this gate would pass vacuously")
	}

	for _, p := range games {
		t.Run(p.GameID+"@"+p.Version, func(t *testing.T) {
			var (
				triples   int
				ldwFound  int
				winFound  int
				lossFound int
			)
			// Distinct LDW outcomes, recorded so the finding is visible in the
			// log rather than buried in a pass.
			ldwOutcomes := map[string]bool{}

			for _, a := range p.Symbols {
				for _, b := range p.Symbols {
					for _, c := range p.Symbols {
						outcome, err := game.Evaluate(p, []string{a.ID, b.ID, c.ID})
						if err != nil {
							t.Fatalf("evaluate %s/%s/%s: %v", a.ID, b.ID, c.ID, err)
						}
						triples++

						for _, stakeStr := range stakeLadder {
							stake, err := domain.MoneyFromString(stakeStr)
							if err != nil {
								t.Fatalf("stake %s: %v", stakeStr, err)
							}
							won, err := domain.NewMoney(
								game.WinFor(stake.Decimal(), outcome, domain.MoneyScale))
							if err != nil {
								t.Fatalf("win for %s at stake %s: %v", outcome.Line, stakeStr, err)
							}

							net := domain.NetPosition(won, stake)
							class := domain.ClassifyFeedback(net)

							// THE ASSERTION. A payout that does not exceed the
							// stake may never be classified as a win.
							if !won.IsZero() && !won.GreaterThan(stake) && class == domain.FeedbackWin {
								t.Errorf("LDW SHIPPED: %v pays %s on a stake of %s (net %s) "+
									"and was classified WIN",
									outcome.Reels, won, stake, net)
							}
							if class == domain.FeedbackWin && !net.IsPositive() {
								t.Errorf("WIN classified for net %s on %v", net, outcome.Reels)
							}

							// Bucket for the coverage assertions below.
							switch {
							case won.IsPositive() && !won.GreaterThan(stake):
								ldwFound++
								ldwOutcomes[string(outcome.Line)+":"+outcome.WinSymbol] = true
								if class != domain.FeedbackNeutral {
									t.Errorf("a stake-returning round on %v classified %s, want NEUTRAL",
										outcome.Reels, class)
								}
							case won.GreaterThan(stake):
								winFound++
								if class != domain.FeedbackWin {
									t.Errorf("a genuine win on %v classified %s, want WIN",
										outcome.Reels, class)
								}
							default:
								lossFound++
								if class != domain.FeedbackLoss {
									t.Errorf("a zero-return round on %v classified %s, want LOSS",
										outcome.Reels, class)
								}
							}
						}
					}
				}
			}

			// The enumeration must have found all three shapes, or it asserted
			// nothing about the ones it missed.
			if triples == 0 || winFound == 0 || lossFound == 0 {
				t.Fatalf("enumeration incomplete: %d triples, %d wins, %d losses",
					triples, winFound, lossFound)
			}
			if ldwFound == 0 {
				t.Errorf("no stake-returning outcome was found for %s. If this paytable "+
					"genuinely has none, that is good news — but this gate then proves "+
					"nothing about the case it exists for, so say so deliberately rather "+
					"than letting it pass silently.", p.GameID)
			}

			t.Logf("%s v%s: %d triples x %d stakes | %d genuine wins, %d losses, %d stake-returning",
				p.GameID, p.Version, triples, len(stakeLadder), winFound, lossFound, ldwFound)
			for k := range ldwOutcomes {
				t.Logf("  loss-disguised-as-a-win outcome: %s", k)
			}
		})
	}
}

// TestLDWOutcomesAreExactlyTheKnownSet pins the LDW set for classic-3reel and
// its probability mass.
//
// This is a CHANGE DETECTOR on purpose. If a future paytable edit adds a third
// stake-returning outcome, or moves the probability of the existing two, this
// fails and forces the change to be looked at — an accidental extra ×1 payout is
// exactly the kind of edit that would otherwise slip through as "a small tweak
// to the cherry line".
func TestLDWOutcomesAreExactlyTheKnownSet(t *testing.T) {
	p := game.ClassicThreeReel
	total := decimal.NewFromInt(int64(p.TotalWeight()))
	one := decimal.NewFromInt(1)

	type ldw struct {
		label string
		prob  decimal.Decimal
	}
	var found []ldw
	mass := decimal.Zero

	for _, s := range p.Symbols {
		prob := decimal.NewFromInt(int64(s.Weight)).Div(total)

		// Three of a kind paying <= 1x.
		if s.PayThree.IsPositive() && s.PayThree.LessThanOrEqual(one) {
			pr := prob.Pow(decimal.NewFromInt(3))
			found = append(found, ldw{"THREE_" + s.ID, pr})
			mass = mass.Add(pr)
		}
		// A leading pair paying <= 1x.
		if s.PayTwo.IsPositive() && s.PayTwo.LessThanOrEqual(one) {
			pr := prob.Pow(decimal.NewFromInt(2)).Mul(one.Sub(prob))
			found = append(found, ldw{"TWO_" + s.ID, pr})
			mass = mass.Add(pr)
		}
	}

	want := map[string]string{
		"TWO_CHERRY": "0.063",
		"TWO_LEMON":  "0.046875",
	}

	if len(found) != len(want) {
		t.Errorf("expected %d stake-returning outcomes, found %d: %+v", len(want), len(found), found)
	}
	for _, f := range found {
		expected, ok := want[f.label]
		if !ok {
			t.Errorf("NEW loss-disguised-as-a-win outcome %s (p=%s). Adding a payout that "+
				"returns the stake exactly increases how often the game pays without "+
				"paying — review this deliberately.", f.label, f.prob)
			continue
		}
		if !f.prob.Round(9).Equal(decimal.RequireFromString(expected)) {
			t.Errorf("%s probability %s, want %s", f.label, f.prob.Round(9), expected)
		}
	}

	// 10.99% of spins. Recorded so the number is in the test output, not only in
	// a comment that can drift away from the paytable.
	if !mass.Round(6).Equal(decimal.RequireFromString("0.109875")) {
		t.Errorf("stake-returning probability mass %s, want 0.109875", mass.Round(6))
	}
	for _, f := range found {
		t.Logf("  stake-returning: %-12s p=%s (%s%% of spins)",
			f.label, f.prob.Round(6), f.prob.Mul(decimal.NewFromInt(100)).Round(4))
	}
	t.Logf("classic-3reel: %d stake-returning outcome(s), combined %s%% of spins",
		len(found), mass.Mul(decimal.NewFromInt(100)).Round(2))
}

// TestSpinResultDerivesFeedbackFromItsOwnAmounts checks the wiring: the fields
// on the wire must follow from the round's own bet and win, whatever they were
// set to beforehand.
func TestSpinResultDerivesFeedbackFromItsOwnAmounts(t *testing.T) {
	mk := func(bet, win string) SpinResult {
		b, err := domain.MoneyFromString(bet)
		if err != nil {
			t.Fatalf("bet: %v", err)
		}
		w, err := domain.MoneyFromString(win)
		if err != nil {
			t.Fatalf("win: %v", err)
		}
		return SpinResult{BetAmount: b, WinAmount: w}
	}

	cases := []struct {
		name      string
		bet, win  string
		wantNet   string
		wantClass domain.FeedbackClass
	}{
		{"total loss", "1.0000", "0.0000", "-1.0000", domain.FeedbackLoss},
		{"stake returned exactly", "1.0000", "1.0000", "0.0000", domain.FeedbackNeutral},
		{"genuine win", "1.0000", "5.0000", "4.0000", domain.FeedbackWin},
		{"partial return", "2.0000", "1.0000", "-1.0000", domain.FeedbackLoss},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mk(tc.bet, tc.win).withFeedback()
			if got.NetPosition.String() != tc.wantNet {
				t.Errorf("net position %s, want %s", got.NetPosition, tc.wantNet)
			}
			if got.FeedbackClass != tc.wantClass {
				t.Errorf("class %s, want %s", got.FeedbackClass, tc.wantClass)
			}
			if err := got.ValidateFeedback(); err != nil {
				t.Errorf("self-derived result failed validation: %v", err)
			}
		})
	}

	// A hand-tampered result must be refused by the response-path guard. This is
	// the case the API handler exists to stop: a WIN asserted on a round whose
	// own numbers say otherwise.
	tampered := mk("1.0000", "1.0000").withFeedback()
	tampered.FeedbackClass = domain.FeedbackWin
	if err := tampered.ValidateFeedback(); err == nil {
		t.Error("a WIN forced onto a break-even round must fail validation")
	}

	// And a stale net position must be caught even when the class matches it.
	stale := mk("1.0000", "5.0000").withFeedback()
	stale.NetPosition = domain.ZeroMoney()
	stale.FeedbackClass = domain.FeedbackNeutral
	if err := stale.ValidateFeedback(); err == nil {
		t.Error("a net position that does not equal win − bet must fail validation")
	}
}
