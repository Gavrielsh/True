package api

// admin.go is the investor/operations API. It is served by a SEPARATE
// gin.Engine on its own port (ADMIN_PORT) wired in cmd/engine — never through
// the operator-facing router — so a gateway misconfiguration can never expose
// it. Auth is a dedicated X-Admin-Key header (constant-time compare), not the
// operator HMAC stack.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/pkg/errors"
)

// HeaderAdminKey authenticates admin requests.
const HeaderAdminKey = "X-Admin-Key"

const (
	adminDefaultLedgerLimit = 50
	adminMaxLedgerLimit     = 200
	adminReadTimeout        = 5 * time.Second
)

// AdminDB is the minimal pgx surface the admin endpoints consume.
// *pgxpool.Pool satisfies it; tests inject pgxmock.
type AdminDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PoolStats is a driver-free snapshot of pgxpool.Stat — the seam lets tests
// fake pool stats without a real pool (pgxpool.Stat is not constructible).
type PoolStats struct {
	TotalConns int32 `json:"total_conns"`
	IdleConns  int32 `json:"idle_conns"`
	InUseConns int32 `json:"in_use_conns"`
	MaxConns   int32 `json:"max_conns"`
}

// AdminConfig wires the admin router's dependencies.
type AdminConfig struct {
	// APIKey is the shared secret checked against X-Admin-Key. Must be
	// non-empty — cmd/engine refuses to start the admin server without it.
	APIKey string
	DB     AdminDB
	// PoolStats snapshots the pgx pool for /health/detailed. Optional.
	PoolStats func() PoolStats
	// Redis supplies client-side pool stats for /health/detailed. Optional.
	Redis  redis.UniversalClient
	Logger *slog.Logger
}

// NewAdminRouter assembles the standalone admin engine.
func NewAdminRouter(cfg AdminConfig) *gin.Engine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(Recovery(logger))
	r.Use(Telemetry(logger))

	h := &adminHandlers{cfg: cfg, logger: logger}
	v1 := r.Group("/admin/v1")
	v1.Use(adminAuth(cfg.APIKey))
	{
		v1.GET("/ggr/daily", h.GGRDaily)
		v1.GET("/players/:player_id/ledger", h.PlayerLedger)
		v1.GET("/health/detailed", h.HealthDetailed)
	}
	return r
}

// adminAuth verifies X-Admin-Key. Both sides are SHA-256 hashed before
// subtle.ConstantTimeCompare so the comparison is constant-time AND leaks no
// key-length information (same pattern rationale as the HMAC middleware).
func adminAuth(apiKey string) gin.HandlerFunc {
	want := sha256.Sum256([]byte(apiKey))
	return func(c *gin.Context) {
		provided := c.GetHeader(HeaderAdminKey)
		got := sha256.Sum256([]byte(provided))
		if provided == "" || apiKey == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			respondErrorCode(c, http.StatusUnauthorized,
				errors.Code("AUTHENTICATION_FAILED"), "invalid admin credentials")
			return
		}
		c.Next()
	}
}

type adminHandlers struct {
	cfg    AdminConfig
	logger *slog.Logger
}

// ----------------------------------------------------------------------------
// GET /admin/v1/ggr/daily?date=YYYY-MM-DD&currency=GC
// ----------------------------------------------------------------------------

// Both GGR variants share one base; the constants are composed at compile
// time (no runtime SQL assembly, no input interpolation).
const (
	adminGGRBase = `
		SELECT gaming_date, currency, total_bets, total_wins, ggr
		FROM daily_ggr
		WHERE gaming_date = $1
	`
	sqlAdminGGRByDate         = adminGGRBase + ` ORDER BY currency`
	sqlAdminGGRByDateCurrency = adminGGRBase + ` AND currency = $2 ORDER BY currency`
)

// ggrRow is one daily_ggr line. All money is decimal-as-string (zero floats).
type ggrRow struct {
	GamingDate string `json:"gaming_date"`
	Currency   string `json:"currency"`
	TotalBets  string `json:"total_bets"`
	TotalWins  string `json:"total_wins"`
	GGR        string `json:"ggr"`
}

// validGGRCurrencies mirrors the currency_type ENUM.
var validGGRCurrencies = map[string]struct{}{
	"GC": {}, "SC_UNPLAYED": {}, "SC_REDEEMABLE": {},
}

