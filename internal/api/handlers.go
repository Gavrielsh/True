package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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
	game        repository.GameEngine // nil = /spin not enabled
	limiter     *RateLimiter          // nil = player-scope rate limiting disabled
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

// WithGameEngine enables the server-authoritative /spin endpoint. Returns h
// for chaining at wire-up time.
func (h *Handlers) WithGameEngine(g repository.GameEngine) *Handlers {
	h.game = g
	return h
}

// WithRateLimiter enables the per-player sliding window inside each money
// handler. Returns h for chaining at wire-up time.
func (h *Handlers) WithRateLimiter(rl *RateLimiter) *Handlers {
	h.limiter = rl
	return h
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

	// Player-scope rate limit: body is parsed (player_id is signed/verified).
	if playerLimited(c, h.limiter, "bet", playerID) {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	spanCtx, span := moneySpan(c, "http.bet", operatorCode, dto.OperatorTransactionID, playerID,
		attribute.String("currency", family.String()))

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.engine.ProcessBet(ctx, repository.BetRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Family:                family,
		Amount:                amount,
		GameID:                dto.GameID,
		RoundID:               dto.RoundID,
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

	// Player-scope rate limit: body is parsed (player_id is signed/verified).
	if playerLimited(c, h.limiter, "win", playerID) {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	spanCtx, span := moneySpan(c, "http.win", operatorCode, dto.OperatorTransactionID, playerID,
		attribute.String("currency", family.String()))

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.engine.ProcessWin(ctx, repository.WinRequest{
		OperatorCode:           operatorCode,
		OperatorTransactionID:  dto.OperatorTransactionID,
		PlayerID:               playerID,
		Family:                 family,
		Amount:                 amount,
		GameID:                 dto.GameID,
		RoundID:                dto.RoundID,
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

	// Player-scope rate limit: body is parsed (player_id is signed/verified).
	if playerLimited(c, h.limiter, "rollback", playerID) {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	spanCtx, span := moneySpan(c, "http.rollback", operatorCode, dto.OperatorTransactionID, playerID,
		attribute.String("reference_transaction_id", reference.String()))

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.engine.ProcessRollback(ctx, repository.RollbackRequest{
		OperatorCode:           operatorCode,
		OperatorTransactionID:  dto.OperatorTransactionID,
		PlayerID:               playerID,
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

// Spin handles POST /api/v1/spin — the server-authoritative game round.
//
// The caller sends a stake and a game id. The server draws the reels from
// crypto/rand, evaluates the paytable, derives the win, and settles both the
// debit and the credit in one transaction. No caller-supplied field reaches
// the payout amount.
func (h *Handlers) Spin(c *gin.Context) {
	if h.game == nil {
		respondErrorCode(c, http.StatusNotFound, errors.CodeUnsupportedGame, "game engine not enabled")
		return
	}

	var dto spinRequestDTO
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
	betAmount, ok := parseAmount(c, dto.BetAmount)
	if !ok {
		return
	}

	// Player-scope rate limit: body is parsed (player_id is signed/verified).
	if playerLimited(c, h.limiter, "spin", playerID) {
		return
	}

	operatorCode := OperatorCodeFromContext(c.Request.Context())
	spanCtx, span := moneySpan(c, "http.spin", operatorCode, dto.OperatorTransactionID, playerID,
		attribute.String("currency", family.String()),
		attribute.String("game_id", dto.GameID))

	ctx, cancel := context.WithTimeout(spanCtx, h.txTimeout)
	defer cancel()
	result, err := h.game.ProcessSpin(ctx, repository.SpinRequest{
		OperatorCode:          operatorCode,
		OperatorTransactionID: dto.OperatorTransactionID,
		PlayerID:              playerID,
		Family:                family,
		BetAmount:             betAmount,
		GameID:                dto.GameID,
		RoundID:               dto.RoundID,
		Metadata:              dto.Metadata,
		BodyHash:              BodyHashFromContext(c.Request.Context()),
	})
	telemetry.EndSpan(span, err)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, spinResponse{Code: errors.CodeOK, Result: result})
}

// Session handles POST /api/v1/session — a non-locking balance snapshot.
//
// POST with a body (not GET with a query) is a SECURITY requirement, not a
// style choice: the HMAC middleware signs only the raw request body, so a GET
// variant's valid signature would be the constant HMAC(secret, "") — bound to
// neither the player_id, the timestamp, nor the nonce. One captured signature
// would then mint unlimited balance reads for arbitrary players. With the id
// in the body, every request is individually authenticated.
func (h *Handlers) Session(c *gin.Context) {
	var dto sessionRequestDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid request body")
		return
	}

	playerID, ok := parsePlayerID(c, dto.PlayerID)
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

// moneySpan opens the handler-level span for a money endpoint with the
// standard attribute set. Centralizes cursor rule §9 (DATA PRIVACY) for every
// money handler: identifiers only — player_id is a UUID (not PII) — and raw
// amounts are NEVER attached, in extra or otherwise.
func moneySpan(
	c *gin.Context,
	name, operatorCode, opTxID string,
	playerID uuid.UUID,
	extra ...attribute.KeyValue,
) (context.Context, trace.Span) {
	attrs := append([]attribute.KeyValue{
		attribute.String("player_id", playerID.String()),
		attribute.String("operator_code", operatorCode),
		attribute.String("operator_transaction_id", opTxID),
	}, extra...)
	return telemetry.StartSpan(c.Request.Context(), name, attrs...)
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
