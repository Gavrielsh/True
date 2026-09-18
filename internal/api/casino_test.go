package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Gavrielsh/True/internal/repository"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// fakeCasino implements repository.CasinoEngine with injectable behaviour.
type fakeCasino struct {
	create   func(context.Context, repository.CreatePlayerRequest) (repository.CreatePlayerResult, error)
	purchase func(context.Context, repository.PurchaseRequest) (repository.TxResult, error)
	redeem   func(context.Context, repository.RedeemRequest) (repository.TxResult, error)
	status   func(context.Context, repository.StatusTransitionRequest) (repository.StatusTransitionResult, error)
	setLimit func(context.Context, repository.SetPlayerLimitRequest) (repository.SetPlayerLimitResult, error)
	refund   func(context.Context, repository.RedemptionRefundRequest) (repository.TxResult, error)
	promo    func(context.Context, repository.PromoGrantRequest) (repository.TxResult, error)

	lastPurchase repository.PurchaseRequest
	lastRedeem   repository.RedeemRequest
	lastStatus   repository.StatusTransitionRequest
	lastLimit    repository.SetPlayerLimitRequest
	lastRefund   repository.RedemptionRefundRequest
	lastPromo    repository.PromoGrantRequest
}

func (f *fakeCasino) CreatePlayer(ctx context.Context, req repository.CreatePlayerRequest) (repository.CreatePlayerResult, error) {
	return f.create(ctx, req)
}
func (f *fakeCasino) ProcessPurchase(ctx context.Context, req repository.PurchaseRequest) (repository.TxResult, error) {
	f.lastPurchase = req
	return f.purchase(ctx, req)
}
func (f *fakeCasino) ProcessRedeem(ctx context.Context, req repository.RedeemRequest) (repository.TxResult, error) {
	f.lastRedeem = req
	return f.redeem(ctx, req)
}
func (f *fakeCasino) ProcessRedemptionRefund(ctx context.Context, req repository.RedemptionRefundRequest) (repository.TxResult, error) {
	f.lastRefund = req
	if f.refund == nil {
		return repository.TxResult{}, nil
	}
	return f.refund(ctx, req)
}
func (f *fakeCasino) ProcessPromoGrant(ctx context.Context, req repository.PromoGrantRequest) (repository.TxResult, error) {
	f.lastPromo = req
	if f.promo == nil {
		return repository.TxResult{}, nil
	}
	return f.promo(ctx, req)
}
func (f *fakeCasino) ProcessStatusTransition(ctx context.Context, req repository.StatusTransitionRequest) (repository.StatusTransitionResult, error) {
	f.lastStatus = req
	return f.status(ctx, req)
}
func (f *fakeCasino) ProcessSetPlayerLimit(ctx context.Context, req repository.SetPlayerLimitRequest) (repository.SetPlayerLimitResult, error) {
	f.lastLimit = req
	return f.setLimit(ctx, req)
}

// casinoRouter mounts the casino handlers behind a stub middleware that injects
// a verified operator code (simulating a successful HMAC pass).
func casinoRouter(c repository.CasinoEngine) *gin.Engine {
	r := gin.New()
	h := NewCasinoHandlers(c)
	inject := func(ctx *gin.Context) {
		ctx.Set(ginKeyOperator, "OP1")
		ctx.Request = ctx.Request.WithContext(withOperator(ctx.Request.Context(), "OP1"))
		ctx.Next()
	}
	g := r.Group("/api/v1", inject)
	g.POST("/player/create", h.CreatePlayer)
	g.POST("/store/purchase", h.Purchase)
	g.POST("/store/redeem", h.Redeem)
	g.POST("/store/redeem/refund", h.RedemptionRefund)
	g.POST("/store/promo-grant", h.PromoGrant)
	g.POST("/player/status", h.UpdateStatus)
	g.POST("/player/limits", h.SetPlayerLimit)
	return r
}

// ----------------------------------------------------------------------------
// player/create
// ----------------------------------------------------------------------------