func (h *adminHandlers) GGRDaily(c *gin.Context) {
	// date defaults to today UTC (the aggregator buckets in UTC by default).
	dateStr := c.Query("date")
	if dateStr == "" {
		dateStr = time.Now().UTC().Format("2006-01-02")
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount,
			"date must be YYYY-MM-DD")
		return
	}

	currency := c.Query("currency")
	if currency != "" {
		if _, ok := validGGRCurrencies[currency]; !ok {
			respondErrorCode(c, http.StatusBadRequest, errors.CodeUnsupportedCurrency,
				"currency must be GC, SC_UNPLAYED, or SC_REDEEMABLE")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), adminReadTimeout)
	defer cancel()

	var rows pgx.Rows
	if currency == "" {
		rows, err = h.cfg.DB.Query(ctx, sqlAdminGGRByDate, date)
	} else {
		rows, err = h.cfg.DB.Query(ctx, sqlAdminGGRByDateCurrency, date, currency)
	}
	if err != nil {
		h.failInternal(c, "ggr query", err)
		return
	}
	defer rows.Close()

	out := make([]ggrRow, 0, 3)
	for rows.Next() {
		var (
			gamingDate            time.Time
			cur                   string
			bets, wins, ggrAmount decimal.Decimal
		)
		if err := rows.Scan(&gamingDate, &cur, &bets, &wins, &ggrAmount); err != nil {
			h.failInternal(c, "ggr scan", err)
			return
		}
		out = append(out, ggrRow{
			GamingDate: gamingDate.Format("2006-01-02"),
			Currency:   cur,
			TotalBets:  bets.StringFixed(4),
			TotalWins:  wins.StringFixed(4),
			GGR:        ggrAmount.StringFixed(4),
		})
	}
	if err := rows.Err(); err != nil {
		h.failInternal(c, "ggr rows", err)
		return
	}

	if currency != "" {
		if len(out) == 0 {
			respondErrorCode(c, http.StatusNotFound, errors.Code("NOT_FOUND"),
				"no GGR recorded for that date and currency")
			return
		}
		c.JSON(http.StatusOK, out[0])
		return
	}
	c.JSON(http.StatusOK, gin.H{"date": dateStr, "rows": out})
}

// ----------------------------------------------------------------------------
// GET /admin/v1/players/:player_id/ledger?limit=50&cursor=<uuid>
// ----------------------------------------------------------------------------

// Amount is derived from the PLAYER_WALLET entries (the header carries no
// amount by design — the ledger entries are the source of truth). For a BET
// split across SC_UNPLAYED/SC_REDEEMABLE this is the total player movement.
// The first-page and cursor variants share one base and tail; only the
// cursor predicate differs. Composed at compile time — no runtime SQL
// assembly, no input interpolation.
const (
	adminLedgerBase = `
		SELECT t.id, t.operator_transaction_id, t.transaction_type, t.status,
		       t.created_at, COALESCE(SUM(e.amount), 0) AS amount
		FROM ledger_transactions t
		LEFT JOIN ledger_entries e
		  ON e.ledger_transaction_id = t.id
		 AND e.account_type = 'PLAYER_WALLET'
		WHERE t.player_id = $1
	`
	adminLedgerTail = `
		GROUP BY t.id, t.operator_transaction_id, t.transaction_type, t.status, t.created_at
		ORDER BY t.id DESC
		LIMIT $2
	`
	sqlAdminPlayerLedger      = adminLedgerBase + adminLedgerTail
	sqlAdminPlayerLedgerAfter = adminLedgerBase + ` AND t.id < $3` + adminLedgerTail
)

type ledgerTxRow struct {
	ID                    string `json:"id"`
	OperatorTransactionID string `json:"operator_transaction_id"`
	TransactionType       string `json:"transaction_type"`
	Amount                string `json:"amount"`
	CreatedAt             string `json:"created_at"`
	Status                string `json:"status"`
}

