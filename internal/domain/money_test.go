package domain

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

// --------------------------- NewMoney ---------------------------------------

func TestNewMoney(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string // parsed via decimal.RequireFromString for table compactness
		want    string // expected String() output (StringFixed(4))
		wantErr error  // nil for success, sentinel otherwise
	}{
		{"zero",                 "0",                "0.0000",          nil},
		{"positive_whole",       "100",              "100.0000",        nil},
		{"positive_one_decimal", "1.5",              "1.5000",          nil},
		{"positive_four_dec",    "1.2345",           "1.2345",          nil},
		{"trailing_zeros",       "1.2300",           "1.2300",          nil},
		{"negative_allowed",     "-5.5000",          "-5.5000",         nil},
		{"max_18_4",             "99999999999999.9999", "99999999999999.9999", nil},
		{"scale_5_rejected",     "0.00001",          "",                ErrMoneyScaleExceeded},
		{"scale_8_rejected",     "1.12345678",       "",                ErrMoneyScaleExceeded},
		{"sub_cent_boundary",    "0.00005",          "",                ErrMoneyScaleExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := decimal.RequireFromString(tt.input)
			m, err := NewMoney(d)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("NewMoney(%s): want %v, got %v", tt.input, tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewMoney(%s): unexpected error: %v", tt.input, err)
			}
			if got := m.String(); got != tt.want {
				t.Errorf("NewMoney(%s).String() = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --------------------------- MoneyFromString --------------------------------

func TestMoneyFromString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"plain_int",          "42",         "42.0000",        false},
		{"two_decimals",       "1.23",       "1.2300",         false},
		{"max_scale",          "1.2345",     "1.2345",         false},
		{"trailing_zeros_pad", "1.20",       "1.2000",         false},
		{"negative",           "-100.5000",  "-100.5000",      false},
		{"empty",              "",           "",               true},
		{"garbage",            "hello",      "",               true},
		{"too_much_precision", "1.00001",    "",               true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, err := MoneyFromString(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("MoneyFromString(%q): expected error, got money %s", tt.input, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("MoneyFromString(%q): unexpected error: %v", tt.input, err)
			}
			if got := m.String(); got != tt.want {
				t.Errorf("MoneyFromString(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

// --------------------------- Zero value & predicates ------------------------

func TestMoneyZeroValueIsUsable(t *testing.T) {
	t.Parallel()
	var m Money
	if !m.IsZero() {
		t.Errorf("zero-value Money should be zero, got %s", m)
	}
	if m.IsPositive() || m.IsNegative() {
		t.Errorf("zero-value Money should be neither positive nor negative")
	}
	if !m.Equal(ZeroMoney()) {
		t.Errorf("zero-value Money should equal ZeroMoney()")
	}
}

func TestMoneyPredicates(t *testing.T) {
	t.Parallel()
	one, _ := MoneyFromString("1.0000")
	zero := ZeroMoney()
	neg, _ := MoneyFromString("-0.0001")

	tests := []struct {
		name        string
		m           Money
		isZero      bool
		isPositive  bool
		isNegative  bool
	}{
		{"zero",     zero, true,  false, false},
		{"one",      one,  false, true,  false},
		{"neg_eps",  neg,  false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.m.IsZero() != tt.isZero {
				t.Errorf("%s: IsZero=%v want %v", tt.name, tt.m.IsZero(), tt.isZero)
			}
			if tt.m.IsPositive() != tt.isPositive {
				t.Errorf("%s: IsPositive=%v want %v", tt.name, tt.m.IsPositive(), tt.isPositive)
			}
			if tt.m.IsNegative() != tt.isNegative {
				t.Errorf("%s: IsNegative=%v want %v", tt.name, tt.m.IsNegative(), tt.isNegative)
			}
		})
	}
}

// --------------------------- Arithmetic -------------------------------------

func TestMoneyAddSub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		add  string
		sub  string
	}{
		{"whole_plus_whole",    "10",     "5",      "15.0000",  "5.0000"},
		{"decimals",            "1.2345", "0.0001", "1.2346",   "1.2344"},
		{"penny_precision",     "100.00", "0.01",   "100.0100", "99.9900"},
		{"sub_to_zero",         "5.0000", "5.0000", "10.0000",  "0.0000"},
		{"crosses_zero",        "5.0000", "10.0000","15.0000",  "-5.0000"},
		{"max_scale_no_loss",   "0.0001", "0.0001", "0.0002",   "0.0000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := MoneyFromString(tt.a)
			if err != nil { t.Fatalf("a parse: %v", err) }
			b, err := MoneyFromString(tt.b)
			if err != nil { t.Fatalf("b parse: %v", err) }

			if got := a.Add(b).String(); got != tt.add {
				t.Errorf("Add: got %s, want %s", got, tt.add)
			}
			if got := a.Sub(b).String(); got != tt.sub {
				t.Errorf("Sub: got %s, want %s", got, tt.sub)
			}
		})
	}
}

// Precision invariant: repeated tiny operations must NOT drift, the way
// they would under float64. This is the single most important test in this
// package — it's the reason we use shopspring/decimal at all.
func TestMoneyNoFloatDrift(t *testing.T) {
	t.Parallel()
	one := MoneyFromInt(1)
	step, _ := MoneyFromString("0.0001")

	// Accumulate ten-thousand 0.0001 increments; must land EXACTLY on 1.0000.
	acc := ZeroMoney()
	for i := 0; i < 10_000; i++ {
		acc = acc.Add(step)
	}
	if !acc.Equal(one) {
		t.Fatalf("10_000 * 0.0001 = %s, want exactly 1.0000 (drift detected)", acc)
	}
}

// --------------------------- Comparison -------------------------------------

func TestMoneyComparison(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		lt, gt, eq, lte, gte bool
	}{
		{"1.0000", "1.0000", false, false, true,  true,  true},
		{"0.9999", "1.0000", true,  false, false, true,  false},
		{"1.0001", "1.0000", false, true,  false, false, true},
		{"-1.0000","1.0000", true,  false, false, true,  false},
	}
	for _, tt := range tests {
		a, _ := MoneyFromString(tt.a)
		b, _ := MoneyFromString(tt.b)
		if a.LessThan(b) != tt.lt {
			t.Errorf("%s < %s: got %v, want %v", tt.a, tt.b, a.LessThan(b), tt.lt)
		}
		if a.GreaterThan(b) != tt.gt {
			t.Errorf("%s > %s: got %v, want %v", tt.a, tt.b, a.GreaterThan(b), tt.gt)
		}
		if a.Equal(b) != tt.eq {
			t.Errorf("%s == %s: got %v, want %v", tt.a, tt.b, a.Equal(b), tt.eq)
		}
		if a.LTE(b) != tt.lte {
			t.Errorf("%s <= %s: got %v, want %v", tt.a, tt.b, a.LTE(b), tt.lte)
		}
		if a.GTE(b) != tt.gte {
			t.Errorf("%s >= %s: got %v, want %v", tt.a, tt.b, a.GTE(b), tt.gte)
		}
	}
}

func TestMinMoney(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b, want string
	}{
		{"1.0000", "2.0000", "1.0000"},
		{"2.0000", "1.0000", "1.0000"},
		{"1.0000", "1.0000", "1.0000"},
		{"-1.0000","0.0000", "-1.0000"},
	}
	for _, tt := range tests {
		a, _ := MoneyFromString(tt.a)
		b, _ := MoneyFromString(tt.b)
		want, _ := MoneyFromString(tt.want)
		if got := MinMoney(a, b); !got.Equal(want) {
			t.Errorf("Min(%s, %s) = %s, want %s", tt.a, tt.b, got, want)
		}
	}
}

// --------------------------- Decimal round-trip -----------------------------

func TestMoneyDecimalRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []string{"0.0000", "1.2345", "100.0000", "-50.0001", "99999999999999.9999"}
	for _, s := range cases {
		m, err := MoneyFromString(s)
		if err != nil { t.Fatalf("%s: %v", s, err) }
		// The Decimal() return must stamp scale=-4 so pgx writes the canonical form.
		d := m.Decimal()
		if d.Exponent() != -MoneyScale {
			t.Errorf("%s: Decimal().Exponent() = %d, want %d", s, d.Exponent(), -MoneyScale)
		}
		if d.StringFixed(MoneyScale) != s {
			t.Errorf("%s: round-trip mismatch: %s", s, d.StringFixed(MoneyScale))
		}
	}
}
