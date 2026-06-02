package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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

	lastPurchase repository.PurchaseRequest
	lastRedeem   repository.RedeemRequest
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
