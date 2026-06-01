package domain

import (
	"fmt"

	errs "github.com/Gavrielsh/TransactionMechanism/pkg/errors"
)

// Wallet is the in-memory projection of one row from the `wallets` table.
// It is a value type — methods take a value receiver and return new Wallets
// for mutations — so passing it across the lock boundary cannot accidentally
// leak a stale reference back into the caller.
//
// Lock-duration contract:
//
//	The repository acquires SELECT ... FOR UPDATE, builds a Wallet, then
//	calls AllocateBet / AllocateWin. Those calls are pure CPU (no I/O, no
//	allocator-heavy paths) and complete in microseconds, so the row lock
//	is held only for SELECT + math + UPDATE + INSERT-ledger + COMMIT.
type Wallet struct {
	PlayerID     string
	GC           Money
	SCUnplayed   Money
	SCRedeemable Money
}

// SCTotal returns the combined SC liquidity (SC_UNPLAYED + SC_REDEEMABLE).
// Used by the SC bet allocator to decide whether the wallet can cover the wager.
func (w Wallet) SCTotal() Money {
	return w.SCUnplayed.Add(w.SCRedeemable)
}

// BalanceFor returns the wallet column matching the given currency. Used by
// the repository to fill `balance_after` on per-currency ledger entries.
// Returns ZeroMoney for unrecognised currencies (callers should validate
// upstream via Currency.Valid()).
func (w Wallet) BalanceFor(c Currency) Money {
	switch c {
	case CurrencyGC:
		return w.GC
	case CurrencySCUnplayed:
		return w.SCUnplayed
	case CurrencySCRedeemable:
		return w.SCRedeemable
	}
	return ZeroMoney()
}

// ----------------------------------------------------------------------------
// Bet allocation
// ----------------------------------------------------------------------------

// Debit is one deduction line of an allocation. A BetAllocation may emit one
// or two debits depending on how the SC bet straddles SC_UNPLAYED /
// SC_REDEEMABLE.
type Debit struct {
	Currency Currency
	Amount   Money
}

// BetAllocation is the complete debit plan for a single bet.
//
// Invariants (validated by AllocateBet, relied on by the repository):
//   - len(Debits) >= 1
//   - Sum(Debits[*].Amount) == Total == requested bet amount
//   - Family == FamilyGC  → exactly one Debit, Currency == CurrencyGC.
//   - Family == FamilySC  → 1–2 Debits; if two, [0] is SC_UNPLAYED and
//     [1] is SC_REDEEMABLE (engine-enforced priority order).
type BetAllocation struct {
	Family CurrencyFamily
	Total  Money
	Debits []Debit
}

// AllocateBet computes the debit plan for a wager of `amount` from `family`
// without mutating the wallet. SC bets exhaust SC_UNPLAYED before drawing on
// SC_REDEEMABLE, per the US sweepstakes wagering rule.
//
// Errors:
//   - ErrInvalidAmount       — amount is zero or negative.
//   - ErrInsufficientFunds   — the relevant balance(s) cannot cover amount.
//   - ErrUnsupportedCurrency — family is not GC or SC.
func (w Wallet) AllocateBet(family CurrencyFamily, amount Money) (BetAllocation, error) {
	if !amount.IsPositive() {
		return BetAllocation{}, fmt.Errorf(
			"%w: bet amount must be > 0, got %s",
			errs.ErrInvalidAmount, amount,
		)
	}

	switch family {
	case FamilyGC:
		if w.GC.LessThan(amount) {
			return BetAllocation{}, fmt.Errorf(
				"%w: GC balance %s < bet %s",
				errs.ErrInsufficientFunds, w.GC, amount,
			)
		}
		return BetAllocation{
			Family: FamilyGC,
			Total:  amount,
			Debits: []Debit{{Currency: CurrencyGC, Amount: amount}},
		}, nil

	case FamilySC:
		if w.SCTotal().LessThan(amount) {
			return BetAllocation{}, fmt.Errorf(
				"%w: SC balance %s < bet %s (unplayed=%s, redeemable=%s)",
				errs.ErrInsufficientFunds, w.SCTotal(), amount,
				w.SCUnplayed, w.SCRedeemable,
			)
		}
		// Priority: take everything we can from SC_UNPLAYED first; the
		// remainder (if any) draws against SC_REDEEMABLE.
		fromUnplayed := MinMoney(w.SCUnplayed, amount)
		fromRedeemable := amount.Sub(fromUnplayed)

		debits := make([]Debit, 0, 2)
		if fromUnplayed.IsPositive() {
			debits = append(debits, Debit{
				Currency: CurrencySCUnplayed,
				Amount:   fromUnplayed,
			})
		}
		if fromRedeemable.IsPositive() {
			debits = append(debits, Debit{
				Currency: CurrencySCRedeemable,
				Amount:   fromRedeemable,
			})
		}
		return BetAllocation{
			Family: FamilySC,
			Total:  amount,
			Debits: debits,
		}, nil

	default:
		return BetAllocation{}, fmt.Errorf(
			"%w: family=%d", errs.ErrUnsupportedCurrency, family,
		)
	}
}

