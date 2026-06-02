package domain

import (
	"errors"
	"testing"

	errs "github.com/Gavrielsh/TransactionMechanism/pkg/errors"
)

// ----------------------------------------------------------------------------
// AllocateEscrowReserve — draws EXCLUSIVELY from SC_REDEEMABLE.
// ----------------------------------------------------------------------------

func TestAllocateEscrowReserve_HappyPath(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "100.0000", "50.0000", "40.0000")

	res, err := w.AllocateEscrowReserve(mustMoney(t, "30.0000"))
	if err != nil {
		t.Fatalf("AllocateEscrowReserve: %v", err)
	}
	if res.Currency != CurrencySCRedeemable {
		t.Errorf("currency: got %s want SC_REDEEMABLE", res.Currency)
	}
	if !res.Amount.Equal(mustMoney(t, "30.0000")) {
		t.Errorf("amount: got %s want 30.0000", res.Amount)
	}

	post := w.ApplyEscrowReserve(res)
	if post.SCRedeemable.String() != "10.0000" {
		t.Errorf("post SC_REDEEMABLE: got %s want 10.0000", post.SCRedeemable)
	}
	// GC and SC_UNPLAYED are untouched — they cannot fund a withdrawal.
	if post.GC.String() != "100.0000" || post.SCUnplayed.String() != "50.0000" {
		t.Errorf("non-redeemable balances mutated: GC=%s SCU=%s", post.GC, post.SCUnplayed)
	}
}

// SC_UNPLAYED, however large, never covers an escrow reserve.
func TestAllocateEscrowReserve_IgnoresUnplayed(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "0.0000", "1000.0000", "5.0000")

	_, err := w.AllocateEscrowReserve(mustMoney(t, "10.0000"))
	if !errors.Is(err, errs.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds (SC_UNPLAYED must not count)", err)
	}
}

func TestAllocateEscrowReserve_ExactBalance(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "0.0000", "0.0000", "25.0000")
	res, err := w.AllocateEscrowReserve(mustMoney(t, "25.0000"))
	if err != nil {
		t.Fatalf("exact-balance reserve should succeed: %v", err)
	}
	if w.ApplyEscrowReserve(res).SCRedeemable.String() != "0.0000" {
		t.Error("exact reserve should drain SC_REDEEMABLE to 0.0000")
	}
}

func TestAllocateEscrowReserve_Rejects(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "0.0000", "0.0000", "5.0000")
	cases := []struct {
		name   string
		amount Money
		err    error
	}{
		{"zero", ZeroMoney(), errs.ErrInvalidAmount},
		{"over_balance", mustMoney(t, "5.0001"), errs.ErrInsufficientFunds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := w.AllocateEscrowReserve(tc.amount); !errors.Is(err, tc.err) {
				t.Errorf("got %v, want wrapping %v", err, tc.err)
			}
		})
	}
}

// Release is the exact inverse of reserve on the SC_REDEEMABLE column.
func TestApplyEscrowRelease_RestoresBalance(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "0.0000", "0.0000", "10.0000")
	res, err := w.AllocateEscrowReserve(mustMoney(t, "7.0000"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	reserved := w.ApplyEscrowReserve(res)
	if reserved.SCRedeemable.String() != "3.0000" {
		t.Fatalf("reserved SC_REDEEMABLE: got %s want 3.0000", reserved.SCRedeemable)
	}
	released := reserved.ApplyEscrowRelease(mustMoney(t, "7.0000"))
	if released.SCRedeemable.String() != "10.0000" {
		t.Errorf("released SC_REDEEMABLE: got %s want 10.0000 (reserve↔release must round-trip)", released.SCRedeemable)
	}
}
