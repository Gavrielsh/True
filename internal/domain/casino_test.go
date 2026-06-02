package domain

import (
	"errors"
	"testing"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

func money(t *testing.T, s string) Money {
	t.Helper()
	m, err := MoneyFromString(s)
	if err != nil {
		t.Fatalf("MoneyFromString(%q): %v", s, err)
	}
	return m
}

// ----------------------------------------------------------------------------
// AllocatePurchase
// ----------------------------------------------------------------------------

func TestAllocatePurchase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		gc          string
		scPromo     string
		wantErr     error
		wantCredits []Credit
	}{
		{
			name:    "gc_only",
			gc:      "100.0000",
			scPromo: "0.0000",
			wantCredits: []Credit{
				{Currency: CurrencyGC, Amount: money(t, "100.0000")},
			},
		},
		{
			name:    "gc_with_sc_promo",
			gc:      "100.0000",
			scPromo: "5.0000",
			wantCredits: []Credit{
				{Currency: CurrencyGC, Amount: money(t, "100.0000")},
				{Currency: CurrencySCUnplayed, Amount: money(t, "5.0000")},
			},
		},
		{
			name:    "promo_only_no_gc",
			gc:      "0.0000",
			scPromo: "5.0000",
			wantCredits: []Credit{
				{Currency: CurrencySCUnplayed, Amount: money(t, "5.0000")},
			},
		},
		{name: "both_zero_rejected", gc: "0.0000", scPromo: "0.0000", wantErr: errs.ErrInvalidAmount},
		{name: "negative_gc_rejected", gc: "-1.0000", scPromo: "0.0000", wantErr: errs.ErrInvalidAmount},
		{name: "negative_promo_rejected", gc: "100.0000", scPromo: "-1.0000", wantErr: errs.ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := Wallet{} // empty wallet — allocation never reads existing balances
			got, err := w.AllocatePurchase(money(t, tc.gc), money(t, tc.scPromo))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err: got %v want wrapping %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got.Credits) != len(tc.wantCredits) {
				t.Fatalf("credits: got %d (%+v) want %d", len(got.Credits), got.Credits, len(tc.wantCredits))
			}
			for i, c := range tc.wantCredits {
				if got.Credits[i].Currency != c.Currency || !got.Credits[i].Amount.Equal(c.Amount) {
					t.Errorf("credit[%d]: got %v %s want %v %s", i,
						got.Credits[i].Currency, got.Credits[i].Amount, c.Currency, c.Amount)
				}
			}
		})
	}
}

func TestApplyPurchase_NeverTouchesRedeemable(t *testing.T) {
	t.Parallel()
	w := Wallet{
		GC:           money(t, "10.0000"),
		SCUnplayed:   money(t, "2.0000"),
		SCRedeemable: money(t, "7.0000"),
	}
	alloc, err := w.AllocatePurchase(money(t, "100.0000"), money(t, "5.0000"))
	if err != nil {
		t.Fatalf("AllocatePurchase: %v", err)
	}
	post := w.ApplyPurchase(alloc)

	if post.GC.String() != "110.0000" {
		t.Errorf("GC: got %s want 110.0000", post.GC)
	}
	if post.SCUnplayed.String() != "7.0000" {
		t.Errorf("SCUnplayed: got %s want 7.0000", post.SCUnplayed)
	}
	// SC_REDEEMABLE must be untouched — issuance can never mint cashable tokens.
	if post.SCRedeemable.String() != "7.0000" {
		t.Errorf("SCRedeemable: got %s want 7.0000 (must be untouched)", post.SCRedeemable)
	}
}

// ----------------------------------------------------------------------------
// AllocateRedeem
// ----------------------------------------------------------------------------

func TestAllocateRedeem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		wallet    Wallet
		amount    string
		wantErr   error
		wantDebit string
	}{
		{
			name:      "exact_balance",
			wallet:    Wallet{SCRedeemable: money(t, "50.0000")},
			amount:    "50.0000",
			wantDebit: "50.0000",
		},
		{
			name:      "partial",
			wallet:    Wallet{SCRedeemable: money(t, "50.0000")},
			amount:    "20.0000",
			wantDebit: "20.0000",
		},
		{
			name:    "insufficient_redeemable",
			wallet:  Wallet{SCRedeemable: money(t, "10.0000")},
			amount:  "20.0000",
			wantErr: errs.ErrInsufficientFunds,
		},
		{
			// The crucial sweepstakes rule: a wallet flush with SC_UNPLAYED but
			// short on SC_REDEEMABLE still cannot redeem.
			name:    "unplayed_does_not_cover_redeem",
			wallet:  Wallet{SCUnplayed: money(t, "1000.0000"), SCRedeemable: money(t, "5.0000")},
			amount:  "10.0000",
			wantErr: errs.ErrInsufficientFunds,
		},
		{
			name:    "zero_rejected",
			wallet:  Wallet{SCRedeemable: money(t, "50.0000")},
			amount:  "0.0000",
			wantErr: errs.ErrInvalidAmount,
		},
		{
			name:    "negative_rejected",
			wallet:  Wallet{SCRedeemable: money(t, "50.0000")},
			amount:  "-5.0000",
			wantErr: errs.ErrInvalidAmount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.wallet.AllocateRedeem(money(t, tc.amount))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err: got %v want wrapping %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Debit.Currency != CurrencySCRedeemable {
				t.Errorf("debit currency: got %v want SC_REDEEMABLE", got.Debit.Currency)
			}
			if got.Debit.Amount.String() != tc.wantDebit {
				t.Errorf("debit amount: got %s want %s", got.Debit.Amount, tc.wantDebit)
			}
		})
	}
}

func TestApplyRedeem_OnlyReducesRedeemable(t *testing.T) {
	t.Parallel()
	w := Wallet{
		GC:           money(t, "10.0000"),
		SCUnplayed:   money(t, "20.0000"),
		SCRedeemable: money(t, "30.0000"),
	}
	alloc, err := w.AllocateRedeem(money(t, "12.0000"))
	if err != nil {
		t.Fatalf("AllocateRedeem: %v", err)
	}
	post := w.ApplyRedeem(alloc)

	if post.SCRedeemable.String() != "18.0000" {
		t.Errorf("SCRedeemable: got %s want 18.0000", post.SCRedeemable)
	}
	if post.GC.String() != "10.0000" || post.SCUnplayed.String() != "20.0000" {
		t.Errorf("GC/SCUnplayed must be untouched; got GC=%s SCU=%s", post.GC, post.SCUnplayed)
	}
}
