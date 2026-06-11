package api

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/internal/telemetry"
	"github.com/Gavrielsh/True/pkg/errors"
)

// CasinoHandlers wires the real-money wrapper endpoints (player provisioning,
// store purchase, store redemption) to the repository.CasinoEngine. Like
// Handlers it is stateless beyond its dependency and safe for concurrent use.
type CasinoHandlers struct {
	casino repository.CasinoEngine
}

func NewCasinoHandlers(casino repository.CasinoEngine) *CasinoHandlers {
	return &CasinoHandlers{casino: casino}
}

// ----------------------------------------------------------------------------
// DTOs
// ----------------------------------------------------------------------------

// createPlayerDTO is the POST /api/v1/player/create wire format. Only
// external_id is required; the rest default server-side (username→external_id,
// country_code→"US", status→"ACTIVE").
type createPlayerDTO struct {
	ExternalID  string `json:"external_id" binding:"required"`
	Username    string `json:"username,omitempty"`
	Email       string `json:"email,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Status      string `json:"status,omitempty"`
}

// purchaseDTO is the POST /api/v1/store/purchase wire format. Amounts are JSON
// strings to preserve 4-decimal precision. sc_promo_amount is optional ("" → 0).
type purchaseDTO struct {
	OperatorTransactionID string          `json:"operator_transaction_id" binding:"required"`
	PlayerID              string          `json:"player_id"               binding:"required"`
	GCAmount              string          `json:"gc_amount"               binding:"required"`
	SCPromoAmount         string          `json:"sc_promo_amount,omitempty"`
	Metadata              json.RawMessage `json:"metadata,omitempty"`
}

// redeemDTO is the POST /api/v1/store/redeem wire format. Amount is drawn from
// SC_REDEEMABLE only.
type redeemDTO struct {
	OperatorTransactionID string          `json:"operator_transaction_id" binding:"required"`
	PlayerID              string          `json:"player_id"               binding:"required"`
	Amount                string          `json:"amount"                  binding:"required"`
	Metadata              json.RawMessage `json:"metadata,omitempty"`
}

// createPlayerResponse is the POST /api/v1/player/create 2xx payload.
type createPlayerResponse struct {
	Code     errors.Code               `json:"code"`
	PlayerID string                    `json:"player_id"`
	Created  bool                      `json:"created"`
	Balances repository.BalanceSummary `json:"balances"`
}

// ----------------------------------------------------------------------------
// Handlers
// ----------------------------------------------------------------------------

// CreatePlayer handles POST /api/v1/player/create.
func (h *CasinoHandlers) CreatePlayer(c *gin.Context) {
	var dto createPlayerDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	result, err := h.casino.CreatePlayer(c.Request.Context(), repository.CreatePlayerRequest{
		ExternalID:  dto.ExternalID,
		Username:    dto.Username,
		Email:       dto.Email,
		CountryCode: dto.CountryCode,
		Status:      dto.Status,
	})
	if err != nil {
		respondError(c, err)
		return
	}

	// A freshly-created player is 201; an idempotent replay (already existed)
	// is 200. Both carry the same body so the operator can treat them alike.
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	c.JSON(status, createPlayerResponse{
		Code:     errors.CodeOK,
		PlayerID: result.PlayerID.String(),
		Created:  result.Created,
		Balances: result.Balances,
	})
}

// Purchase handles POST /api/v1/store/purchase.
func (h *CasinoHandlers) Purchase(c *gin.Context) {
	var dto purchaseDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	gcAmount, ok := parseAmount(c, dto.GCAmount)
	if !ok {
		return
	}
	scPromo, ok := parseOptionalAmount(c, dto.SCPromoAmount)
	if !ok {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := moneySpan(c, "http.purchase", operatorCode, dto.OperatorTransactionID, playerID)

	result, err := h.casino.ProcessPurchase(ctx, repository.PurchaseRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		GCAmount:              gcAmount,
		SCPromoAmount:         scPromo,
		Metadata:              dto.Metadata,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// Redeem handles POST /api/v1/store/redeem.
func (h *CasinoHandlers) Redeem(c *gin.Context) {
	var dto redeemDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	amount, ok := parseAmount(c, dto.Amount)
	if !ok {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := moneySpan(c, "http.redeem", operatorCode, dto.OperatorTransactionID, playerID)

	result, err := h.casino.ProcessRedeem(ctx, repository.RedeemRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Amount:                amount,
		Metadata:              dto.Metadata,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// parseOptionalAmount parses a money string that may be empty. An empty/omitted
// value is treated as 0.0000 (used for the optional sc_promo_amount). A present
// but malformed value is still rejected.
func parseOptionalAmount(c *gin.Context, raw string) (domain.Money, bool) {
	if raw == "" {
		return domain.ZeroMoney(), true
	}
	return parseAmount(c, raw)
}
