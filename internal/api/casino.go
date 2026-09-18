package api

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

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

// redemptionRefundDTO is the POST /api/v1/store/redeem/refund wire format.
//
// `amount` must be EXACTLY what the redemption debited. The engine credits what
// it is told and cannot look the original up, so a caller sending a different
// figure is the one failure this design cannot catch — which is why the
// gateway forwards its stored decimal string verbatim rather than recomputing
// it.
type redemptionRefundDTO struct {
	OperatorTransactionID  string          `json:"operator_transaction_id" binding:"required"`
	PlayerID               string          `json:"player_id"               binding:"required"`
	Amount                 string          `json:"amount"                  binding:"required"`
	ReferenceTransactionID string          `json:"reference_transaction_id,omitempty"`
	Metadata               json.RawMessage `json:"metadata,omitempty"`
}

// promoGrantDTO is the POST /api/v1/store/promo-grant wire format — the AMOE
// free-entry route and its siblings.
//
// gc_amount and sc_amount are decimal STRINGS like every other money field on
// this API, and both are optional individually ("" → 0) provided at least one is
// positive. There is no sc_redeemable field and there cannot be one: a
// no-purchase path that could mint cashable tokens would be a withdrawal channel
// with neither payment nor gameplay behind it. sc_amount is credited to
// SC_UNPLAYED under the same 1x wagering requirement a purchaser's promotional
// SC carries.
//
// channel_reference is REQUIRED when channel is AMOE — it is the operator's
// identifier for the mail-in entry this grant answers, and an AMOE record that
// cannot be tied back to a received entry is an assertion rather than evidence.
type promoGrantDTO struct {
	OperatorTransactionID string          `json:"operator_transaction_id" binding:"required"`
	PlayerID              string          `json:"player_id"               binding:"required"`
	GCAmount              string          `json:"gc_amount,omitempty"`
	SCAmount              string          `json:"sc_amount,omitempty"`
	Channel               string          `json:"channel"                 binding:"required"`
	ChannelReference      string          `json:"channel_reference,omitempty"`
	Metadata              json.RawMessage `json:"metadata,omitempty"`
}

// statusTransitionDTO is the POST /api/v1/player/status wire format.
//
// self_exclusion_days is a whole-day COUNT, required when to_status is
// SELF_EXCLUDED and rejected otherwise. Deliberately not an end timestamp: an
// instant computed by the caller is already stale when it arrives, so a request
// for exactly the 180-day minimum would be refused for having spent a few
// milliseconds in transport. A day count is measured by the engine's own clock
// at the instant the transition commits — see the note in
// repository/status.go.
type statusTransitionDTO struct {
	PlayerID          string `json:"player_id"                    binding:"required"`
	ToStatus          string `json:"to_status"                    binding:"required"`
	ActorType         string `json:"actor_type"                   binding:"required"`
	ActorRef          string `json:"actor_ref,omitempty"`
	Reason            string `json:"reason"                       binding:"required"`
	SelfExclusionDays int    `json:"self_exclusion_days,omitempty"`
}

// setPlayerLimitDTO is the POST /api/v1/player/limits wire format.
//
// amount is a decimal STRING like every other money field on this API. A limit
// is compared against a stake, so it has to be the same kind of number as the
// stake — a JSON number here would reintroduce float rounding on exactly the
// value that decides whether a player may wager.
type setPlayerLimitDTO struct {
	PlayerID  string `json:"player_id"  binding:"required"`
	Kind      string `json:"limit_kind" binding:"required"`
	Period    string `json:"period"     binding:"required"`
	Amount    string `json:"amount"     binding:"required"`
	ActorType string `json:"actor_type" binding:"required"`
	ActorRef  string `json:"actor_ref,omitempty"`
}

// setPlayerLimitResponse is the POST /api/v1/player/limits 2xx payload.
type setPlayerLimitResponse struct {
	Code   errors.Code                     `json:"code"`
	Result repository.SetPlayerLimitResult `json:"result"`
}

