package domain

import (
	"errors"
	"testing"

	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Compact wallet constructor used throughout this file.
// Strings are parsed with MoneyFromString; anything malformed fails the test.
func mkWallet(t *testing.T, gc, scU, scR string) Wallet {
	t.Helper()
	gcM, err := MoneyFromString(gc)
	if err != nil { t.Fatalf("gc: %v", err) }
	uM, err := MoneyFromString(scU)
	if err != nil { t.Fatalf("scU: %v", err) }
	rM, err := MoneyFromString(scR)
	if err != nil { t.Fatalf("scR: %v", err) }
	return Wallet{
		PlayerID:     "00000000-0000-0000-0000-000000000001",
		GC:           gcM,
		SCUnplayed:   uM,
		SCRedeemable: rM,
	}
}

func mustMoney(t *testing.T, s string) Money {
	t.Helper()
	m, err := MoneyFromString(s)
	if err != nil { t.Fatalf("money %q: %v", s, err) }
	return m
}

// ----------------------------------------------------------------------------
// AllocateBet — GC family
// ----------------------------------------------------------------------------

func TestAllocateBet_GC(t *testing.T) {
	t.Parallel()
	type want struct {
		debits  []Debit
		errKind error // nil for success
	}
	tests := []struct {
		name   string
		wallet Wallet
		amount string
		want   want
	}{
		{
			name:   "sufficient",
			wallet: mkWallet(t, "100.0000", "0.0000", "0.0000"),
			amount: "10.0000",
			want: want{debits: []Debit{
				{CurrencyGC, mustMoney(t, "10.0000")},
			}},
		},
		{
			name:   "exact_balance",
			wallet: mkWallet(t, "10.0000", "0.0000", "0.0000"),
			amount: "10.0000",
			want: want{debits: []Debit{
				{CurrencyGC, mustMoney(t, "10.0000")},
			}},
		},
		{
			name:   "fractional",
			wallet: mkWallet(t, "1.2345", "0.0000", "0.0000"),
			amount: "0.0001",
			want: want{debits: []Debit{
				{CurrencyGC, mustMoney(t, "0.0001")},
			}},
		},
		{
			name:    "insufficient",
			wallet:  mkWallet(t, "5.0000", "100.0000", "100.0000"),
			amount:  "10.0000",
			want:    want{errKind: errs.ErrInsufficientFunds},
		},
		{
			name:    "empty_gc",
			wallet:  mkWallet(t, "0.0000", "999.0000", "999.0000"),
			amount:  "0.0001",
			want:    want{errKind: errs.ErrInsufficientFunds},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			amount := mustMoney(t, tt.amount)
			got, err := tt.wallet.AllocateBet(FamilyGC, amount)
			checkBetResult(t, got, err, tt.want.debits, tt.want.errKind, FamilyGC, amount)
		})
	}
}

// ----------------------------------------------------------------------------
// AllocateBet — SC family (the critical priority rule)
// ----------------------------------------------------------------------------

func TestAllocateBet_SC_PriorityRule(t *testing.T) {
	t.Parallel()
	type want struct {
		debits  []Debit
		errKind error
	}
	tests := []struct {
		name   string
		wallet Wallet
		amount string
		want   want
	}{
		{
			name:   "fully_from_unplayed",
			wallet: mkWallet(t, "0", "50.0000", "100.0000"),
			amount: "10.0000",
			want: want{debits: []Debit{
				{CurrencySCUnplayed, mustMoney(t, "10.0000")},
			}},
		},
		{
			name:   "exactly_drains_unplayed",
			wallet: mkWallet(t, "0", "50.0000", "100.0000"),
			amount: "50.0000",
			want: want{debits: []Debit{
				{CurrencySCUnplayed, mustMoney(t, "50.0000")},
			}},
		},
		{
			name:   "splits_unplayed_then_redeemable",
			wallet: mkWallet(t, "0", "30.0000", "100.0000"),
			amount: "50.0000",
			want: want{debits: []Debit{
				{CurrencySCUnplayed,   mustMoney(t, "30.0000")},
				{CurrencySCRedeemable, mustMoney(t, "20.0000")},
			}},
		},
		{
			name:   "no_unplayed_falls_through",
			wallet: mkWallet(t, "0", "0.0000", "100.0000"),
			amount: "10.0000",
			want: want{debits: []Debit{
				{CurrencySCRedeemable, mustMoney(t, "10.0000")},
			}},
		},
		{
			name:   "exact_total_sc",
			wallet: mkWallet(t, "0", "30.0000", "20.0000"),
			amount: "50.0000",
			want: want{debits: []Debit{
				{CurrencySCUnplayed,   mustMoney(t, "30.0000")},
				{CurrencySCRedeemable, mustMoney(t, "20.0000")},
			}},
		},
		{
			name:   "tiny_amount_from_unplayed",
			wallet: mkWallet(t, "0", "0.0001", "999.0000"),
			amount: "0.0001",
			want: want{debits: []Debit{
				{CurrencySCUnplayed, mustMoney(t, "0.0001")},
			}},
		},
		{
			name:   "tiny_amount_splits_at_boundary",
			wallet: mkWallet(t, "0", "0.0001", "999.0000"),
			amount: "0.0002",
			want: want{debits: []Debit{
				{CurrencySCUnplayed,   mustMoney(t, "0.0001")},
				{CurrencySCRedeemable, mustMoney(t, "0.0001")},
			}},
		},
		{
			name:   "no_sc_at_all",
			wallet: mkWallet(t, "999.0000", "0.0000", "0.0000"),
			amount: "0.0001",
			want:   want{errKind: errs.ErrInsufficientFunds},
		},
		{
			name:   "amount_exceeds_combined",
			wallet: mkWallet(t, "0", "10.0000", "10.0000"),
			amount: "20.0001",
			want:   want{errKind: errs.ErrInsufficientFunds},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			amount := mustMoney(t, tt.amount)
			got, err := tt.wallet.AllocateBet(FamilySC, amount)
			checkBetResult(t, got, err, tt.want.debits, tt.want.errKind, FamilySC, amount)
		})
	}
}

