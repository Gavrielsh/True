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

func TestAllocateRedemptionRefund(t *testing.T) {
	t.Parallel()

	// The refund is NOT symmetric with AllocateRedeem, and that asymmetry is the
	// property worth pinning: a redemption can fail for want of balance, a refund
	// cannot. The money being returned was already taken from this wallet.
	empty := Wallet{}
	alloc, err := empty.AllocateRedemptionRefund(mustMoney(t, "100.0000"))
	if err != nil {
		t.Fatalf("refund against an empty wallet was refused: %v", err)
	}
	if alloc.Credit.Currency != CurrencySCRedeemable {
		t.Errorf("credited %s, want SC_REDEEMABLE — SC_UNPLAYED would impose a fresh playthrough",
			alloc.Credit.Currency)
	}
	if alloc.Credit.Amount.String() != "100.0000" {
		t.Errorf("credit amount %s, want 100.0000", alloc.Credit.Amount)
	}

	// Applying it moves ONLY the redeemable bucket.
	w := Wallet{GC: mustMoney(t, "5.0000"), SCUnplayed: mustMoney(t, "7.0000"), SCRedeemable: mustMoney(t, "1.0000")}
	got := w.ApplyRedemptionRefund(alloc)
	if got.SCRedeemable.String() != "101.0000" {
		t.Errorf("SCRedeemable %s, want 101.0000", got.SCRedeemable)
	}
	if got.GC.String() != "5.0000" || got.SCUnplayed.String() != "7.0000" {
		t.Errorf("other buckets moved: GC=%s SCUnplayed=%s", got.GC, got.SCUnplayed)
	}

	// A refund is exactly reversible against the redemption it undoes.
	base := Wallet{SCRedeemable: mustMoney(t, "250.0000")}
	redeemAlloc, err := base.AllocateRedeem(mustMoney(t, "125.5000"))
	if err != nil {
		t.Fatalf("AllocateRedeem: %v", err)
	}
	afterRedeem := base.ApplyRedeem(redeemAlloc)
	refundAlloc, err := afterRedeem.AllocateRedemptionRefund(mustMoney(t, "125.5000"))
	if err != nil {
		t.Fatalf("AllocateRedemptionRefund: %v", err)
	}
	restored := afterRedeem.ApplyRedemptionRefund(refundAlloc)
	if restored.SCRedeemable.String() != base.SCRedeemable.String() {
		t.Errorf("round trip left %s, want the original %s", restored.SCRedeemable, base.SCRedeemable)
	}
}

func TestAllocateRedemptionRefund_RejectsNonPositive(t *testing.T) {
	t.Parallel()
	w := Wallet{SCRedeemable: mustMoney(t, "10.0000")}

	for _, amount := range []string{"0", "0.0000"} {
		if _, err := w.AllocateRedemptionRefund(mustMoney(t, amount)); !errors.Is(err, errs.ErrInvalidAmount) {
			t.Errorf("amount %q: got %v, want ErrInvalidAmount", amount, err)
		}
	}
}

// ----------------------------------------------------------------------------
// Promotional grant allocation (AMOE)
// ----------------------------------------------------------------------------

func TestAllocatePromoGrant_CreditsGCAndSCUnplayed(t *testing.T) {
	t.Parallel()
	w := Wallet{
		GC:           mustMoney(t, "10.0000"),
		SCUnplayed:   mustMoney(t, "2.0000"),
		SCRedeemable: mustMoney(t, "5.0000"),
	}

	alloc, err := w.AllocatePromoGrant(mustMoney(t, "1000.0000"), mustMoney(t, "1.0000"))
	if err != nil {
		t.Fatalf("AllocatePromoGrant: %v", err)
	}
	if len(alloc.Credits) != 2 {
		t.Fatalf("credits: got %d want 2 (%+v)", len(alloc.Credits), alloc.Credits)
	}

	post := w.ApplyPromoGrant(alloc)
	if post.GC.String() != "1010.0000" {
		t.Errorf("GC: got %s want 1010.0000", post.GC)
	}
	if post.SCUnplayed.String() != "3.0000" {
		t.Errorf("SCUnplayed: got %s want 3.0000", post.SCUnplayed)
	}
	// The line that matters: a free entry must not move the cashable bucket.
	if post.SCRedeemable.String() != "5.0000" {
		t.Errorf("SCRedeemable moved to %s; a promo grant must never touch it", post.SCRedeemable)
	}
}

// A grant of SC alone is the ordinary AMOE shape: a mail-in entry yields Sweeps
// Coins, not a Gold Coin package.
func TestAllocatePromoGrant_SCOnlyIsTheAMOEShape(t *testing.T) {
	t.Parallel()
	w := Wallet{}

	alloc, err := w.AllocatePromoGrant(mustMoney(t, "0.0000"), mustMoney(t, "5.0000"))
	if err != nil {
		t.Fatalf("AllocatePromoGrant: %v", err)
	}
	if len(alloc.Credits) != 1 {
		t.Fatalf("credits: got %d want 1 (%+v)", len(alloc.Credits), alloc.Credits)
	}
	if alloc.Credits[0].Currency != CurrencySCUnplayed {
		t.Errorf("currency: got %s want SC_UNPLAYED", alloc.Credits[0].Currency)
	}

	post := w.ApplyPromoGrant(alloc)
	if post.SCUnplayed.String() != "5.0000" {
		t.Errorf("SCUnplayed: got %s want 5.0000", post.SCUnplayed)
	}
	if post.GC.String() != "0.0000" {
		t.Errorf("GC moved to %s on an SC-only grant", post.GC)
	}
}