// statusTransitionResponse is the POST /api/v1/player/status 2xx payload.
//
// A dedicated type rather than the shared successResponse, which embeds
// repository.TxResult: a status change produces no ledger transaction, and
// widening that envelope to carry either shape would blur the distinction
// between "money moved" and "a compliance flag moved" in every response on the
// API. createPlayerResponse already sets this precedent for the same reason.
type statusTransitionResponse struct {
	Code   errors.Code                       `json:"code"`
	Result repository.StatusTransitionResult `json:"result"`
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
		BodyHash:              BodyHashFromContext(c.Request.Context()),
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
		BodyHash:              BodyHashFromContext(c.Request.Context()),
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// PromoGrant handles POST /api/v1/store/promo-grant — issuing coins with no
// purchase behind them.
//
// Behind the same HMAC + replay perimeter as every other money route, and for
// the sharpest version of the usual reason: this is a pure CREDIT bounded by
// nothing the player owns. A redemption an attacker forged is capped by the
// balance available to take; a grant an attacker forged is capped by nothing at
// all. An unsigned caller reaching this route could mint the casino's own
// currency without limit.
//
// Inside the geo-fence, unlike /player/status and /player/limits. Those are
// protective acts that must never be geo-denied; this one hands a player new
// entries, which is exactly what a blocked jurisdiction must not receive —
// offering a free entry into a sweepstakes where sweepstakes are prohibited is
// the offence, not a workaround for it.
func (h *CasinoHandlers) PromoGrant(c *gin.Context) {
	var dto promoGrantDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	// Both optional individually; the engine refuses a grant of nothing. Parsed
	// with parseOptionalAmount so an omitted leg is 0 rather than a 400 — a
	// GC-only bonus and an SC-only AMOE entry are both ordinary shapes here.
	gcAmount, ok := parseOptionalAmount(c, dto.GCAmount)
	if !ok {
		return
	}
	scAmount, ok := parseOptionalAmount(c, dto.SCAmount)
	if !ok {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := moneySpan(c, "http.promo_grant", operatorCode, dto.OperatorTransactionID, playerID)

	result, err := h.casino.ProcessPromoGrant(ctx, repository.PromoGrantRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		GCAmount:              gcAmount,
		SCAmount:              scAmount,
		Channel:               repository.PromoChannel(dto.Channel),
		ChannelReference:      dto.ChannelReference,
		Metadata:              dto.Metadata,
		BodyHash:              BodyHashFromContext(c.Request.Context()),
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// UpdateStatus handles POST /api/v1/player/status — the only route that can
// change a player's status.
//
// Mounted OUTSIDE the jurisdiction fence (see router.go): a player must always
// be able to exclude themselves, and an operator must always be able to close an
// account, regardless of where the request originates.
// RedemptionRefund handles POST /api/v1/store/redeem/refund — returning
// SC_REDEEMABLE a redemption took but never paid out.
//
// Behind the same HMAC + replay perimeter as every other money route: it is a
// CREDIT, so an unauthenticated caller reaching it could mint balance. Nothing
// about "it is only a refund" makes it less sensitive than the debit it
// reverses — if anything more, since a debit is bounded by the player's balance
// and a credit is bounded by nothing.
func (h *CasinoHandlers) RedemptionRefund(c *gin.Context) {
	var dto redemptionRefundDTO
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

	// Optional: a malformed reference is refused rather than silently dropped —
	// a caller that meant to link the pair should learn it failed to.
	var reference uuid.UUID
	if dto.ReferenceTransactionID != "" {
		parsed, err := uuid.Parse(dto.ReferenceTransactionID)
		if err != nil {
			respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid reference_transaction_id")
			return
		}
		reference = parsed
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := moneySpan(c, "http.redemption_refund", operatorCode, dto.OperatorTransactionID, playerID)

	result, err := h.casino.ProcessRedemptionRefund(ctx, repository.RedemptionRefundRequest{
		OperatorCode:           operatorCode,
		OperatorTransactionID:  dto.OperatorTransactionID,
		PlayerID:               playerID,
		Amount:                 amount,
		ReferenceTransactionID: reference,
		Metadata:               dto.Metadata,
		BodyHash:               BodyHashFromContext(c.Request.Context()),
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

func (h *CasinoHandlers) UpdateStatus(c *gin.Context) {
	var dto statusTransitionDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := telemetry.StartSpan(c.Request.Context(), "http.player_status",
		attribute.String("operator_code", operatorCode),
		attribute.String("player_id", playerID.String()),
		attribute.String("to_status", dto.ToStatus))

	result, err := h.casino.ProcessStatusTransition(ctx, repository.StatusTransitionRequest{
		OperatorCode:      operatorCode,
		PlayerID:          playerID,
		ToStatus:          dto.ToStatus,
		ActorType:         dto.ActorType,
		ActorRef:          dto.ActorRef,
		Reason:            dto.Reason,
		SelfExclusionDays: dto.SelfExclusionDays,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, statusTransitionResponse{Code: errors.CodeOK, Result: result})
}

// SetPlayerLimit handles POST /api/v1/player/limits.
//
// Mounted on the same unfenced compliance group as /player/status: setting or
// lowering a limit is a protective act, and must not be refused on
// jurisdiction. Raising one is not protective, but splitting the route by
// direction would mean the client has to know the current value to know which
// endpoint to call — and a player who cannot reach the endpoint at all cannot
// lower their limit either.
func (h *CasinoHandlers) SetPlayerLimit(c *gin.Context) {
	var dto setPlayerLimitDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	// parseAmount is the same decimal-string parser every money field uses, so a
	// limit and the stake it bounds are parsed by identical rules.
	amount, ok := parseAmount(c, dto.Amount)
	if !ok {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	ctx, span := telemetry.StartSpan(c.Request.Context(), "http.player_limit",
		attribute.String("operator_code", operatorCode),
		attribute.String("player_id", playerID.String()),
		attribute.String("limit_kind", dto.Kind),
		attribute.String("period", dto.Period))

	result, err := h.casino.ProcessSetPlayerLimit(ctx, repository.SetPlayerLimitRequest{
		OperatorCode: operatorCode,
		PlayerID:     playerID,
		Kind:         dto.Kind,
		Period:       dto.Period,
		Amount:       amount,
		ActorType:    dto.ActorType,
		ActorRef:     dto.ActorRef,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, setPlayerLimitResponse{Code: errors.CodeOK, Result: result})
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
