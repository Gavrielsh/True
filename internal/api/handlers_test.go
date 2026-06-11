package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/repository"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// ----------------------------------------------------------------------------
// fakeEngine implements repository.Engine with injectable behaviour.
// ----------------------------------------------------------------------------

type fakeEngine struct {
	bet      func(context.Context, repository.BetRequest) (repository.TxResult, error)
	win      func(context.Context, repository.WinRequest) (repository.TxResult, error)
	rollback func(context.Context, repository.RollbackRequest) (repository.TxResult, error)
	balances func(context.Context, uuid.UUID) (domain.Wallet, error)

	lastBet repository.BetRequest // captured for assertions
}

func (f *fakeEngine) ProcessBet(ctx context.Context, req repository.BetRequest) (repository.TxResult, error) {
	f.lastBet = req
	return f.bet(ctx, req)
}
func (f *fakeEngine) ProcessWin(ctx context.Context, req repository.WinRequest) (repository.TxResult, error) {
	return f.win(ctx, req)
}
func (f *fakeEngine) ProcessRollback(ctx context.Context, req repository.RollbackRequest) (repository.TxResult, error) {
	return f.rollback(ctx, req)
}
func (f *fakeEngine) GetBalances(ctx context.Context, id uuid.UUID) (domain.Wallet, error) {
	return f.balances(ctx, id)
}

func mustMoney(t *testing.T, s string) domain.Money {
	t.Helper()
	m, err := domain.MoneyFromString(s)
	if err != nil {
		t.Fatalf("money %q: %v", s, err)
	}
	return m
}

// handlerRouter mounts the handlers behind a stub middleware that injects a
// verified operator code (simulating a successful HMAC pass) so we can test
// controller logic in isolation from the security stack.
func handlerRouter(eng repository.Engine) *gin.Engine {
	r := gin.New()
	h := NewHandlers(eng)
	inject := func(c *gin.Context) {
		c.Set(ginKeyOperator, "OP1")
		c.Request = c.Request.WithContext(withOperator(c.Request.Context(), "OP1"))
		c.Next()
	}
	g := r.Group("/api/v1", inject)
	g.POST("/bet", h.Bet)
	g.POST("/win", h.Win)
	g.POST("/rollback", h.Rollback)
	g.POST("/session", h.Session)
	return r
}

func doJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var e errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return e
}

// ----------------------------------------------------------------------------
// Bet handler
// ----------------------------------------------------------------------------

func TestBetHandler_HappyPath(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	ledgerID := uuid.New()
	eng := &fakeEngine{
		bet: func(_ context.Context, req repository.BetRequest) (repository.TxResult, error) {
			return repository.TxResult{
				OperatorCode:          req.OperatorCode,
				OperatorTransactionID: req.OperatorTransactionID,
				LedgerTransactionID:   ledgerID,
				PlayerID:              req.PlayerID,
				TransactionType:       "BET",
				Family:                "SC",
				Amount:                req.Amount,
				PostBalances: repository.BalanceSummary{
					GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "90.0000"),
				},
				Status: repository.StatusProcessed,
			}, nil
		},
	}
	r := handlerRouter(eng)

	body := `{"operator_transaction_id":"op-1","player_id":"` + playerID.String() + `","currency":"SC","amount":"10.0000","game_id":"G1"}`
	w := doJSON(r, http.MethodPost, "/api/v1/bet", body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp successResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != errs.CodeOK {
		t.Errorf("code: got %s want OK", resp.Code)
	}
	if resp.Result.PostBalances.SCRedeemable.String() != "90.0000" {
		t.Errorf("balance: got %s", resp.Result.PostBalances.SCRedeemable)
	}

	// The operator code must come from the (trusted) context, NOT the body.
	if eng.lastBet.OperatorCode != "OP1" {
		t.Errorf("operator code: got %q want OP1 (must be from HMAC context)", eng.lastBet.OperatorCode)
	}
	if eng.lastBet.Family != domain.FamilySC {
		t.Errorf("family: got %v want SC", eng.lastBet.Family)
	}
}

