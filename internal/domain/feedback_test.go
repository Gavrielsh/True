package domain

import (
	"testing"

	"github.com/shopspring/decimal"
)

// Uses the package's existing `money(t, string) Money` helper from casino_test.go.

// TestClassifyFeedbackIsStrictAboutWIN is the core of Gate C.
//
// WIN must require net > 0 STRICTLY. The boundary case — net exactly zero — is
// the loss disguised as a win, and it is the one the industry gets wrong, so it
// is tested from both sides at the smallest representable step.
func TestClassifyFeedbackIsStrictAboutWIN(t *testing.T) {
	cases := []struct {
		name string
		net  string
		want FeedbackClass
	}{
		{"a real gain", "1.0000", FeedbackWin},
		{"the smallest representable gain", "0.0001", FeedbackWin},

		// THE LDW BOUNDARY. Stake returned exactly. A payout happened, the reels
		// show a pair, and the player is no better off.
		{"exactly break-even", "0.0000", FeedbackNeutral},

		{"the smallest representable loss", "-0.0001", FeedbackLoss},
		{"a partial return", "-0.5000", FeedbackLoss},
		{"the whole stake lost", "-1.0000", FeedbackLoss},
		{"a large loss", "-1000.0000", FeedbackLoss},
		{"a large gain", "399.0000", FeedbackWin},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFeedback(money(t, tc.net))
			if got != tc.want {
				t.Errorf("net %s classified %s, want %s", tc.net, got, tc.want)
			}
			if got == FeedbackWin && !money(t, tc.net).IsPositive() {
				t.Errorf("WIN returned for net %s, which is not > 0", tc.net)
			}
		})
	}
}

// TestNetPositionIsReturnMinusStake pins the arithmetic the class is derived
// from, including the case that matters: a payout equal to the stake.
func TestNetPositionIsReturnMinusStake(t *testing.T) {
	cases := []struct {
		name             string
		ret, stake, want string
		wantClass        FeedbackClass
	}{
		{"nothing returned", "0.0000", "1.0000", "-1.0000", FeedbackLoss},
		{"stake returned exactly (LDW)", "1.0000", "1.0000", "0.0000", FeedbackNeutral},
		{"double the stake", "2.0000", "1.0000", "1.0000", FeedbackWin},
		{"partial return", "0.5000", "1.0000", "-0.5000", FeedbackLoss},
		{"fractional stake, exact return", "0.2500", "0.2500", "0.0000", FeedbackNeutral},
		{"a hair over the stake", "1.0001", "1.0000", "0.0001", FeedbackWin},
		{"a hair under the stake", "0.9999", "1.0000", "-0.0001", FeedbackLoss},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net := NetPosition(money(t, tc.ret), money(t, tc.stake))
			if !net.Equal(money(t, tc.want)) {
				t.Errorf("net position %s, want %s", net, tc.want)
			}
			if got := ClassifyFeedback(net); got != tc.wantClass {
				t.Errorf("class %s, want %s", got, tc.wantClass)
			}
		})
	}
}

// TestValidateFeedbackRejectsInconsistentPairs exercises the fail-closed guard.
// The first case is the one that matters: a WIN claimed on a break-even round.
func TestValidateFeedbackRejectsInconsistentPairs(t *testing.T) {
	valid := []struct {
		net   string
		class FeedbackClass
	}{
		{"1.0000", FeedbackWin},
		{"0.0000", FeedbackNeutral},
		{"-1.0000", FeedbackLoss},
	}
	for _, v := range valid {
		if err := ValidateFeedback(money(t, v.net), v.class); err != nil {
			t.Errorf("net %s class %s should be valid: %v", v.net, v.class, err)
		}
	}

	invalid := []struct {
		name  string
		net   string
		class FeedbackClass
	}{
		{"WIN claimed on a break-even round (the LDW)", "0.0000", FeedbackWin},
		{"WIN claimed on a loss", "-1.0000", FeedbackWin},
		{"NEUTRAL claimed on a real win", "1.0000", FeedbackNeutral},
		{"LOSS claimed on a real win", "1.0000", FeedbackLoss},
		{"NEUTRAL claimed on a loss", "-0.5000", FeedbackNeutral},
		{"empty class", "1.0000", FeedbackClass("")},
		{"invented class", "1.0000", FeedbackClass("JACKPOT")},
		{"lowercase is not the same class", "1.0000", FeedbackClass("win")},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateFeedback(money(t, tc.net), tc.class); err == nil {
				t.Errorf("net %s with class %q must be rejected", tc.net, tc.class)
			}
		})
	}
}

// TestFeedbackClassValidRejectsAnythingElse keeps the wire vocabulary closed.
// A client switching on this value must never receive a fourth option.
func TestFeedbackClassValidRejectsAnythingElse(t *testing.T) {
	for _, ok := range []FeedbackClass{FeedbackWin, FeedbackNeutral, FeedbackLoss} {
		if !ok.Valid() {
			t.Errorf("%s must be valid", ok)
		}
	}
	for _, bad := range []FeedbackClass{"", "win", "Win", "WINNER", "NEUTRAL ", "BONUS", "0"} {
		if FeedbackClass(bad).Valid() {
			t.Errorf("%q must not be a valid class", bad)
		}
	}
}

// TestClassifyFeedbackIsTotal proves there is no Money value for which the
// classifier has no answer, and that WIN is unreachable without a strict gain.
// A default branch that fell through to WIN is precisely the bug this gate is
// aimed at, so the property is asserted over a swept range rather than trusted
// to the shape of the switch.
func TestClassifyFeedbackIsTotal(t *testing.T) {
	step := decimal.New(1, -MoneyScale) // 0.0001
	v := decimal.NewFromInt(-5)
	for i := 0; i < 100_000; i++ {
		m, err := NewMoney(v)
		if err != nil {
			t.Fatalf("money %s: %v", v, err)
		}
		class := ClassifyFeedback(m)
		if !class.Valid() {
			t.Fatalf("net %s produced invalid class %q", m, class)
		}
		switch class {
		case FeedbackWin:
			if !m.IsPositive() {
				t.Fatalf("WIN for net %s, which is not > 0", m)
			}
		case FeedbackNeutral:
			if !m.IsZero() {
				t.Fatalf("NEUTRAL for net %s, which is not zero", m)
			}
		case FeedbackLoss:
			if !m.IsNegative() {
				t.Fatalf("LOSS for net %s, which is not < 0", m)
			}
		}
		if err := ValidateFeedback(m, class); err != nil {
			t.Fatalf("self-derived pair rejected: %v", err)
		}
		v = v.Add(step)
	}
}