func (h *adminHandlers) PlayerLedger(c *gin.Context) {
	playerID, err := uuid.Parse(c.Param("player_id"))
	if err != nil {
		respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid player_id")
		return
	}

	limit := adminDefaultLedgerLimit
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid limit")
			return
		}
		if n > adminMaxLedgerLimit {
			n = adminMaxLedgerLimit
		}
		limit = n
	}

	var cursor uuid.UUID
	if raw := c.Query("cursor"); raw != "" {
		cursor, err = uuid.Parse(raw)
		if err != nil {
			respondErrorCode(c, http.StatusBadRequest, errors.CodeInvalidAmount, "invalid cursor")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), adminReadTimeout)
	defer cancel()

	var rows pgx.Rows
	if cursor == uuid.Nil {
		rows, err = h.cfg.DB.Query(ctx, sqlAdminPlayerLedger, playerID, limit)
	} else {
		rows, err = h.cfg.DB.Query(ctx, sqlAdminPlayerLedgerAfter, playerID, limit, cursor)
	}
	if err != nil {
		h.failInternal(c, "ledger query", err)
		return
	}
	defer rows.Close()

	out := make([]ledgerTxRow, 0, limit)
	for rows.Next() {
		var (
			id        uuid.UUID
			opTxID    string
			txType    string
			status    string
			createdAt time.Time
			amount    decimal.Decimal
		)
		if err := rows.Scan(&id, &opTxID, &txType, &status, &createdAt, &amount); err != nil {
			h.failInternal(c, "ledger scan", err)
			return
		}
		out = append(out, ledgerTxRow{
			ID:                    id.String(),
			OperatorTransactionID: opTxID,
			TransactionType:       txType,
			Amount:                amount.StringFixed(4),
			CreatedAt:             createdAt.UTC().Format(time.RFC3339Nano),
			Status:                status,
		})
	}
	if err := rows.Err(); err != nil {
		h.failInternal(c, "ledger rows", err)
		return
	}

	resp := gin.H{"player_id": playerID.String(), "transactions": out}
	// A full page means more may exist; the last id is the next cursor.
	if len(out) == limit {
		resp["next_cursor"] = out[len(out)-1].ID
	}
	c.JSON(http.StatusOK, resp)
}

// ----------------------------------------------------------------------------
// GET /admin/v1/health/detailed
// ----------------------------------------------------------------------------

// detailedHealth carries operational stats ONLY — no connection strings, no
// credentials, no addresses (cursor rule §9).
type detailedHealth struct {
	Status     string          `json:"status"`
	Pool       *PoolStats      `json:"postgres_pool,omitempty"`
	Redis      *redisPoolStats `json:"redis_pool,omitempty"`
	Goroutines int             `json:"goroutines"`
	Memory     memorySnapshot  `json:"memory"`
}

type redisPoolStats struct {
	Hits       uint32 `json:"hits"`
	Misses     uint32 `json:"misses"`
	Timeouts   uint32 `json:"timeouts"`
	TotalConns uint32 `json:"total_conns"`
	IdleConns  uint32 `json:"idle_conns"`
	StaleConns uint32 `json:"stale_conns"`
}

type memorySnapshot struct {
	AllocBytes     uint64 `json:"alloc_bytes"`
	TotalAllocB    uint64 `json:"total_alloc_bytes"`
	SysBytes       uint64 `json:"sys_bytes"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapObjects    uint64 `json:"heap_objects"`
	NumGC          uint32 `json:"num_gc"`
}

func (h *adminHandlers) HealthDetailed(c *gin.Context) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	resp := detailedHealth{
		Status:     "ok",
		Goroutines: runtime.NumGoroutine(),
		Memory: memorySnapshot{
			AllocBytes:     mem.Alloc,
			TotalAllocB:    mem.TotalAlloc,
			SysBytes:       mem.Sys,
			HeapAllocBytes: mem.HeapAlloc,
			HeapObjects:    mem.HeapObjects,
			NumGC:          mem.NumGC,
		},
	}
	if h.cfg.PoolStats != nil {
		s := h.cfg.PoolStats()
		resp.Pool = &s
	}
	if h.cfg.Redis != nil {
		if s := h.cfg.Redis.PoolStats(); s != nil {
			resp.Redis = &redisPoolStats{
				Hits:       s.Hits,
				Misses:     s.Misses,
				Timeouts:   s.Timeouts,
				TotalConns: s.TotalConns,
				IdleConns:  s.IdleConns,
				StaleConns: s.StaleConns,
			}
		}
	}
	c.JSON(http.StatusOK, resp)
}

// failInternal logs the raw error (structured, server-side only) and returns
// an opaque 500 — admin responses never carry SQL or driver detail.
func (h *adminHandlers) failInternal(c *gin.Context, op string, err error) {
	h.logger.ErrorContext(c.Request.Context(), "admin endpoint failed",
		slog.String("op", op),
		slog.String("error", err.Error()),
	)
	respondErrorCode(c, http.StatusInternalServerError, errors.CodeInternal, "internal error")
}
