package repository

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/game"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

func money(t *testing.T, s string) domain.Money {
	t.Helper()
	m, err := domain.MoneyFromString(s)
	if err != nil {
		t.Fatalf("MoneyFromString(%q): %v", s, err)
	}
	return m
}

func validSpinReq() SpinRequest {
	return SpinRequest{
		OperatorCode:          "OP1",
		OperatorTransactionID: "spin-1",
		PlayerID:              uuid.New(),
		Family:                domain.FamilySC,
		BetAmount:             domain.MoneyFromInt(1),
	}
}

// TestSpinRequestHasNoWinField is a STRUCTURAL guard on the core security
// property: the caller must not be able to name a payout. If someone adds a
// win-shaped field to SpinRequest, this test fails and explains why.
//
// The vulnerability being locked out: /win accepted an unbounded caller
// amount, so a leaked webhook secret could mint arbitrary SC_REDEEMABLE.
// /spin closes that by deriving the payout server-side — which only holds
// while no caller-supplied field can reach it.
func TestSpinRequestHasNoWinField(t *testing.T) {
	forbidden := []string{"win", "multiplier", "outcome", "reel", "payout", "credit"}

	b, err := json.Marshal(SpinRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// SpinRequest has no json tags, so reflectively check the Go field names.
	var fields []string
	for _, f := range structFieldNames(SpinRequest{}) {
		fields = append(fields, strings.ToLower(f))
	}

	for _, name := range fields {
		for _, bad := range forbidden {
			// BetAmount legitimately contains no forbidden token; anything
			// that does is a caller-controlled payout vector.
			if strings.Contains(name, bad) {
				t.Errorf("SpinRequest.%s looks caller-controlled and payout-shaped (matched %q). "+
					"The server MUST derive the win; do not accept it from the wire. Payload: %s",
					name, bad, b)
			}
		}
	}
}

func structFieldNames(v any) []string {
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out = append(out, rt.Field(i).Name)
	}
	return out
}

func TestSpinRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SpinRequest)
		wantErr error
	}{
		{"valid", func(*SpinRequest) {}, nil},
		{"empty operator", func(r *SpinRequest) { r.OperatorCode = "" }, errs.ErrInvalidAmount},
		{"empty tx id", func(r *SpinRequest) { r.OperatorTransactionID = "" }, errs.ErrInvalidAmount},
		{"nil player", func(r *SpinRequest) { r.PlayerID = uuid.Nil }, errs.ErrPlayerNotFound},
		{"bad family", func(r *SpinRequest) { r.Family = domain.FamilyUnknown }, errs.ErrUnsupportedCurrency},
		{"zero bet", func(r *SpinRequest) { r.BetAmount = domain.ZeroMoney() }, errs.ErrInvalidAmount},
		{"negative bet", func(r *SpinRequest) { r.BetAmount = money(t, "-5") }, errs.ErrInvalidAmount},
		{"bet above ledger bound", func(r *SpinRequest) {
			r.BetAmount = domain.MoneyFromInt(domain.MaxMoneyUnits)
		}, errs.ErrInvalidAmount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validSpinReq()
			tc.mutate(&req)
			err := req.validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want wrapped %v", err, tc.wantErr)
			}
		})
	}
}

// TestSpinMetadataCannotForgeOutcome proves a caller cannot inject its own
// "outcome" into the audit record — the server's outcome always wins.
func TestSpinMetadataCannotForgeOutcome(t *testing.T) {
	real := game.Outcome{
		GameID:          "classic-3reel",
		PaytableVersion: "1.0.0",
		Reels:           []string{"CHERRY", "LEMON", "BELL"},
		Line:            game.LineNone,
		Multiplier:      decimal.Zero,
	}
	// A caller trying to pass off a jackpot as the recorded result.
	forged := []byte(`{"outcome":{"line":"THREE_OF_A_KIND","multiplier":"400"},"session":"abc"}`)

	merged, err := spinMetadata(forged, real)
	if err != nil {
		t.Fatalf("spinMetadata: %v", err)
	}

	var got struct {
		Outcome game.Outcome    `json:"outcome"`
		Session string          `json:"session"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	if got.Outcome.Line != game.LineNone {
		t.Errorf("caller forged the outcome: got line %s, want %s", got.Outcome.Line, game.LineNone)
	}
	if !got.Outcome.Multiplier.IsZero() {
		t.Errorf("caller forged the multiplier: got %s, want 0", got.Outcome.Multiplier)
	}
	// Non-colliding caller keys survive.
	if got.Session != "abc" {
		t.Errorf("caller metadata dropped: session=%q", got.Session)
	}
}

func TestSpinMetadataRejectsOversize(t *testing.T) {
	big := make(map[string]string, 40)
	for i := 0; i < 40; i++ {
		big[strings.Repeat("k", 10)+string(rune('a'+i))] = strings.Repeat("v", 20)
	}
	raw, err := json.Marshal(big)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := spinMetadata(raw, game.Outcome{Multiplier: decimal.Zero}); err == nil {
		t.Fatal("expected oversize metadata to be rejected before it reaches the DB CHECK")
	}
}

func TestSpinMetadataRejectsNonObject(t *testing.T) {
	if _, err := spinMetadata([]byte(`["not","an","object"]`), game.Outcome{Multiplier: decimal.Zero}); err == nil {
		t.Fatal("expected non-object metadata to be rejected")
	}
}

// TestWinCeilingBlocksOversizedProviderWin covers the third-party path that
// /spin does not replace: an aggregator-supplied win above the operator's
// ceiling must be refused before it touches Redis or Postgres.
func TestWinCeilingBlocksOversizedProviderWin(t *testing.T) {
	e := &engine{maxWin: money(t, "1000")}

	if err := e.checkWinCeiling(money(t, "1000")); err != nil {
		t.Errorf("win exactly at the ceiling must be allowed, got %v", err)
	}
	if err := e.checkWinCeiling(money(t, "999.9999")); err != nil {
		t.Errorf("win below the ceiling must be allowed, got %v", err)
	}

	err := e.checkWinCeiling(money(t, "1000.0001"))
	if err == nil {
		t.Fatal("win above the ceiling must be rejected")
	}
	if !errors.Is(err, errs.ErrWinExceedsCeiling) {
		t.Fatalf("got %v, want wrapped ErrWinExceedsCeiling", err)
	}
}

func TestWinCeilingDisabledWhenUnset(t *testing.T) {
	e := &engine{} // zero maxWin
	if err := e.checkWinCeiling(money(t, "999999999")); err != nil {
		t.Fatalf("a zero ceiling disables the check, got %v", err)
	}
}
