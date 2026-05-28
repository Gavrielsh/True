package domain

import "testing"

func TestCurrencyValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		c    Currency
		want bool
	}{
		{CurrencyGC, true},
		{CurrencySCUnplayed, true},
		{CurrencySCRedeemable, true},
		{Currency(""), false},
		{Currency("USD"), false},
		{Currency("gc"), false}, // case-sensitive: must match DB ENUM exactly
		{Currency("SC"), false}, // family label, not a currency value
	}
	for _, tt := range tests {
		t.Run(string(tt.c), func(t *testing.T) {
			t.Parallel()
			if got := tt.c.Valid(); got != tt.want {
				t.Errorf("%q.Valid() = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

func TestCurrencyFamily(t *testing.T) {
	t.Parallel()
	tests := []struct {
		f     CurrencyFamily
		str   string
		valid bool
	}{
		{FamilyGC, "GC", true},
		{FamilySC, "SC", true},
		{FamilyUnknown, "UNKNOWN", false},
		{CurrencyFamily(99), "UNKNOWN", false},
	}
	for _, tt := range tests {
		t.Run(tt.str, func(t *testing.T) {
			t.Parallel()
			if got := tt.f.String(); got != tt.str {
				t.Errorf("String: got %q want %q", got, tt.str)
			}
			if got := tt.f.Valid(); got != tt.valid {
				t.Errorf("Valid: got %v want %v", got, tt.valid)
			}
		})
	}
}

func TestFamilyFromString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want CurrencyFamily
	}{
		{"GC", FamilyGC},
		{"SC", FamilySC},
		{"", FamilyUnknown},
		{"gc", FamilyUnknown}, // case-sensitive on purpose
		{"USD", FamilyUnknown},
		{"SC_UNPLAYED", FamilyUnknown}, // raw currency leaks must not pass
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := FamilyFromString(tt.in); got != tt.want {
				t.Errorf("FamilyFromString(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