func TestCreatePlayerHandler_Created201(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{create: func(_ context.Context, req repository.CreatePlayerRequest) (repository.CreatePlayerResult, error) {
		if req.ExternalID != "ext-1" {
			t.Errorf("external_id: got %q want ext-1", req.ExternalID)
		}
		return repository.CreatePlayerResult{
			PlayerID: playerID, Created: true,
			Balances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000")},
		}, nil
	}}
	r := casinoRouter(eng)

	w := doJSON(r, http.MethodPost, "/api/v1/player/create", `{"external_id":"ext-1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", w.Code, w.Body.String())
	}
	var resp createPlayerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PlayerID != playerID.String() || !resp.Created {
		t.Errorf("resp: got %+v", resp)
	}
}

func TestCreatePlayerHandler_Existing200(t *testing.T) {
	t.Parallel()
	eng := &fakeCasino{create: func(_ context.Context, _ repository.CreatePlayerRequest) (repository.CreatePlayerResult, error) {
		return repository.CreatePlayerResult{PlayerID: uuid.New(), Created: false,
			Balances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000")}}, nil
	}}
	r := casinoRouter(eng)
	w := doJSON(r, http.MethodPost, "/api/v1/player/create", `{"external_id":"ext-existing"}`)
	if w.Code != http.StatusOK {
		t.Errorf("idempotent replay: got %d want 200", w.Code)
	}
}

func TestCreatePlayerHandler_BadBodyAndConflict(t *testing.T) {
	t.Parallel()
	t.Run("missing_external_id", func(t *testing.T) {
		t.Parallel()
		eng := &fakeCasino{create: func(_ context.Context, _ repository.CreatePlayerRequest) (repository.CreatePlayerResult, error) {
			t.Error("engine must not be called on invalid body")
			return repository.CreatePlayerResult{}, nil
		}}
		w := doJSON(casinoRouter(eng), http.MethodPost, "/api/v1/player/create", `{}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status: got %d want 400", w.Code)
		}
	})
	t.Run("conflict_maps_409", func(t *testing.T) {
		t.Parallel()
		eng := &fakeCasino{create: func(_ context.Context, _ repository.CreatePlayerRequest) (repository.CreatePlayerResult, error) {
			return repository.CreatePlayerResult{}, errs.ErrTransactionConflict
		}}
		w := doJSON(casinoRouter(eng), http.MethodPost, "/api/v1/player/create", `{"external_id":"x","username":"taken"}`)
		if w.Code != http.StatusConflict {
			t.Errorf("status: got %d want 409", w.Code)
		}
	})
}

// ----------------------------------------------------------------------------
// store/purchase
// ----------------------------------------------------------------------------

func TestPurchaseHandler_HappyPath(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{purchase: func(_ context.Context, req repository.PurchaseRequest) (repository.TxResult, error) {
		return repository.TxResult{
			PlayerID: req.PlayerID, TransactionType: "DEPOSIT", Amount: req.GCAmount,
			PostBalances: repository.BalanceSummary{GC: mustMoney(t, "100.0000"), SCUnplayed: mustMoney(t, "5.0000"), SCRedeemable: mustMoney(t, "0.0000")},
			Status:       repository.StatusProcessed,
		}, nil
	}}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"op-pur-1","player_id":"` + playerID.String() + `","gc_amount":"100.0000","sc_promo_amount":"5.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/purchase", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	// Operator must come from the trusted HMAC context, amounts parsed precisely.
	if eng.lastPurchase.OperatorCode != "OP1" {
		t.Errorf("operator: got %q want OP1", eng.lastPurchase.OperatorCode)
	}
	if eng.lastPurchase.GCAmount.String() != "100.0000" || eng.lastPurchase.SCPromoAmount.String() != "5.0000" {
		t.Errorf("amounts: gc=%s promo=%s", eng.lastPurchase.GCAmount, eng.lastPurchase.SCPromoAmount)
	}
}