// ApplyBet returns a new Wallet with the BetAllocation deducted. Used by
// the repository after a successful AllocateBet to derive the post-state
// balances written to ledger_entries.balance_after.
//
// ApplyBet does not revalidate — AllocateBet already produced a feasible
// plan against this exact wallet, and the FOR UPDATE lock guarantees no
// concurrent mutation between AllocateBet and ApplyBet.
func (w Wallet) ApplyBet(a BetAllocation) Wallet {
	out := w
	for _, d := range a.Debits {
		switch d.Currency {
		case CurrencyGC:
			out.GC = out.GC.Sub(d.Amount)
		case CurrencySCUnplayed:
			out.SCUnplayed = out.SCUnplayed.Sub(d.Amount)
		case CurrencySCRedeemable:
			out.SCRedeemable = out.SCRedeemable.Sub(d.Amount)
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Win allocation
// ----------------------------------------------------------------------------

// Credit is one increment line of a win.
type Credit struct {
	Currency Currency
	Amount   Money
}

// WinAllocation is the credit plan for a single win.
//
// Invariants:
//   - Total == requested win amount.
//   - Family == FamilyGC  → Credit.Currency == CurrencyGC.
//   - Family == FamilySC  → Credit.Currency == CurrencySCRedeemable  (always).
type WinAllocation struct {
	Family CurrencyFamily
	Total  Money
	Credit Credit
}

// AllocateWin computes the credit instruction for a win of `amount` in
// `family`. SC winnings ALWAYS credit SC_REDEEMABLE; the operator does not
// get to choose otherwise (this is what makes the won tokens eligible for
// real-world prize redemption).
//
// Errors:
//   - ErrInvalidAmount       — amount is zero or negative.
//   - ErrUnsupportedCurrency — family is not GC or SC.
//
// Note: there is no ErrInsufficientFunds path — wins only ever add liquidity.
func (w Wallet) AllocateWin(family CurrencyFamily, amount Money) (WinAllocation, error) {
	if !amount.IsPositive() {
		return WinAllocation{}, fmt.Errorf(
			"%w: win amount must be > 0, got %s",
			errs.ErrInvalidAmount, amount,
		)
	}

	switch family {
	case FamilyGC:
		return WinAllocation{
			Family: FamilyGC,
			Total:  amount,
			Credit: Credit{Currency: CurrencyGC, Amount: amount},
		}, nil
	case FamilySC:
		return WinAllocation{
			Family: FamilySC,
			Total:  amount,
			Credit: Credit{Currency: CurrencySCRedeemable, Amount: amount},
		}, nil
	default:
		return WinAllocation{}, fmt.Errorf(
			"%w: family=%d", errs.ErrUnsupportedCurrency, family,
		)
	}
}

// ApplyWin returns a new Wallet with the WinAllocation credited.
func (w Wallet) ApplyWin(a WinAllocation) Wallet {
	out := w
	switch a.Credit.Currency {
	case CurrencyGC:
		out.GC = out.GC.Add(a.Credit.Amount)
	case CurrencySCRedeemable:
		out.SCRedeemable = out.SCRedeemable.Add(a.Credit.Amount)
	}
	return out
}