func TestBetHandler_DomainErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		engineErr  error
		wantStatus int
		wantCode   errs.Code
	}{
		{"insufficient", errs.ErrInsufficientFunds, http.StatusBadRequest, errs.CodeInsufficientFunds},
		{"player_missing", errs.ErrPlayerNotFound, http.StatusNotFound, errs.CodePlayerNotFound},
		{"pending", errs.ErrTransactionPending, http.StatusConflict, errs.CodeTransactionPending},
		{"conflict", errs.ErrTransactionConflict, http.StatusConflict, errs.CodeTransactionConflict},
		{"internal", context.DeadlineExceeded, http.StatusInternalServerError, errs.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeEngine{
				bet: func(_ context.Context, _ repository.BetRequest) (repository.TxResult, error) {
					return repository.TxResult{}, tc.engineErr
				},
			}
			r := handlerRouter(eng)
			playerID := uuid.New()
			body := `{"operator_transaction_id":"op","player_id":"` + playerID.String() + `","currency":"GC","amount":"10.0000"}`
			w := doJSON(r, http.MethodPost, "/api/v1/bet", body)

			if w.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d", w.Code, tc.wantStatus)
			}
			if got := decodeErr(t, w).Code; got != tc.wantCode {
				t.Errorf("code: got %s want %s", got, tc.wantCode)
			}
		})
	}
}

