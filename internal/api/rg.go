package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/internal/telemetry"
	"github.com/Gavrielsh/True/pkg/errors"
)

// RGHandlers wires the responsible-gaming endpoints (PLAN.md item 6) to
// repository.ResponsibleGamingEngine. Like CasinoHandlers/KYCHandlers it is
// stateless beyond its dependency and safe for concurrent use.
type RGHandlers struct {
	rg repository.ResponsibleGamingEngine
}

func NewRGHandlers(rg repository.ResponsibleGamingEngine) *RGHandlers {
	return &RGHandlers{rg: rg}
}

// ----------------------------------------------------------------------------
// DTOs
// ----------------------------------------------------------------------------

// setLimitDTO is the POST /api/v1/player/limits wire format. amount and
// ends_at are only required for certain limit_type/active combinations —
// see repository.SetLimitRequest.validate, which is the single source of
// truth for that shape.
type setLimitDTO struct {
	PlayerID    string `json:"player_id"  binding:"required"`
	LimitType   string `json:"limit_type" binding:"required"`
	Active      bool   `json:"active"`
	Period      string `json:"period,omitempty"`
	Amount      string `json:"amount,omitempty"`
	EndsAt      string `json:"ends_at,omitempty"` // RFC3339
	EventID     string `json:"event_id"   binding:"required"`
	RequestedBy string `json:"requested_by,omitempty"`
}

// queryLimitsDTO is the POST /api/v1/player/limits/query wire format. POST
// (not GET) for the same reason as /session: player_id must live in the
// HMAC-signed body.
type queryLimitsDTO struct {
	PlayerID string `json:"player_id" binding:"required"`
}

// checkPurchaseDTO is the POST /api/v1/player/limits/check-purchase wire
// format — the pre-charge check the gateway must call BEFORE charging the
// card (point 1). usd_amount is required here, unlike purchaseDTO's
// optional usd_amount backstop.
type checkPurchaseDTO struct {
	PlayerID  string `json:"player_id"  binding:"required"`
	USDAmount string `json:"usd_amount" binding:"required"`
}

type setLimitResponse struct {
	Code   errors.Code            `json:"code"`
	Result repository.LimitResult `json:"result"`
}

type queryLimitsResponse struct {
	Code   errors.Code                    `json:"code"`
	Result repository.PlayerLimitsSummary `json:"result"`
}

type checkPurchaseResponse struct {
	Code   errors.Code                    `json:"code"`
	Result repository.CheckPurchaseResult `json:"result"`
}

// ----------------------------------------------------------------------------
// Handlers
// ----------------------------------------------------------------------------

// SetLimit handles POST /api/v1/player/limits — sets or lifts one
// SELF_EXCLUSION / COOL_OFF / LOSS_LIMIT / DEPOSIT_LIMIT for a player.
func (h *RGHandlers) SetLimit(c *gin.Context) {
	var dto setLimitDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}

	var amount *domain.Money
	if dto.Amount != "" {
		a, ok := parseAmount(c, dto.Amount)
		if !ok {
			return
		}
		amount = &a
	}

	var endsAt *time.Time
	if dto.EndsAt != "" {
		t, err := time.Parse(time.RFC3339, dto.EndsAt)
		if err != nil {
			respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "ends_at must be RFC3339")
			return
		}
		endsAt = &t
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := telemetry.StartSpan(c.Request.Context(), "http.rg_set_limit",
		attribute.String("player_id", playerID.String()),
		attribute.String("operator_code", operatorCode),
		attribute.String("limit_type", dto.LimitType))

	result, err := h.rg.SetLimit(ctx, repository.SetLimitRequest{
		PlayerID:    playerID,
		LimitType:   dto.LimitType,
		Active:      dto.Active,
		Period:      dto.Period,
		Amount:      amount,
		EndsAt:      endsAt,
		EventID:     dto.EventID,
		RequestedBy: dto.RequestedBy,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, setLimitResponse{Code: errors.CodeOK, Result: result})
}

// QueryLimits handles POST /api/v1/player/limits/query — the player's
// current effective limits and, for LOSS_LIMIT/DEPOSIT_LIMIT, the remaining
// allowance in the current window.
func (h *RGHandlers) QueryLimits(c *gin.Context) {
	var dto queryLimitsDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}

	ctx, span := telemetry.StartSpan(c.Request.Context(), "http.rg_query_limits",
		attribute.String("player_id", playerID.String()))
	result, err := h.rg.QueryLimits(ctx, playerID)
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, queryLimitsResponse{Code: errors.CodeOK, Result: result})
}

// CheckPurchase handles POST /api/v1/player/limits/check-purchase — the
// PRE-CHARGE check (point 1). The gateway MUST call this before charging
// the card, and refund if /store/purchase's own backstop guard ever
// refuses instead (see repository.PurchaseRequest.USDAmount).
func (h *RGHandlers) CheckPurchase(c *gin.Context) {
	var dto checkPurchaseDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	usdAmount, ok := parseAmount(c, dto.USDAmount)
	if !ok {
		return
	}

	ctx, span := telemetry.StartSpan(c.Request.Context(), "http.rg_check_purchase",
		attribute.String("player_id", playerID.String()))
	result, err := h.rg.CheckPurchase(ctx, repository.CheckPurchaseRequest{
		PlayerID:  playerID,
		USDAmount: usdAmount,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, checkPurchaseResponse{Code: errors.CodeOK, Result: result})
}
