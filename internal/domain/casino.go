package domain

import (
	"fmt"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

// This file holds the pure allocation rules for the real-money wrapper
// operations (store purchase and redemption). Like the bet/win allocators in
// wallet.go they are I/O-free, value-receiver methods that compute a plan
// WITHOUT mutating the wallet, so the repository can run them inside the
// SELECT ... FOR UPDATE window with a bounded, predictable lock duration.
//
// Sweepstakes rules enforced here:
//   - Purchase: a fiat purchase issues GC (the product) and may grant
//     SC_UNPLAYED as a promo. It NEVER issues SC_REDEEMABLE directly —
//     redeemable tokens are only ever WON through gameplay.
//   - Redeem: a fiat redemption draws EXCLUSIVELY from SC_REDEEMABLE. It must
//     never touch SC_UNPLAYED (promo tokens are not cashable). This is the
//     mirror image of the bet rule and a hard legal requirement.

// ----------------------------------------------------------------------------
// Purchase allocation
// ----------------------------------------------------------------------------

// PurchaseAllocation is the credit plan for a store purchase. Credits holds one
// line per non-zero currency (GC always; SC_UNPLAYED only when a promo is
// granted) so the repository can post exactly the ledger rows that carry a
// positive amount (ledger_entries enforces amount > 0).
//
// Invariants (validated by AllocatePurchase):
//   - GC >= 0, SCPromo >= 0, and GC + SCPromo > 0.
//   - len(Credits) >= 1; every Credit.Amount is strictly positive.
//   - Credits never contains SC_REDEEMABLE (issuance can't mint cashable SC).
type PurchaseAllocation struct {
	GC      Money
	SCPromo Money
	Credits []Credit
}

// AllocatePurchase computes the credit plan for a purchase of `gc` Gold Coins
// plus `scPromo` promotional SC_UNPLAYED, without mutating the wallet.
//
// Errors:
//   - ErrInvalidAmount — a negative component, or a zero-value purchase.
func (w Wallet) AllocatePurchase(gc, scPromo Money) (PurchaseAllocation, error) {
	if gc.IsNegative() || scPromo.IsNegative() {
		return PurchaseAllocation{}, fmt.Errorf(
			"%w: purchase amounts must be >= 0 (gc=%s, sc_promo=%s)",
			errs.ErrInvalidAmount, gc, scPromo,
		)
	}
	if !gc.IsPositive() && !scPromo.IsPositive() {
		return PurchaseAllocation{}, fmt.Errorf(
			"%w: purchase must issue a positive GC and/or SC_UNPLAYED amount",
			errs.ErrInvalidAmount,
		)
	}

	credits := make([]Credit, 0, 2)
	if gc.IsPositive() {
		credits = append(credits, Credit{Currency: CurrencyGC, Amount: gc})
	}
	if scPromo.IsPositive() {
		credits = append(credits, Credit{Currency: CurrencySCUnplayed, Amount: scPromo})
	}
	return PurchaseAllocation{GC: gc, SCPromo: scPromo, Credits: credits}, nil
}

// ApplyPurchase returns a new Wallet with the purchase credited. GC and the
// SC_UNPLAYED promo are added; SC_REDEEMABLE is never touched.
func (w Wallet) ApplyPurchase(a PurchaseAllocation) Wallet {
	out := w
	for _, c := range a.Credits {
		switch c.Currency {
		case CurrencyGC:
			out.GC = out.GC.Add(c.Amount)
		case CurrencySCUnplayed:
			out.SCUnplayed = out.SCUnplayed.Add(c.Amount)
		case CurrencySCRedeemable:
			// Unreachable: AllocatePurchase never emits SC_REDEEMABLE. Guarded
			// anyway so a future change can't silently mint cashable tokens.
			out.SCRedeemable = out.SCRedeemable.Add(c.Amount)
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Redemption allocation
// ----------------------------------------------------------------------------

// RedeemAllocation is the debit plan for a redemption. It is always a single
// SC_REDEEMABLE debit — redemptions cannot draw on SC_UNPLAYED.
type RedeemAllocation struct {
	Debit Debit
}

// AllocateRedeem computes the SC_REDEEMABLE debit for a redemption of `amount`,
// without mutating the wallet.
//
// Errors:
//   - ErrInvalidAmount     — amount is zero or negative.
//   - ErrInsufficientFunds — SC_REDEEMABLE alone cannot cover amount. The
//     SC_UNPLAYED balance is deliberately NOT considered: promo tokens are not
//     redeemable, so a wallet rich in SC_UNPLAYED but short on SC_REDEEMABLE
//     still fails here.
func (w Wallet) AllocateRedeem(amount Money) (RedeemAllocation, error) {
	if !amount.IsPositive() {
		return RedeemAllocation{}, fmt.Errorf(
			"%w: redeem amount must be > 0, got %s",
			errs.ErrInvalidAmount, amount,
		)
	}
	if w.SCRedeemable.LessThan(amount) {
		return RedeemAllocation{}, fmt.Errorf(
			"%w: SC_REDEEMABLE balance %s < redeem %s (SC_UNPLAYED is not redeemable)",
			errs.ErrInsufficientFunds, w.SCRedeemable, amount,
		)
	}
	return RedeemAllocation{
		Debit: Debit{Currency: CurrencySCRedeemable, Amount: amount},
	}, nil
}

// ApplyRedeem returns a new Wallet with the redemption deducted from
// SC_REDEEMABLE.
func (w Wallet) ApplyRedeem(a RedeemAllocation) Wallet {
	out := w
	out.SCRedeemable = out.SCRedeemable.Sub(a.Debit.Amount)
	return out
}

// RedemptionRefundAllocation is the SC_REDEEMABLE credit that returns what a
// redemption took but never paid out.
type RedemptionRefundAllocation struct {
	Credit Credit
}

// AllocateRedemptionRefund computes the SC_REDEEMABLE credit for refunding a
// redemption, without mutating the wallet.
//
// THE MIRROR OF AllocateRedeem, AND DELIBERATELY NOT ITS EQUAL.
// A redemption can fail for want of balance; a REFUND cannot. The money being
// returned was already taken from this wallet, so there is no sufficiency test
// to perform and no way for the player's current balance to make the refund
// invalid. The only thing that can be wrong here is the amount itself.
//
// It credits SC_REDEEMABLE, not SC_UNPLAYED, because that is the bucket the
// redemption debited. Returning it as unplayed would silently impose a fresh
// playthrough requirement on money the player had already made redeemable —
// taking their money and then, on giving it back, making it harder to withdraw.
func (w Wallet) AllocateRedemptionRefund(amount Money) (RedemptionRefundAllocation, error) {
	if !amount.IsPositive() {
		return RedemptionRefundAllocation{}, fmt.Errorf(
			"%w: refund amount must be > 0, got %s",
			errs.ErrInvalidAmount, amount,
		)
	}
	return RedemptionRefundAllocation{
		Credit: Credit{Currency: CurrencySCRedeemable, Amount: amount},
	}, nil
}

// ApplyRedemptionRefund returns a new Wallet with the refund credited to
// SC_REDEEMABLE.
func (w Wallet) ApplyRedemptionRefund(a RedemptionRefundAllocation) Wallet {
	out := w
	out.SCRedeemable = out.SCRedeemable.Add(a.Credit.Amount)
	return out
}

// ----------------------------------------------------------------------------
// Promotional grant allocation (AMOE and other no-purchase issuance)
// ----------------------------------------------------------------------------

// PromoGrantAllocation is the credit plan for a no-purchase coin issuance: an
// AMOE mail-in entry, a marketing bonus, or a goodwill credit. Credits holds one
// line per non-zero currency so the repository posts exactly the ledger rows
// that carry a positive amount (ledger_entries enforces amount > 0).
//
// Invariants (validated by AllocatePromoGrant):
//   - GC >= 0, SC >= 0, and GC + SC > 0.
//   - len(Credits) >= 1; every Credit.Amount is strictly positive.
//   - Credits NEVER contains SC_REDEEMABLE.
type PromoGrantAllocation struct {
	GC      Money
	SC      Money
	Credits []Credit
}

// AllocatePromoGrant computes the credit plan for issuing `gc` Gold Coins and
// `sc` Sweeps Coins with no purchase behind them, without mutating the wallet.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS IS A SEPARATE ALLOCATOR AND NOT AllocatePurchase
// ─────────────────────────────────────────────────────────────────────────────
// The two compute the same shape today, and that is precisely why they are
// apart. AllocatePurchase's rules answer to the commercial product: a future
// bundle could reasonably issue something new. This allocator answers to the
// sweepstakes statute, and its "never SC_REDEEMABLE" rule must not be able to
// move as a side effect of a change to what a coin package contains. Sharing
// one function would couple a legal guarantee to a pricing decision.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY SC LANDS IN SC_UNPLAYED, AND WHY THAT IS THE *FAIR* CHOICE
// ─────────────────────────────────────────────────────────────────────────────
// SC_REDEEMABLE is reachable only by winning (AllocateWin). A free-entry path
// that credited it directly would be a cash withdrawal channel requiring neither
// purchase nor gameplay — trivially drainable, and indefensible in every
// direction at once.
//
// Crediting SC_UNPLAYED is also what makes AMOE honest rather than a worse deal.
// A purchaser's promotional SC lands in SC_UNPLAYED under a 1x wagering
// requirement; the free entrant's lands in the same bucket under the same
// requirement, recorded by the same sc_playthrough machinery. Equal dignity —
// the free route offering materially the same chance as the paid one — is the
// condition that keeps a sweepstakes from being a lottery, and it is met here by
// the two paths being genuinely identical from the wallet down.
//
// Errors:
//   - ErrInvalidAmount — a negative component, or a zero-value grant.
func (w Wallet) AllocatePromoGrant(gc, sc Money) (PromoGrantAllocation, error) {
	if gc.IsNegative() || sc.IsNegative() {
		return PromoGrantAllocation{}, fmt.Errorf(
			"%w: promo grant amounts must be >= 0 (gc=%s, sc=%s)",
			errs.ErrInvalidAmount, gc, sc,
		)
	}
	if !gc.IsPositive() && !sc.IsPositive() {
		return PromoGrantAllocation{}, fmt.Errorf(
			"%w: promo grant must issue a positive GC and/or SC_UNPLAYED amount",
			errs.ErrInvalidAmount,
		)
	}

	credits := make([]Credit, 0, 2)
	if gc.IsPositive() {
		credits = append(credits, Credit{Currency: CurrencyGC, Amount: gc})
	}
	if sc.IsPositive() {
		credits = append(credits, Credit{Currency: CurrencySCUnplayed, Amount: sc})
	}
	return PromoGrantAllocation{GC: gc, SC: sc, Credits: credits}, nil
}

// ApplyPromoGrant returns a new Wallet with the grant credited.
//
// SC_REDEEMABLE is not merely "not credited" here — it is UNREACHABLE. The
// switch has no SC_REDEEMABLE case, so even a hand-built allocation carrying one
// (which AllocatePromoGrant cannot produce) would be dropped rather than
// applied. ApplyPurchase guards the same currency by adding it, since a purchase
// allocation is trusted input from a path that validates it; here the stricter
// reading is the right one, because this is the path with no payment behind it.
func (w Wallet) ApplyPromoGrant(a PromoGrantAllocation) Wallet {
	out := w
	for _, c := range a.Credits {
		switch c.Currency {
		case CurrencyGC:
			out.GC = out.GC.Add(c.Amount)
		case CurrencySCUnplayed:
			out.SCUnplayed = out.SCUnplayed.Add(c.Amount)
		case CurrencySCRedeemable:
			// Deliberately no-op. See the doc comment above: a no-purchase path
			// must not be able to mint cashable tokens even by mistake.
		}
	}
	return out
}