func TestPurchaseHandler_OptionalPromoDefaultsZero(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{purchase: func(_ context.Context, req repository.PurchaseRequest) (repository.TxResult, error) {
		return repository.TxResult{PlayerID: req.PlayerID, Status: repository.StatusProcessed,
			PostBalances: repository.BalanceSummary{GC: mustMoney(t, "10.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000")}}, nil
	}}
	r := casinoRouter(eng)
	// No sc_promo_amount field at all.
	body := `{"operator_transaction_id":"op-pur-2","player_id":"` + playerID.String() + `","gc_amount":"10.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/purchase", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if !eng.lastPurchase.SCPromoAmount.IsZero() {
		t.Errorf("promo default: got %s want 0", eng.lastPurchase.SCPromoAmount)
	}
}

func TestPurchaseHandler_InvalidInput(t *testing.T) {
	t.Parallel()
	validID := uuid.New().String()
	cases := []struct {
		name string
		body string
	}{
		{"missing_required", `{"player_id":"` + validID + `"}`},
		{"bad_player_id", `{"operator_transaction_id":"op","player_id":"nope","gc_amount":"1.0000"}`},
		{"bad_gc_amount", `{"operator_transaction_id":"op","player_id":"` + validID + `","gc_amount":"abc"}`},
		{"too_precise_promo", `{"operator_transaction_id":"op","player_id":"` + validID + `","gc_amount":"1.0000","sc_promo_amount":"1.00001"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeCasino{purchase: func(_ context.Context, _ repository.PurchaseRequest) (repository.TxResult, error) {
				t.Error("engine must not be called on invalid input")
				return repository.TxResult{}, nil
			}}
			w := doJSON(casinoRouter(eng), http.MethodPost, "/api/v1/store/purchase", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// ----------------------------------------------------------------------------
// store/redeem
// ----------------------------------------------------------------------------

func TestRedeemHandler_HappyPath(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{redeem: func(_ context.Context, req repository.RedeemRequest) (repository.TxResult, error) {
		return repository.TxResult{
			PlayerID: req.PlayerID, TransactionType: "WITHDRAWAL", Family: "SC", Amount: req.Amount,
			PostBalances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "30.0000")},
			Status:       repository.StatusProcessed,
		}, nil
	}}
	r := casinoRouter(eng)
	body := `{"operator_transaction_id":"op-red-1","player_id":"` + playerID.String() + `","amount":"20.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/redeem", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if eng.lastRedeem.OperatorCode != "OP1" || eng.lastRedeem.Amount.String() != "20.0000" {
		t.Errorf("redeem req: op=%q amount=%s", eng.lastRedeem.OperatorCode, eng.lastRedeem.Amount)
	}
}

func TestRedeemHandler_InsufficientMaps400(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{redeem: func(_ context.Context, _ repository.RedeemRequest) (repository.TxResult, error) {
		return repository.TxResult{}, errs.ErrInsufficientFunds
	}}
	r := casinoRouter(eng)
	body := `{"operator_transaction_id":"op-red-2","player_id":"` + playerID.String() + `","amount":"999.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/redeem", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
	if got := decodeErr(t, w).Code; got != errs.CodeInsufficientFunds {
		t.Errorf("code: got %s want %s", got, errs.CodeInsufficientFunds)
	}
}

// ----------------------------------------------------------------------------
// player/status
// ----------------------------------------------------------------------------

func TestUpdateStatusHandler_HappyPath(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	until := time.Now().UTC().Add(repository.MinSelfExclusionTerm)
	eng := &fakeCasino{status: func(_ context.Context, req repository.StatusTransitionRequest) (repository.StatusTransitionResult, error) {
		return repository.StatusTransitionResult{
			PlayerID: req.PlayerID, TransitionID: uuid.New(),
			FromStatus: repository.StatusActive, ToStatus: req.ToStatus,
			SelfExclusionUntil: &until, OccurredAt: time.Now().UTC(),
		}, nil
	}}
	r := casinoRouter(eng)
	body := `{"player_id":"` + playerID.String() + `","to_status":"SELF_EXCLUDED",` +
		`"actor_type":"PLAYER","actor_ref":"ticket-9","reason":"player requested an exclusion",` +
		`"self_exclusion_days":180}`
	w := doJSON(r, http.MethodPost, "/api/v1/player/status", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}

	// Every field must reach the engine unaltered — an actor or a reason
	// silently dropped here would produce an audit row that misattributes a
	// compliance action.
	got := eng.lastStatus
	if got.OperatorCode != "OP1" {
		t.Errorf("operator_code = %q, want OP1", got.OperatorCode)
	}
	if got.PlayerID != playerID {
		t.Errorf("player_id = %s, want %s", got.PlayerID, playerID)
	}
	if got.ToStatus != repository.StatusSelfExcluded {
		t.Errorf("to_status = %q", got.ToStatus)
	}
	if got.ActorType != "PLAYER" || got.ActorRef != "ticket-9" {
		t.Errorf("actor = %q/%q", got.ActorType, got.ActorRef)
	}
	if got.Reason != "player requested an exclusion" {
		t.Errorf("reason = %q", got.Reason)
	}
	if got.SelfExclusionDays != repository.MinSelfExclusionDays {
		t.Errorf("self_exclusion_days = %d, want %d", got.SelfExclusionDays, repository.MinSelfExclusionDays)
	}
}

// TestUpdateStatusHandler_ErrorMapping pins the status code each compliance
// refusal produces. These codes drive an operator's retry policy: a 409 means
// "already done, stop", a 422 means "never going to work, a human must look".
// Getting them the wrong way round would have an integration retry an
// irrevocable exclusion forever.
func TestUpdateStatusHandler_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   errs.Code
	}{
		{"self-exclusion in force", errs.ErrSelfExclusionActive, http.StatusUnprocessableEntity, errs.CodeSelfExclusionActive},
		{"term too short", errs.ErrSelfExclusionTooShort, http.StatusUnprocessableEntity, errs.CodeSelfExclusionTooShort},
		{"illegal transition", errs.ErrStatusTransitionInvalid, http.StatusUnprocessableEntity, errs.CodeStatusTransitionInvalid},
		{"already in that status", errs.ErrStatusUnchanged, http.StatusConflict, errs.CodeStatusUnchanged},
		{"unknown player", errs.ErrPlayerNotFound, http.StatusNotFound, errs.CodePlayerNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeCasino{status: func(_ context.Context, _ repository.StatusTransitionRequest) (repository.StatusTransitionResult, error) {
				return repository.StatusTransitionResult{}, c.err
			}}
			r := casinoRouter(eng)
			body := `{"player_id":"` + uuid.New().String() + `","to_status":"ACTIVE",` +
				`"actor_type":"OPERATOR","reason":"attempting a transition"}`
			w := doJSON(r, http.MethodPost, "/api/v1/player/status", body)
			if w.Code != c.wantStatus {
				t.Errorf("status: got %d want %d", w.Code, c.wantStatus)
			}
			if got := decodeErr(t, w).Code; got != c.wantCode {
				t.Errorf("code: got %s want %s", got, c.wantCode)
			}
		})
	}
}

