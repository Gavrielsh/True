package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/internal/telemetry"
	"github.com/Gavrielsh/True/pkg/errors"
)

// KYCHandlers wires the Zone 1 KYC ingestion endpoint to repository.KYCEngine.
type KYCHandlers struct {
	kyc repository.KYCEngine
}

func NewKYCHandlers(kyc repository.KYCEngine) *KYCHandlers {
	return &KYCHandlers{kyc: kyc}
}

// kycDecisionDTO is the POST /api/v1/kyc/decision wire format. decided_at
// is RFC3339 and MUST carry the provider's decision instant, not the
// receipt time — see repository.KYCDecisionRequest.DecidedAt.
type kycDecisionDTO struct {
	ExternalID string `json:"external_id" binding:"required"`
	EventID    string `json:"event_id" binding:"required"`
	Decision   string `json:"decision" binding:"required"`
	DecidedAt  string `json:"decided_at" binding:"required"`
	Reason     string `json:"reason,omitempty"`
}

// kycDecisionResponse is the POST /api/v1/kyc/decision payload.
type kycDecisionResponse struct {
	Code   errors.Code                  `json:"code"`
	Result repository.KYCDecisionResult `json:"result"`
}

// Decision handles POST /api/v1/kyc/decision — the Zone 1 ingestion route
// for identity-verification results relayed by the Gateway (Zone 2). It
// runs behind the same HMAC + replay-guard stack as every other /api/v1
// route (see router.go); GeoFence is deliberately NOT applied, since this
// is Gateway-to-engine service traffic, not player-geography-relevant.
func (h *KYCHandlers) Decision(c *gin.Context) {
	var dto kycDecisionDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	decidedAt, err := time.Parse(time.RFC3339, dto.DecidedAt)
	if err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "decided_at must be RFC3339")
		return
	}

	ctx, span := telemetry.StartSpan(c.Request.Context(), "http.kyc_decision",
		attribute.String("event_id", dto.EventID))
	result, err := h.kyc.RecordDecision(ctx, repository.KYCDecisionRequest{
		ExternalID: dto.ExternalID,
		EventID:    dto.EventID,
		Decision:   dto.Decision,
		DecidedAt:  decidedAt,
		Reason:     dto.Reason,
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, kycDecisionResponse{Code: errors.CodeOK, Result: result})
}