func TestBetHandler_InputValidation(t *testing.T) {
	t.Parallel()
	validID := uuid.New().String()
	cases := []struct {
		name string
		body string
		code errs.Code
	}{
		{"bad_json", `{not json`, errs.CodeInvalidAmount},
		{"missing_required", `{"player_id":"` + validID + `"}`, errs.CodeInvalidAmount},
		{"bad_player_id", `{"operator_transaction_id":"op","player_id":"not-a-uuid","currency":"GC","amount":"1.0000"}`, errs.CodeInvalidAmount},
		{"bad_currency", `{"operator_transaction_id":"op","player_id":"` + validID + `","currency":"USD","amount":"1.0000"}`, errs.CodeUnsupportedCurrency},
		{"sub_currency_leak", `{"operator_transaction_id":"op","player_id":"` + validID + `","currency":"SC_UNPLAYED","amount":"1.0000"}`, errs.CodeUnsupportedCurrency},
		{"bad_amount", `{"operator_transaction_id":"op","player_id":"` + validID + `","currency":"GC","amount":"abc"}`, errs.CodeInvalidAmount},
		{"too_precise", `{"operator_transaction_id":"op","player_id":"` + validID + `","currency":"GC","amount":"1.00001"}`, errs.CodeInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeEngine{
				bet: func(_ context.Context, _ repository.BetRequest) (repository.TxResult, error) {
					t.Error("engine must NOT be called on invalid input")
					return repository.TxResult{}, nil
				},
			}
			r := handlerRouter(eng)
			w := doJSON(r, http.MethodPost, "/api/v1/bet", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
			}
			if got := decodeErr(t, w).Code; got != tc.code {
				t.Errorf("code: got %s want %s", got, tc.code)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Win handler
// ----------------------------------------------------------------------------

func TestWinHandler_HappyPath_WithReference(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	refID := uuid.New()
	var captured repository.WinRequest
	eng := &fakeEngine{
		win: func(_ context.Context, req repository.WinRequest) (repository.TxResult, error) {
			captured = req
			return repository.TxResult{
				PlayerID: req.PlayerID, TransactionType: "WIN", Amount: req.Amount,
				PostBalances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "15.0000")},
				Status:       repository.StatusProcessed,
			}, nil
		},
	}
	r := handlerRouter(eng)
	body := `{"operator_transaction_id":"win-1","player_id":"` + playerID.String() + `","currency":"SC","amount":"5.0000","reference_transaction_id":"` + refID.String() + `"}`
	w := doJSON(r, http.MethodPost, "/api/v1/win", body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if captured.ReferenceTransactionID != refID {
		t.Errorf("reference: got %v want %v", captured.ReferenceTransactionID, refID)
	}
}

func TestWinHandler_BadReference(t *testing.T) {
	t.Parallel()
	eng := &fakeEngine{win: func(_ context.Context, _ repository.WinRequest) (repository.TxResult, error) {
		t.Error("engine must not be called")
		return repository.TxResult{}, nil
	}}
	r := handlerRouter(eng)
	playerID := uuid.New()
	body := `{"operator_transaction_id":"win","player_id":"` + playerID.String() + `","currency":"SC","amount":"5.0000","reference_transaction_id":"nope"}`
	w := doJSON(r, http.MethodPost, "/api/v1/win", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

// ----------------------------------------------------------------------------
// Rollback handler
// ----------------------------------------------------------------------------

func TestRollbackHandler(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	refID := uuid.New()
	cases := []struct {
		name       string
		engineErr  error
		wantStatus int
		wantCode   errs.Code
	}{
		{"ok", nil, http.StatusOK, errs.CodeOK},
		{"not_found", errs.ErrRollbackNotFound, http.StatusNotFound, errs.CodeRollbackNotFound},
		{"already", errs.ErrRollbackAlready, http.StatusConflict, errs.CodeRollbackAlready},
		{"unsupported", errs.ErrRollbackUnsupported, http.StatusUnprocessableEntity, errs.CodeRollbackUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeEngine{
				rollback: func(_ context.Context, req repository.RollbackRequest) (repository.TxResult, error) {
					if tc.engineErr != nil {
						return repository.TxResult{}, tc.engineErr
					}
					return repository.TxResult{PlayerID: req.PlayerID, TransactionType: "ROLLBACK", Status: repository.StatusProcessed,
						PostBalances: repository.BalanceSummary{GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000")}}, nil
				},
			}
			r := handlerRouter(eng)
			body := `{"operator_transaction_id":"rb","player_id":"` + playerID.String() + `","reference_transaction_id":"` + refID.String() + `"}`
			w := doJSON(r, http.MethodPost, "/api/v1/rollback", body)
			if w.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Session handler
// ----------------------------------------------------------------------------

func TestSessionHandler(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeEngine{
		balances: func(_ context.Context, id uuid.UUID) (domain.Wallet, error) {
			return domain.Wallet{
				PlayerID: id.String(),
				GC:       mustMoney(t, "100.0000"), SCUnplayed: mustMoney(t, "5.0000"), SCRedeemable: mustMoney(t, "10.0000"),
			}, nil
		},
	}
	r := handlerRouter(eng)
	w := doJSON(r, http.MethodPost, "/api/v1/session", `{"player_id":"`+playerID.String()+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp balancesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Balances.GC.String() != "100.0000" {
		t.Errorf("GC: got %s want 100.0000", resp.Balances.GC)
	}
}

// Every DB-bound handler must hand the engine a context carrying a STRICT
// deadline (context.WithTimeout), so a dropped client can never leave a
// SELECT FOR UPDATE lock (or a pooled connection) held indefinitely.
func TestHandlers_ApplyContextDeadline(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	ok := repository.TxResult{
		PlayerID: playerID, Status: repository.StatusProcessed,
		PostBalances: repository.BalanceSummary{
			GC: mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000"),
		},
	}

	var betDL, winDL, rbDL, sessDL bool
	eng := &fakeEngine{
		bet: func(ctx context.Context, _ repository.BetRequest) (repository.TxResult, error) {
			_, betDL = ctx.Deadline()
			return ok, nil
		},
		win: func(ctx context.Context, _ repository.WinRequest) (repository.TxResult, error) {
			_, winDL = ctx.Deadline()
			return ok, nil
		},
		rollback: func(ctx context.Context, _ repository.RollbackRequest) (repository.TxResult, error) {
			_, rbDL = ctx.Deadline()
			return ok, nil
		},
		balances: func(ctx context.Context, _ uuid.UUID) (domain.Wallet, error) {
			_, sessDL = ctx.Deadline()
			return domain.Wallet{
				PlayerID: playerID.String(),
				GC:       mustMoney(t, "0.0000"), SCUnplayed: mustMoney(t, "0.0000"), SCRedeemable: mustMoney(t, "0.0000"),
			}, nil
		},
	}
	r := handlerRouter(eng)
	pid := playerID.String()
	ref := uuid.New().String()

	doJSON(r, http.MethodPost, "/api/v1/bet", `{"operator_transaction_id":"op","player_id":"`+pid+`","currency":"GC","amount":"1.0000"}`)
	doJSON(r, http.MethodPost, "/api/v1/win", `{"operator_transaction_id":"op","player_id":"`+pid+`","currency":"GC","amount":"1.0000"}`)
	doJSON(r, http.MethodPost, "/api/v1/rollback", `{"operator_transaction_id":"op","player_id":"`+pid+`","reference_transaction_id":"`+ref+`"}`)
	doJSON(r, http.MethodPost, "/api/v1/session", `{"player_id":"`+pid+`"}`)

	if !betDL {
		t.Error("Bet: engine context must carry a deadline")
	}
	if !winDL {
		t.Error("Win: engine context must carry a deadline")
	}
	if !rbDL {
		t.Error("Rollback: engine context must carry a deadline")
	}
	if !sessDL {
		t.Error("Session: engine context must carry a deadline")
	}
}

func TestSessionHandler_MissingPlayerID(t *testing.T) {
	t.Parallel()
	eng := &fakeEngine{balances: func(_ context.Context, _ uuid.UUID) (domain.Wallet, error) {
		t.Error("engine must not be called")
		return domain.Wallet{}, nil
	}}
	r := handlerRouter(eng)
	w := doJSON(r, http.MethodPost, "/api/v1/session", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

func TestSessionHandler_NotFound(t *testing.T) {
	t.Parallel()
	eng := &fakeEngine{balances: func(_ context.Context, _ uuid.UUID) (domain.Wallet, error) {
		return domain.Wallet{}, errs.ErrPlayerNotFound
	}}
	r := handlerRouter(eng)
	w := doJSON(r, http.MethodPost, "/api/v1/session", `{"player_id":"`+uuid.NewString()+`"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", w.Code)
	}
}

// ----------------------------------------------------------------------------
// Error-code → HTTP-status mapping (exhaustive table)
// ----------------------------------------------------------------------------

func TestHTTPStatusFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code   errs.Code
		status int
	}{
		{errs.CodeOK, http.StatusOK},
		{errs.CodeInsufficientFunds, http.StatusBadRequest},
		{errs.CodeInvalidAmount, http.StatusBadRequest},
		{errs.CodeUnsupportedCurrency, http.StatusBadRequest},
		{errs.CodePlayerNotFound, http.StatusNotFound},
		{errs.CodePlayerNotActive, http.StatusForbidden},
		{errs.CodeTransactionPending, http.StatusConflict},
		{errs.CodeTransactionConflict, http.StatusConflict},
		{errs.CodeRollbackNotFound, http.StatusNotFound},
		{errs.CodeRollbackAlready, http.StatusConflict},
		{errs.CodeRollbackUnsupported, http.StatusUnprocessableEntity},
		{errs.CodeInternal, http.StatusInternalServerError},
		{errs.Code("WHO_KNOWS"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			t.Parallel()
			if got := httpStatusFor(tc.code); got != tc.status {
				t.Errorf("httpStatusFor(%s) = %d, want %d", tc.code, got, tc.status)
			}
		})
	}
}
