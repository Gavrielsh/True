package repository

// Unit coverage for the limit RULES. Both decisions a limit makes — "does this
// stake fit" and "is this change an increase" — are pure functions, so they are
// exercised exhaustively here and the integration suite is left to prove the one
// property that needs a database: that concurrent wagers cannot race past a cap.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// limDec / limMoney: the package's tests already define `dec` and `money` for
// other purposes, so these carry a prefix rather than shadowing them.
func limDec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func limMoney(t *testing.T, s string) domain.Money {
	t.Helper()
	m, err := domain.MoneyFromString(s)
	if err != nil {
		t.Fatalf("MoneyFromString(%q): %v", s, err)
	}
	return m
}

func TestAssertStakeWithinLimits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rows    []limitRow
		stake   string
		wantErr bool
	}{
		{"no limits set admits anything", nil, "1000.0000", false},
		{
			"comfortably inside",
			[]limitRow{{Kind: LimitWager, Period: PeriodDaily, Effective: limDec("100"), Used: limDec("10")}},
			"5.0000", false,
		},
		{
			// The boundary is inclusive: a limit of 100 permits reaching exactly
			// 100, not 99.9999. Off by one here and a player can never spend the
			// last unit of the cap they chose.
			"exactly at the limit is admitted",
			[]limitRow{{Kind: LimitWager, Period: PeriodDaily, Effective: limDec("100"), Used: limDec("90")}},
			"10.0000", false,
		},
		{
			"one unit over is refused",
			[]limitRow{{Kind: LimitWager, Period: PeriodDaily, Effective: limDec("100"), Used: limDec("90")}},
			"10.0001", true,
		},
		{
			"already consumed leaves nothing",
			[]limitRow{{Kind: LimitLoss, Period: PeriodDaily, Effective: limDec("50"), Used: limDec("50")}},
			"0.0001", true,
		},
		{
			// A zero limit is a real choice — "I do not want to wager at all" —
			// and must actually stop every wager rather than being treated as
			// "unset".
			"a zero limit stops everything",
			[]limitRow{{Kind: LimitWager, Period: PeriodDaily, Effective: limDec("0"), Used: limDec("0")}},
			"0.0001", true,
		},
		{
			// Negative usage is how a winning period is stored for LOSS. It must
			// genuinely extend headroom, or a player who is up cannot play.
			"a winning period extends loss headroom",
			[]limitRow{{Kind: LimitLoss, Period: PeriodDaily, Effective: limDec("50"), Used: limDec("-30")}},
			"80.0000", false,
		},
		{
			"but not without bound",
			[]limitRow{{Kind: LimitLoss, Period: PeriodDaily, Effective: limDec("50"), Used: limDec("-30")}},
			"80.0001", true,
		},
		{
			// Every limit binds independently: the tightest one decides. A stake
			// that fits the daily cap but breaks the weekly one is refused.
			"the tightest of several limits binds",
			[]limitRow{
				{Kind: LimitWager, Period: PeriodDaily, Effective: limDec("100"), Used: limDec("0")},
				{Kind: LimitWager, Period: PeriodWeekly, Effective: limDec("200"), Used: limDec("195")},
				{Kind: LimitLoss, Period: PeriodMonthly, Effective: limDec("500"), Used: limDec("0")},
			},
			"10.0000", true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertStakeWithinLimits(c.rows, limMoney(t, c.stake))
			if c.wantErr && !errors.Is(err, errs.ErrLimitExceeded) {
				t.Errorf("got %v, want ErrLimitExceeded", err)
			}
			if !c.wantErr && err != nil {
				t.Errorf("got %v, want nil", err)
			}
		})
	}
}

