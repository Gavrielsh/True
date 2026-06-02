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
