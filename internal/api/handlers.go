package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/pkg/errors"
)

// Handlers wires the HTTP endpoints to the repository.Engine. It holds no
// state beyond the engine dependency — safe for concurrent use.
type Handlers struct {
	engine repository.Engine
}

func NewHandlers(engine repository.Engine) *Handlers {
	return &Handlers{engine: engine}
}

// Bet handles POST /api/v1/bet.
func (h *Handlers) Bet(c *gin.Context) {
	var dto betRequestDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	family, ok := parseFamily(c, dto.Currency)
	if !ok {
		return
	}
	amount, ok := parseAmount(c, dto.Amount)
	if !ok {
		return
	}

	result, err := h.engine.ProcessBet(c.Request.Context(), repository.BetRequest{
		OperatorCode:          OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Family:                family,
		Amount:                amount,
		GameID:                dto.GameID,
		RoundID:               dto.RoundID,
		Metadata:              dto.Metadata,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// Win handles POST /api/v1/win.
func (h *Handlers) Win(c *gin.Context) {
	var dto winRequestDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	family, ok := parseFamily(c, dto.Currency)
	if !ok {
		return
	}
	amount, ok := parseAmount(c, dto.Amount)
	if !ok {
		return
	}

	// reference_transaction_id is optional; empty → uuid.Nil (NULL in DB).
	var reference uuid.UUID
	if dto.ReferenceTransactionID != "" {
		ref, err := uuid.Parse(dto.ReferenceTransactionID)
		if err != nil {
			respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid reference_transaction_id")
			return
		}
		reference = ref
	}

	result, err := h.engine.ProcessWin(c.Request.Context(), repository.WinRequest{
		OperatorCode:           OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID:  dto.OperatorTransactionID,
		PlayerID:               playerID,
		Family:                 family,
		Amount:                 amount,
		GameID:                 dto.GameID,
		RoundID:                dto.RoundID,
		ReferenceTransactionID: reference,
		Metadata:               dto.Metadata,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// Rollback handles POST /api/v1/rollback.
func (h *Handlers) Rollback(c *gin.Context) {
	var dto rollbackRequestDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	reference, err := uuid.Parse(dto.ReferenceTransactionID)
	if err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid reference_transaction_id")
		return
	}

	result, err := h.engine.ProcessRollback(c.Request.Context(), repository.RollbackRequest{
		OperatorCode:           OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID:  dto.OperatorTransactionID,
		PlayerID:               playerID,
		ReferenceTransactionID: reference,
		Metadata:               dto.Metadata,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// Session handles GET /api/v1/session?player_id=<uuid>.
func (h *Handlers) Session(c *gin.Context) {
	playerID, ok := parsePlayerID(c, c.Query("player_id"))
	if !ok {
		return
	}

	wallet, err := h.engine.GetBalances(c.Request.Context(), playerID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, balancesResponse{
		Code:     errors.CodeOK,
		PlayerID: playerID.String(),
		Balances: repository.BalanceSummary{
			GC:           wallet.GC,
			SCUnplayed:   wallet.SCUnplayed,
			SCRedeemable: wallet.SCRedeemable,
		},
	})
}

// ----------------------------------------------------------------------------
// Shared field parsers. Each writes the error response itself and returns
// ok=false so the caller just `return`s — keeps the handlers flat.
// ----------------------------------------------------------------------------

func parsePlayerID(c *gin.Context, raw string) (uuid.UUID, bool) {
	if raw == "" {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "player_id is required")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid player_id")
		return uuid.Nil, false
	}
	return id, true
}

func parseFamily(c *gin.Context, raw string) (domain.CurrencyFamily, bool) {
	family := domain.FamilyFromString(raw)
	if !family.Valid() {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeUnsupportedCurrency, "currency must be GC or SC")
		return domain.FamilyUnknown, false
	}
	return family, true
}

func parseAmount(c *gin.Context, raw string) (domain.Money, bool) {
	amount, err := domain.MoneyFromString(raw)
	if err != nil {
		// Covers both unparseable strings and >4-decimal precision.
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid amount")
		return domain.ZeroMoney(), false
	}
	return amount, true
}