// TestDirectionFor pins the classification that decides whether a player waits.
//
// The first-limit case is the one worth stating explicitly: a player who has
// never set a limit is not protected by anything, so making their FIRST limit
// serve a 24-hour cooling-off period would leave them unprotected for a day as
// the direct consequence of deciding to protect themselves.
func TestDirectionFor(t *testing.T) {
	t.Parallel()

	fifty := limDec("50")

	cases := []struct {
		name      string
		previous  *decimal.Decimal
		requested string
		want      limitDirection
	}{
		{"a first limit applies at once", nil, "100", limitDirectionSet},
		{"a first limit of zero applies at once", nil, "0", limitDirectionSet},
		{"lower is a decrease", &fifty, "25", limitDirectionDecrease},
		{"zero is a decrease", &fifty, "0", limitDirectionDecrease},
		{"higher is an increase", &fifty, "75", limitDirectionIncrease},
		{"the same value changes nothing", &fifty, "50", limitDirectionUnchanged},
		{
			// Scale must not create a phantom change: 50 and 50.0000 are the
			// same cap, and re-submitting one should not be recorded as a
			// decision.
			"trailing zeros are the same value",
			&fifty, "50.0000", limitDirectionUnchanged,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := directionFor(c.previous, limDec(c.requested)); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestSetPlayerLimitRequest_Validate(t *testing.T) {
	t.Parallel()

	valid := func() SetPlayerLimitRequest {
		return SetPlayerLimitRequest{
			OperatorCode: "OP1",
			PlayerID:     uuid.New(),
			Kind:         LimitLoss,
			Period:       PeriodDaily,
			Amount:       limMoney(t, "50.0000"),
			ActorType:    "PLAYER",
		}
	}

	cases := []struct {
		name    string
		mutate  func(*SetPlayerLimitRequest)
		wantErr error
	}{
		{"valid", func(*SetPlayerLimitRequest) {}, nil},
		{
			// Not an error: a player is entitled to cap themselves at nothing.
			"zero is a legitimate limit",
			func(r *SetPlayerLimitRequest) { r.Amount = limMoney(t, "0.0000") }, nil,
		},
		{"empty operator", func(r *SetPlayerLimitRequest) { r.OperatorCode = "" }, errs.ErrInvalidAmount},
		{"nil player", func(r *SetPlayerLimitRequest) { r.PlayerID = uuid.Nil }, errs.ErrPlayerNotFound},
		{"unknown kind", func(r *SetPlayerLimitRequest) { r.Kind = "DEPOSIT" }, errs.ErrInvalidAmount},
		{"unknown period", func(r *SetPlayerLimitRequest) { r.Period = "HOURLY" }, errs.ErrInvalidAmount},
		{"unknown actor", func(r *SetPlayerLimitRequest) { r.ActorType = "ROBOT" }, errs.ErrInvalidAmount},
		{
			"over-long actor_ref",
			func(r *SetPlayerLimitRequest) { r.ActorRef = repeat('y', maxTransitionActorRefLen+1) },
			errs.ErrInvalidAmount,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := valid()
			c.mutate(&r)
			r.normalize()
			err := r.validate()
			switch {
			case c.wantErr == nil && err != nil:
				t.Errorf("got %v, want nil", err)
			case c.wantErr != nil && !errors.Is(err, c.wantErr):
				t.Errorf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestSetPlayerLimitRequest_Normalize(t *testing.T) {
	t.Parallel()

	r := SetPlayerLimitRequest{
		Kind:      "  loss ",
		Period:    " daily ",
		ActorType: " player ",
		ActorRef:  "  ticket-3  ",
	}
	r.normalize()

	if r.Kind != LimitLoss || r.Period != PeriodDaily || r.ActorType != "PLAYER" {
		t.Errorf("normalize: kind=%q period=%q actor=%q", r.Kind, r.Period, r.ActorType)
	}
	if r.ActorRef != "ticket-3" {
		t.Errorf("ActorRef = %q", r.ActorRef)
	}
}

// TestLimitIncreaseCoolOffIsTwentyFourHours states the constant directly.
// Shortening it is a policy change with the same evidential weight as shortening
// the self-exclusion floor, so it has to be argued for here rather than tuned
// away in configuration.
func TestLimitIncreaseCoolOffIsTwentyFourHours(t *testing.T) {
	t.Parallel()
	if got, want := LimitIncreaseCoolOff.Hours(), 24.0; got != want {
		t.Errorf("LimitIncreaseCoolOff = %v hours, want %v", got, want)
	}
}