// AllocatePromoGrant cannot express an SC_REDEEMABLE credit. This asserts the
// property directly rather than trusting the two call sites above to have
// covered every shape.
func TestAllocatePromoGrant_NeverEmitsRedeemable(t *testing.T) {
	t.Parallel()
	w := Wallet{}

	for _, tc := range []struct{ gc, sc string }{
		{"1.0000", "0.0000"},
		{"0.0000", "1.0000"},
		{"1.0000", "1.0000"},
		{"999999999999.9999", "999999999999.9999"},
	} {
		alloc, err := w.AllocatePromoGrant(mustMoney(t, tc.gc), mustMoney(t, tc.sc))
		if err != nil {
			t.Fatalf("AllocatePromoGrant(%s,%s): %v", tc.gc, tc.sc, err)
		}
		for _, c := range alloc.Credits {
			if c.Currency == CurrencySCRedeemable {
				t.Fatalf("grant(%s,%s) emitted an SC_REDEEMABLE credit — a no-purchase path must never mint cashable tokens", tc.gc, tc.sc)
			}
			if !c.Amount.IsPositive() {
				t.Errorf("grant(%s,%s) emitted a non-positive %s credit %s", tc.gc, tc.sc, c.Currency, c.Amount)
			}
		}
	}
}

// ApplyPromoGrant is the second line of defence: even handed an allocation that
// AllocatePromoGrant could not have produced, it must not credit SC_REDEEMABLE.
// This is the difference from ApplyPurchase, which adds that currency.
func TestApplyPromoGrant_DropsAHandBuiltRedeemableCredit(t *testing.T) {
	t.Parallel()
	w := Wallet{SCRedeemable: mustMoney(t, "7.0000")}

	forged := PromoGrantAllocation{
		Credits: []Credit{{Currency: CurrencySCRedeemable, Amount: mustMoney(t, "1000000.0000")}},
	}
	post := w.ApplyPromoGrant(forged)

	if post.SCRedeemable.String() != "7.0000" {
		t.Fatalf("SCRedeemable became %s; ApplyPromoGrant must drop a redeemable credit, not apply it", post.SCRedeemable)
	}
}

func TestAllocatePromoGrant_RejectsEmptyAndNegativeGrants(t *testing.T) {
	t.Parallel()
	w := Wallet{}

	// A grant of nothing is not a grant.
	if _, err := w.AllocatePromoGrant(mustMoney(t, "0.0000"), mustMoney(t, "0.0000")); !errors.Is(err, errs.ErrInvalidAmount) {
		t.Errorf("zero grant: got %v, want ErrInvalidAmount", err)
	}
	// A negative component would post a ledger entry the amount > 0 CHECK
	// refuses; caught here so the failure is a clean 400, not a 500 from the DB.
	neg := ZeroMoney().Sub(mustMoney(t, "1.0000"))
	if _, err := w.AllocatePromoGrant(neg, mustMoney(t, "1.0000")); !errors.Is(err, errs.ErrInvalidAmount) {
		t.Errorf("negative gc: got %v, want ErrInvalidAmount", err)
	}
	if _, err := w.AllocatePromoGrant(mustMoney(t, "1.0000"), neg); !errors.Is(err, errs.ErrInvalidAmount) {
		t.Errorf("negative sc: got %v, want ErrInvalidAmount", err)
	}
}

// Equal dignity at the allocator level: the free route and the paid route must
// put the same coins in the same buckets. If these ever diverge, an AMOE entrant
// is getting a materially different product from a purchaser — which is the
// condition that separates a lawful sweepstakes from a lottery.
func TestPromoGrantAndPurchase_CreditIdenticalBuckets(t *testing.T) {
	t.Parallel()
	w := Wallet{}
	gc, sc := mustMoney(t, "5000.0000"), mustMoney(t, "5.0000")

	purchaseAlloc, err := w.AllocatePurchase(gc, sc)
	if err != nil {
		t.Fatalf("AllocatePurchase: %v", err)
	}
	promoAlloc, err := w.AllocatePromoGrant(gc, sc)
	if err != nil {
		t.Fatalf("AllocatePromoGrant: %v", err)
	}

	bought := w.ApplyPurchase(purchaseAlloc)
	granted := w.ApplyPromoGrant(promoAlloc)

	if bought.GC.String() != granted.GC.String() {
		t.Errorf("GC: purchase gave %s, AMOE gave %s", bought.GC, granted.GC)
	}
	if bought.SCUnplayed.String() != granted.SCUnplayed.String() {
		t.Errorf("SC_UNPLAYED: purchase gave %s, AMOE gave %s", bought.SCUnplayed, granted.SCUnplayed)
	}
	if bought.SCRedeemable.String() != granted.SCRedeemable.String() {
		t.Errorf("SC_REDEEMABLE: purchase gave %s, AMOE gave %s", bought.SCRedeemable, granted.SCRedeemable)
	}
}