// TestUpdateStatusHandler_RejectsMalformedRequests: the engine is never reached
// for a request that cannot be a valid transition.
func TestUpdateStatusHandler_RejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"bad player id", `{"player_id":"not-a-uuid","to_status":"SUSPENDED","actor_type":"OPERATOR","reason":"x"}`},
		{"missing to_status", `{"player_id":"` + uuid.New().String() + `","actor_type":"OPERATOR","reason":"x"}`},
		{"missing actor_type", `{"player_id":"` + uuid.New().String() + `","to_status":"SUSPENDED","reason":"x"}`},
		{"missing reason", `{"player_id":"` + uuid.New().String() + `","to_status":"SUSPENDED","actor_type":"OPERATOR"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			called := false
			eng := &fakeCasino{status: func(_ context.Context, _ repository.StatusTransitionRequest) (repository.StatusTransitionResult, error) {
				called = true
				return repository.StatusTransitionResult{}, nil
			}}
			r := casinoRouter(eng)
			w := doJSON(r, http.MethodPost, "/api/v1/player/status", c.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
			}
			if called {
				t.Error("a malformed request reached the engine")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// player/limits
// ----------------------------------------------------------------------------

func TestSetPlayerLimitHandler_HappyPath(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{setLimit: func(_ context.Context, req repository.SetPlayerLimitRequest) (repository.SetPlayerLimitResult, error) {
		return repository.SetPlayerLimitResult{
			PlayerID: req.PlayerID, ChangeID: uuid.New(),
			Kind: req.Kind, Period: req.Period,
			Direction: "DECREASE", Amount: req.Amount,
		}, nil
	}}
	r := casinoRouter(eng)
	body := `{"player_id":"` + playerID.String() + `","limit_kind":"LOSS","period":"DAILY",` +
		`"amount":"25.5000","actor_type":"PLAYER","actor_ref":"rg-page"}`
	w := doJSON(r, http.MethodPost, "/api/v1/player/limits", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}

	got := eng.lastLimit
	if got.OperatorCode != "OP1" {
		t.Errorf("operator_code = %q, want OP1 (from the verified HMAC context)", got.OperatorCode)
	}
	if got.PlayerID != playerID || got.Kind != "LOSS" || got.Period != "DAILY" {
		t.Errorf("request = %+v", got)
	}
	// The limit must arrive as an exact decimal. A float round-trip here would
	// silently shift the cap that decides whether a player may wager.
	if got.Amount.String() != "25.5000" {
		t.Errorf("amount = %s, want 25.5000", got.Amount)
	}
	if got.ActorType != "PLAYER" || got.ActorRef != "rg-page" {
		t.Errorf("actor = %q/%q", got.ActorType, got.ActorRef)
	}
}

// TestSetPlayerLimitHandler_IncreaseReportsWhatIsActuallyInForce guards the
// field a client is most likely to render wrongly: during a cooling-off period
// `amount` is the OLD cap, and `pending_amount` is what the player asked for. A
// UI that showed the pending value would tell a player they have headroom they
// do not have.
func TestSetPlayerLimitHandler_IncreaseReportsWhatIsActuallyInForce(t *testing.T) {
	t.Parallel()
	effectiveAt := time.Now().UTC().Add(repository.LimitIncreaseCoolOff)
	pending := mustMoney(t, "500.0000")
	eng := &fakeCasino{setLimit: func(_ context.Context, req repository.SetPlayerLimitRequest) (repository.SetPlayerLimitResult, error) {
		return repository.SetPlayerLimitResult{
			PlayerID: req.PlayerID, ChangeID: uuid.New(),
			Kind: req.Kind, Period: req.Period, Direction: "INCREASE",
			Amount:        mustMoney(t, "10.0000"),
			PendingAmount: &pending,
			EffectiveAt:   &effectiveAt,
		}, nil
	}}
	r := casinoRouter(eng)
	body := `{"player_id":"` + uuid.New().String() + `","limit_kind":"WAGER","period":"DAILY",` +
		`"amount":"500.0000","actor_type":"PLAYER"}`
	w := doJSON(r, http.MethodPost, "/api/v1/player/limits", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}

	var resp setPlayerLimitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.Amount.String() != "10.0000" {
		t.Errorf("amount = %s, want the cap actually in force (10.0000)", resp.Result.Amount)
	}
	if resp.Result.PendingAmount == nil || resp.Result.PendingAmount.String() != "500.0000" {
		t.Errorf("pending_amount = %v, want 500.0000", resp.Result.PendingAmount)
	}
	if resp.Result.EffectiveAt == nil {
		t.Error("an increase must report when it becomes effective")
	}
}

func TestSetPlayerLimitHandler_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   errs.Code
	}{
		{"already at that value", errs.ErrStatusUnchanged, http.StatusConflict, errs.CodeStatusUnchanged},
		{"unknown player", errs.ErrPlayerNotFound, http.StatusNotFound, errs.CodePlayerNotFound},
		{"player not active", errs.ErrPlayerNotActive, http.StatusForbidden, errs.CodePlayerNotActive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeCasino{setLimit: func(_ context.Context, _ repository.SetPlayerLimitRequest) (repository.SetPlayerLimitResult, error) {
				return repository.SetPlayerLimitResult{}, c.err
			}}
			body := `{"player_id":"` + uuid.New().String() + `","limit_kind":"LOSS","period":"DAILY",` +
				`"amount":"10.0000","actor_type":"PLAYER"}`
			w := doJSON(casinoRouter(eng), http.MethodPost, "/api/v1/player/limits", body)
			if w.Code != c.wantStatus {
				t.Errorf("status: got %d want %d", w.Code, c.wantStatus)
			}
			if got := decodeErr(t, w).Code; got != c.wantCode {
				t.Errorf("code: got %s want %s", got, c.wantCode)
			}
		})
	}
}

// TestLimitExceededIsNotInsufficientFunds pins the distinction a client acts on.
//
// The money IS there — the player asked us not to let them spend it. Mapping
// this to INSUFFICIENT_FUNDS would have the UI tell a player to top up, which is
// the exact opposite of what a limit is for.
func TestLimitExceededIsNotInsufficientFunds(t *testing.T) {
	t.Parallel()
	eng := &fakeEngine{bet: func(_ context.Context, _ repository.BetRequest) (repository.TxResult, error) {
		return repository.TxResult{}, errs.ErrLimitExceeded
	}}
	r := handlerRouter(eng)
	body := `{"operator_transaction_id":"lim-1","player_id":"` + uuid.New().String() +
		`","currency":"SC","amount":"10.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/bet", body)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d want 403; body=%s", w.Code, w.Body.String())
	}
	got := decodeErr(t, w).Code
	if got == errs.CodeInsufficientFunds {
		t.Fatal("a limit refusal was reported as INSUFFICIENT_FUNDS; the client would tell the player to top up")
	}
	if got != errs.CodeLimitExceeded {
		t.Errorf("code: got %s want %s", got, errs.CodeLimitExceeded)
	}
}

// ----------------------------------------------------------------------------
// store/redeem/refund
// ----------------------------------------------------------------------------

func TestRedemptionRefundHandler_ForwardsTheExactAmountAndReference(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	originalDebit := uuid.New()
	eng := &fakeCasino{refund: func(_ context.Context, req repository.RedemptionRefundRequest) (repository.TxResult, error) {
		return repository.TxResult{
			PlayerID: req.PlayerID, TransactionType: "REDEMPTION_REFUND", Amount: req.Amount,
			PostBalances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "125.5000")},
			Status:       repository.StatusProcessed,
		}, nil
	}}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"redeem-refund:abc","player_id":"` + playerID.String() +
		`","amount":"125.5000","reference_transaction_id":"` + originalDebit.String() + `"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/redeem/refund", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	// The operator comes from the trusted HMAC context, never the body.
	if eng.lastRefund.OperatorCode != "OP1" {
		t.Errorf("operator: got %q want OP1", eng.lastRefund.OperatorCode)
	}
	// The amount is parsed precisely — scale intact, no float anywhere near it.
	if eng.lastRefund.Amount.String() != "125.5000" {
		t.Errorf("amount: got %s want 125.5000", eng.lastRefund.Amount)
	}
	if eng.lastRefund.ReferenceTransactionID != originalDebit {
		t.Errorf("reference: got %s want %s", eng.lastRefund.ReferenceTransactionID, originalDebit)
	}
}

func TestRedemptionRefundHandler_ReferenceIsOptional(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{refund: func(_ context.Context, req repository.RedemptionRefundRequest) (repository.TxResult, error) {
		return repository.TxResult{PlayerID: req.PlayerID, Status: repository.StatusProcessed,
			PostBalances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "1.0000")}}, nil
	}}
	r := casinoRouter(eng)

	// A refund whose original debit id was lost is still better than no refund.
	body := `{"operator_transaction_id":"redeem-refund:def","player_id":"` + playerID.String() + `","amount":"1.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/redeem/refund", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if eng.lastRefund.ReferenceTransactionID != uuid.Nil {
		t.Errorf("reference: got %s want the nil UUID", eng.lastRefund.ReferenceTransactionID)
	}
}

// A NEGATIVE amount is not the handler's to refuse, and this pins where that
// boundary actually is. parseAmount validates parseability and scale only —
// identically for bet, win, redeem and refund — while the SIGN is checked by the
// engine's validate(), which is the layer that can refuse it without any
// database work (see TestProcessRedemptionRefund_Validation). What the handler
// owes is that the value reaches the engine UNMANGLED rather than silently
// zeroed, because a sign quietly lost between the two layers would turn a
// refund into a second debit.
func TestRedemptionRefundHandler_ForwardsSignForTheEngineToRefuse(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{refund: func(_ context.Context, _ repository.RedemptionRefundRequest) (repository.TxResult, error) {
		return repository.TxResult{}, errs.ErrInvalidAmount
	}}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"x","player_id":"` + playerID.String() + `","amount":"-1.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/redeem/refund", body)

	if eng.lastRefund.Amount.String() != "-1.0000" {
		t.Errorf("amount reached the engine as %s, want -1.0000 — the sign must survive the hop",
			eng.lastRefund.Amount)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400 (the engine's refusal, surfaced); body=%s", w.Code, w.Body.String())
	}
}

func TestRedemptionRefundHandler_RejectsMalformedInput(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()

	cases := []struct {
		name string
		body string
	}{
		{"missing amount", `{"operator_transaction_id":"x","player_id":"` + playerID.String() + `"}`},
		{"amount as a number", `{"operator_transaction_id":"x","player_id":"` + playerID.String() + `","amount":125.5}`},
		{"bad player id", `{"operator_transaction_id":"x","player_id":"not-a-uuid","amount":"1.0000"}`},
		// A malformed reference is refused rather than silently dropped: a caller
		// that meant to link the pair should learn that it failed to.
		{"bad reference", `{"operator_transaction_id":"x","player_id":"` + playerID.String() + `","amount":"1.0000","reference_transaction_id":"nope"}`},
		{"missing transaction id", `{"player_id":"` + playerID.String() + `","amount":"1.0000"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeCasino{}
			w := doJSON(casinoRouter(eng), http.MethodPost, "/api/v1/store/redeem/refund", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
			}
			// Nothing reached the ledger.
			if eng.lastRefund.OperatorTransactionID != "" {
				t.Errorf("a malformed request reached the engine: %+v", eng.lastRefund)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// store/promo-grant — the AMOE free-entry route
// ----------------------------------------------------------------------------

func TestPromoGrantHandler_ForwardsAMOEGrantVerbatim(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{promo: func(_ context.Context, req repository.PromoGrantRequest) (repository.TxResult, error) {
		return repository.TxResult{
			OperatorCode:          req.OperatorCode,
			OperatorTransactionID: req.OperatorTransactionID,
			LedgerTransactionID:   uuid.New(),
			PlayerID:              req.PlayerID,
			TransactionType:       "PROMO_CREDIT",
			Amount:                req.SCAmount,
			Status:                repository.StatusProcessed,
		}, nil
	}}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"amoe-1","player_id":"` + playerID.String() +
		`","sc_amount":"5.0000","channel":"AMOE","channel_reference":"MAIL-2026-000123"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}

	got := eng.lastPromo
	if got.PlayerID != playerID {
		t.Errorf("player_id: got %s want %s", got.PlayerID, playerID)
	}
	if got.Channel != repository.PromoChannelAMOE {
		t.Errorf("channel: got %q want AMOE", got.Channel)
	}
	if got.ChannelReference != "MAIL-2026-000123" {
		t.Errorf("channel_reference: got %q want MAIL-2026-000123", got.ChannelReference)
	}
	if got.SCAmount.String() != "5.0000" {
		t.Errorf("sc_amount: got %s want 5.0000", got.SCAmount)
	}
	// An omitted gc_amount is a zero grant leg, not a parse failure.
	if got.GCAmount.String() != "0.0000" {
		t.Errorf("gc_amount: got %s want 0.0000 for an omitted leg", got.GCAmount)
	}
	if got.OperatorCode != "OP1" {
		t.Errorf("operator_code: got %q want the VERIFIED OP1, never a client-supplied value", got.OperatorCode)
	}
}

// The DTO has no field that could carry an SC_REDEEMABLE grant. A caller that
// tries anyway is ignored rather than obeyed — asserted here at the wire edge so
// the guarantee is proven where an attacker actually reaches it, not only in the
// allocator.
func TestPromoGrantHandler_HasNoRouteToRedeemableBalance(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"amoe-2","player_id":"` + playerID.String() +
		`","sc_amount":"1.0000","channel":"AMOE","channel_reference":"MAIL-1",` +
		`"sc_redeemable_amount":"999999.0000","sc_redeemable":"999999.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}

	// The request the engine receives carries only the two legs the type has.
	if eng.lastPromo.SCAmount.String() != "1.0000" {
		t.Errorf("sc_amount: got %s want 1.0000", eng.lastPromo.SCAmount)
	}
	if eng.lastPromo.GCAmount.String() != "0.0000" {
		t.Errorf("gc_amount: got %s want 0.0000", eng.lastPromo.GCAmount)
	}
}

// Money crosses this boundary as decimal strings. A JSON number is refused at
// the bind step — the same rule every other money route on this API enforces,
// and the one that keeps float rounding out of the ledger.
func TestPromoGrantHandler_RejectsNativeJSONNumbers(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"amoe-3","player_id":"` + playerID.String() +
		`","sc_amount":5.0,"channel":"AMOE","channel_reference":"MAIL-1"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 for a native JSON number; body=%s", w.Code, w.Body.String())
	}
	if eng.lastPromo.OperatorTransactionID != "" {
		t.Error("engine was called with a float-bearing payload")
	}
}

func TestPromoGrantHandler_RejectsMissingChannel(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"amoe-4","player_id":"` + playerID.String() + `","sc_amount":"1.0000"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", w.Code, w.Body.String())
	}
	if eng.lastPromo.OperatorTransactionID != "" {
		t.Error("engine was called for a channel-less grant")
	}
}

func TestPromoGrantHandler_RejectsMalformedAmount(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"amoe-5","player_id":"` + playerID.String() +
		`","sc_amount":"1.00005","channel":"BONUS"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 for sub-scale precision; body=%s", w.Code, w.Body.String())
	}
}

// A blocked player is refused by the ENGINE, not the handler — the guard lives
// inside the wallet lock where it is serialized against concurrent traffic. This
// asserts the handler maps that refusal to the right status rather than masking
// it as a 500.
func TestPromoGrantHandler_MapsBlockedPlayerRefusal(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeCasino{promo: func(_ context.Context, _ repository.PromoGrantRequest) (repository.TxResult, error) {
		return repository.TxResult{}, errs.ErrPlayerNotActive
	}}
	r := casinoRouter(eng)

	body := `{"operator_transaction_id":"amoe-6","player_id":"` + playerID.String() +
		`","sc_amount":"5.0000","channel":"AMOE","channel_reference":"MAIL-1"}`
	w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
	if w.Code == http.StatusOK || w.Code >= http.StatusInternalServerError {
		t.Fatalf("status: got %d, want a 4xx refusal; body=%s", w.Code, w.Body.String())
	}
}

// The channel string reaches the engine unmangled, including a lowercase one the
// engine will normalize. The handler must not silently "fix" or drop it — a
// grant whose channel was rewritten in transit is exactly the evidence problem
// the typed column exists to prevent.
func TestPromoGrantHandler_ForwardsChannelUnmangled(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()

	for _, channel := range []string{"AMOE", "BONUS", "COMPENSATION", "amoe", "NOT_A_CHANNEL"} {
		eng := &fakeCasino{}
		r := casinoRouter(eng)
		body := `{"operator_transaction_id":"amoe-7","player_id":"` + playerID.String() +
			`","sc_amount":"1.0000","channel":"` + channel + `","channel_reference":"MAIL-1"}`
		w := doJSON(r, http.MethodPost, "/api/v1/store/promo-grant", body)
		if w.Code != http.StatusOK {
			t.Fatalf("channel %q: status %d; body=%s", channel, w.Code, w.Body.String())
		}
		if string(eng.lastPromo.Channel) != channel {
			t.Errorf("channel %q arrived as %q", channel, eng.lastPromo.Channel)
		}
	}
}
