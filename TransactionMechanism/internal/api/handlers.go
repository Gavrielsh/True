package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Gavrielsh/TransactionMechanism/internal/domain"
	"github.com/Gavrielsh/TransactionMechanism/internal/repository"
	"github.com/Gavrielsh/TransactionMechanism/pkg/errors"
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
		RequestHash:           RequestHashFromContext(c.Request.Context()),
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
		RequestHash:            RequestHashFromContext(c.Request.Context()),
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
		RequestHash:            RequestHashFromContext(c.Request.Context()),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// CreatePlayer handles POST /api/v1/player — provisions a new user + wallet.
func (h *Handlers) CreatePlayer(c *gin.Context) {
	var dto createPlayerDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}

	if err := h.engine.CreatePlayer(c.Request.Context(), repository.CreatePlayerRequest{
		PlayerID:   playerID,
		ExternalID: dto.ExternalID,
		Email:      dto.Email,
		Username:   dto.Username,
	}); err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": errors.CodeOK, "player_id": dto.PlayerID})
}

// Deposit handles POST /api/v1/deposit — credits the player's GC or SC wallet.
func (h *Handlers) Deposit(c *gin.Context) {
	var dto depositRequestDTO
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

	result, err := h.engine.ProcessDeposit(c.Request.Context(), repository.DepositRequest{
		OperatorCode:          OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Currency:              family,
		Amount:                amount,
		Metadata:              dto.Metadata,
		RequestHash:           RequestHashFromContext(c.Request.Context()),
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

// EscrowReserve handles POST /api/v1/escrow/reserve — locks SC_REDEEMABLE for a
// pending withdrawal. The returned ledger_transaction_id is the
// escrow_transaction_id the operator persists for later commit/release.
func (h *Handlers) EscrowReserve(c *gin.Context) {
	var dto escrowReserveRequestDTO
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

	result, err := h.engine.ProcessEscrowReserve(c.Request.Context(), repository.EscrowReserveRequest{
		OperatorCode:          OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Amount:                amount,
		Metadata:              dto.Metadata,
		RequestHash:           RequestHashFromContext(c.Request.Context()),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// EscrowCommit handles POST /api/v1/escrow/commit — finalises (burns) a reserve.
func (h *Handlers) EscrowCommit(c *gin.Context) {
	var dto escrowCommitRequestDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	escrowID, ok := parseEscrowID(c, dto.EscrowTransactionID)
	if !ok {
		return
	}

	result, err := h.engine.ProcessEscrowCommit(c.Request.Context(), repository.EscrowCommitRequest{
		OperatorCode:          OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		EscrowTransactionID:   escrowID,
		Metadata:              dto.Metadata,
		RequestHash:           RequestHashFromContext(c.Request.Context()),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// EscrowRelease handles POST /api/v1/escrow/release — returns a reserve to the
// player (rejected withdrawal).
func (h *Handlers) EscrowRelease(c *gin.Context) {
	var dto escrowReleaseRequestDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
	if !ok {
		return
	}
	escrowID, ok := parseEscrowID(c, dto.EscrowTransactionID)
	if !ok {
		return
	}

	result, err := h.engine.ProcessEscrowRelease(c.Request.Context(), repository.EscrowReleaseRequest{
		OperatorCode:          OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		EscrowTransactionID:   escrowID,
		Metadata:              dto.Metadata,
		RequestHash:           RequestHashFromContext(c.Request.Context()),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, successResponse{Code: errors.CodeOK, Result: result})
}

// Transactions handles GET /api/v1/transactions?player_id=<uuid>&type=&status=&limit=
// returning the player's ledger history — the financial source of truth.
func (h *Handlers) Transactions(c *gin.Context) {
	playerID, ok := parsePlayerID(c, c.Query("player_id"))
	if !ok {
		return
	}

	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	txns, err := h.engine.ListTransactions(c.Request.Context(), repository.ListTransactionsRequest{
		PlayerID:        playerID,
		Limit:           limit,
		TransactionType: c.Query("type"),
		Status:          c.Query("status"),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, transactionsResponse{
		Code:         errors.CodeOK,
		PlayerID:     playerID.String(),
		Transactions: txns,
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

func parseEscrowID(c *gin.Context, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid escrow_transaction_id")
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