// ----------------------------------------------------------------------------
// AllocateBet — validation
// ----------------------------------------------------------------------------

func TestAllocateBet_Validation(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "100.0000", "100.0000", "100.0000")

	tests := []struct {
		name    string
		family  CurrencyFamily
		amount  string
		errKind error
	}{
		{"zero_amount_gc",   FamilyGC,      "0.0000",  errs.ErrInvalidAmount},
		{"zero_amount_sc",   FamilySC,      "0.0000",  errs.ErrInvalidAmount},
		{"negative_amount",  FamilyGC,      "-1.0000", errs.ErrInvalidAmount},
		{"negative_eps",     FamilySC,      "-0.0001", errs.ErrInvalidAmount},
		{"unknown_family",   FamilyUnknown, "1.0000",  errs.ErrUnsupportedCurrency},
		{"out_of_range",     CurrencyFamily(99), "1.0000", errs.ErrUnsupportedCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			amount := mustMoney(t, tt.amount)
			_, err := w.AllocateBet(tt.family, amount)
			if !errors.Is(err, tt.errKind) {
				t.Fatalf("want %v, got %v", tt.errKind, err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// AllocateWin
// ----------------------------------------------------------------------------

func TestAllocateWin(t *testing.T) {
	t.Parallel()
	w := mkWallet(t, "0", "0", "0")

	tests := []struct {
		name     string
		family   CurrencyFamily
		amount   string
		credit   Credit
		errKind  error
	}{
		{
			name:   "gc_win",
			family: FamilyGC,
			amount: "10.0000",
			credit: Credit{CurrencyGC, mustMoney(t, "10.0000")},
		},
		{
			name:   "sc_win_routes_to_redeemable",
			family: FamilySC,
			amount: "5.0000",
			credit: Credit{CurrencySCRedeemable, mustMoney(t, "5.0000")},
		},
		{
			name:   "sc_win_tiny_still_redeemable",
			family: FamilySC,
			amount: "0.0001",
			credit: Credit{CurrencySCRedeemable, mustMoney(t, "0.0001")},
		},
		{
			name:    "zero_amount_rejected",
			family:  FamilySC,
			amount:  "0.0000",
			errKind: errs.ErrInvalidAmount,
		},
		{
			name:    "negative_amount_rejected",
			family:  FamilyGC,
			amount:  "-0.0001",
			errKind: errs.ErrInvalidAmount,
		},
		{
			name:    "unknown_family_rejected",
			family:  FamilyUnknown,
			amount:  "1.0000",
			errKind: errs.ErrUnsupportedCurrency,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			amount := mustMoney(t, tt.amount)
			got, err := w.AllocateWin(tt.family, amount)
			if tt.errKind != nil {
				if !errors.Is(err, tt.errKind) {
					t.Fatalf("want %v, got %v", tt.errKind, err)
				}
				return
			}
			if err != nil { t.Fatalf("unexpected: %v", err) }
			if got.Family != tt.family {
				t.Errorf("Family: got %v want %v", got.Family, tt.family)
			}
			if !got.Total.Equal(amount) {
				t.Errorf("Total: got %s want %s", got.Total, amount)
			}
			if got.Credit.Currency != tt.credit.Currency {
				t.Errorf("Credit.Currency: got %s want %s", got.Credit.Currency, tt.credit.Currency)
			}
			if !got.Credit.Amount.Equal(tt.credit.Amount) {
				t.Errorf("Credit.Amount: got %s want %s", got.Credit.Amount, tt.credit.Amount)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// ApplyBet / ApplyWin — post-state derivation
// ----------------------------------------------------------------------------

func TestApplyBet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		wallet         Wallet
		family         CurrencyFamily
		amount         string
		wantGC, wantU, wantR string
	}{
		{
			name:   "gc_deduction",
			wallet: mkWallet(t, "100.0000", "50.0000", "50.0000"),
			family: FamilyGC, amount: "30.0000",
			wantGC: "70.0000", wantU: "50.0000", wantR: "50.0000",
		},
		{
			name:   "sc_drains_only_unplayed",
			wallet: mkWallet(t, "10.0000", "100.0000", "50.0000"),
			family: FamilySC, amount: "40.0000",
			wantGC: "10.0000", wantU: "60.0000", wantR: "50.0000",
		},
		{
			name:   "sc_split_both_buckets",
			wallet: mkWallet(t, "10.0000", "20.0000", "100.0000"),
			family: FamilySC, amount: "50.0000",
			wantGC: "10.0000", wantU: "0.0000", wantR: "70.0000",
		},
		{
			name:   "sc_only_redeemable",
			wallet: mkWallet(t, "10.0000", "0.0000", "50.0000"),
			family: FamilySC, amount: "25.0000",
			wantGC: "10.0000", wantU: "0.0000", wantR: "25.0000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			amount := mustMoney(t, tt.amount)
			alloc, err := tt.wallet.AllocateBet(tt.family, amount)
			if err != nil { t.Fatalf("AllocateBet: %v", err) }
			post := tt.wallet.ApplyBet(alloc)
			if got := post.GC.String();           got != tt.wantGC { t.Errorf("GC = %s want %s", got, tt.wantGC) }
			if got := post.SCUnplayed.String();   got != tt.wantU  { t.Errorf("U  = %s want %s", got, tt.wantU)  }
			if got := post.SCRedeemable.String(); got != tt.wantR  { t.Errorf("R  = %s want %s", got, tt.wantR)  }

			// ApplyBet must be a pure function: the original wallet is unchanged.
			if tt.wallet.GC.Equal(post.GC) && tt.wallet.SCUnplayed.Equal(post.SCUnplayed) && tt.wallet.SCRedeemable.Equal(post.SCRedeemable) {
				t.Errorf("ApplyBet did not change the wallet — bug in test setup")
			}
		})
	}
}

func TestApplyWin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		wallet         Wallet
		family         CurrencyFamily
		amount         string
		wantGC, wantU, wantR string
	}{
		{
			name:   "gc_credit",
			wallet: mkWallet(t, "100.0000", "50.0000", "50.0000"),
			family: FamilyGC, amount: "30.0000",
			wantGC: "130.0000", wantU: "50.0000", wantR: "50.0000",
		},
		{
			name:   "sc_credit_routes_to_redeemable_only",
			wallet: mkWallet(t, "100.0000", "50.0000", "50.0000"),
			family: FamilySC, amount: "30.0000",
			wantGC: "100.0000", wantU: "50.0000", wantR: "80.0000",
		},
		{
			name:   "sc_win_on_empty_wallet",
			wallet: mkWallet(t, "0.0000", "0.0000", "0.0000"),
			family: FamilySC, amount: "0.0001",
			wantGC: "0.0000", wantU: "0.0000", wantR: "0.0001",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			amount := mustMoney(t, tt.amount)
			alloc, err := tt.wallet.AllocateWin(tt.family, amount)
			if err != nil { t.Fatalf("AllocateWin: %v", err) }
			post := tt.wallet.ApplyWin(alloc)
			if got := post.GC.String();           got != tt.wantGC { t.Errorf("GC = %s want %s", got, tt.wantGC) }
			if got := post.SCUnplayed.String();   got != tt.wantU  { t.Errorf("U  = %s want %s", got, tt.wantU)  }
			if got := post.SCRedeemable.String(); got != tt.wantR  { t.Errorf("R  = %s want %s", got, tt.wantR)  }
		})
	}
}

// ----------------------------------------------------------------------------
// Helper
// ----------------------------------------------------------------------------

func checkBetResult(
	t *testing.T,
	got BetAllocation,
	err error,
	wantDebits []Debit,
	wantErr error,
	wantFamily CurrencyFamily,
	wantTotal Money,
) {
	t.Helper()
	if wantErr != nil {
		if !errors.Is(err, wantErr) {
			t.Fatalf("want err %v, got %v", wantErr, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Family != wantFamily {
		t.Errorf("Family: got %v want %v", got.Family, wantFamily)
	}
	if !got.Total.Equal(wantTotal) {
		t.Errorf("Total: got %s want %s", got.Total, wantTotal)
	}

	// Invariant: sum of debits equals Total.
	sum := ZeroMoney()
	for _, d := range got.Debits {
		sum = sum.Add(d.Amount)
	}
	if !sum.Equal(wantTotal) {
		t.Errorf("Σ debits = %s, want %s", sum, wantTotal)
	}

	if len(got.Debits) != len(wantDebits) {
		t.Fatalf("debit count: got %d (%+v) want %d (%+v)",
			len(got.Debits), got.Debits, len(wantDebits), wantDebits)
	}
	for i, d := range wantDebits {
		if got.Debits[i].Currency != d.Currency {
			t.Errorf("debit[%d] currency: got %s want %s", i, got.Debits[i].Currency, d.Currency)
		}
		if !got.Debits[i].Amount.Equal(d.Amount) {
			t.Errorf("debit[%d] amount: got %s want %s", i, got.Debits[i].Amount, d.Amount)
		}
	}
}
