package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/repository"
	"github.com/Gavrielsh/True/internal/telemetry"
	"github.com/Gavrielsh/True/pkg/errors"
)

const (
	// defaultTxTimeout bounds one state-mutating transaction end-to-end
	// (idempotency → SELECT FOR UPDATE → ledger writes → COMMIT). The raw Gin
	// request context is cancelled if the operator's socket drops, but it has
	// NO upper bound otherwise — a wedged backend or a half-open connection
	// could then hold a wallet row's FOR UPDATE lock indefinitely, head-of-line
	// blocking every spin for that player. A strict derived deadline guarantees
	// pgx cancels the query and releases the lock no matter what the client does.
	defaultTxTimeout = 5 * time.Second
	// defaultReadTimeout bounds the non-locking /session snapshot read.
	defaultReadTimeout = 3 * time.Second
)

// Handlers wires the HTTP endpoints to the repository.Engine. It holds no
// mutable state beyond the engine dependency and the per-request deadlines —
// safe for concurrent use.
type Handlers struct {
	engine      repository.Engine
	txTimeout   time.Duration
	readTimeout time.Duration
}

func NewHandlers(engine repository.Engine) *Handlers {
	return &Handlers{
		engine:      engine,
		txTimeout:   defaultTxTimeout,
		readTimeout: defaultReadTimeout,
	}
}

// NewHandlersWithTimeouts overrides the per-request DB deadlines (e.g. from
// config / load tests). Non-positive values fall back to the defaults.
func NewHandlersWithTimeouts(engine repository.Engine, tx, read time.Duration) *Handlers {
	h := NewHandlers(engine)
	if tx > 0 {
		h.txTimeout = tx
	}
	if read > 0 {
		h.readTimeout = read
	}
	return h
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

	// Span attributes follow cursor rule §9 (DATA PRIVACY): identifiers only —
	// player_id is a UUID (not PII), and raw amounts are NEVER attached.
	spanCtx, span := telemetry.StartSpan(c.Request.Context(), "http.bet",
		attribute.String("player_id", playerID.String()),
		attribute.String("operator_code", OperatorCodeFromContext(c.Request.Context())),
		attribute.String("operator_transaction_id", dto.OperatorTransactionID),
		attribute.String("currency", family.String()),
	)

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.engine.ProcessBet(ctx, repository.BetRequest{
		OperatorCode:          OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Family:                family,
		Amount:                amount,
		GameID:                dto.GameID,
		RoundID:               dto.RoundID,
		Metadata:              dto.Metadata,
	})
	telemetry.EndSpan(span, err)
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

	spanCtx, span := telemetry.StartSpan(c.Request.Context(), "http.win",
		attribute.String("player_id", playerID.String()),
		attribute.String("operator_code", OperatorCodeFromContext(c.Request.Context())),
		attribute.String("operator_transaction_id", dto.OperatorTransactionID),
		attribute.String("currency", family.String()),
	)

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.engine.ProcessWin(ctx, repository.WinRequest{
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
	telemetry.EndSpan(span, err)
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

	spanCtx, span := telemetry.StartSpan(c.Request.Context(), "http.rollback",
		attribute.String("player_id", playerID.String()),
		attribute.String("operator_code", OperatorCodeFromContext(c.Request.Context())),
		attribute.String("operator_transaction_id", dto.OperatorTransactionID),
		attribute.String("reference_transaction_id", reference.String()),
	)

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.engine.ProcessRollback(ctx, repository.RollbackRequest{
		OperatorCode:           OperatorCodeFromContext(c.Request.Context()),
		OperatorTransactionID:  dto.OperatorTransactionID,
		PlayerID:               playerID,
		ReferenceTransactionID: reference,
		Metadata:               dto.Metadata,
	})
	telemetry.EndSpan(span, err)
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

	// Even the non-locking snapshot read gets a hard deadline so a stalled
	// backend can't pin a connection for a dropped client.
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.readTimeout)
	defer cancel()
	wallet, err := h.engine.GetBalances(ctx, playerID)
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
