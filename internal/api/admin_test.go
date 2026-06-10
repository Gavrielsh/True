package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

const testAdminKey = "test-admin-key-7f3a"

// adminStack builds the standalone admin router over a pgxmock pool.
func adminStack(t *testing.T) (http.Handler, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(func() {
		mock.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})
	r := NewAdminRouter(AdminConfig{
		APIKey: testAdminKey,
		DB:     mock,
		PoolStats: func() PoolStats {
			return PoolStats{TotalConns: 9, IdleConns: 4, InUseConns: 5, MaxConns: 17}
		},
		Logger: discardLogger(),
	})
	return r, mock
}

func adminGet(t *testing.T, r http.Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set(HeaderAdminKey, key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ----------------------------------------------------------------------------
// Auth: missing / wrong / right key, across every admin endpoint.
// ----------------------------------------------------------------------------

func TestAdminAuth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
		want int
	}{
		{"missing_key", "", http.StatusUnauthorized},
		{"wrong_key", "not-the-key", http.StatusUnauthorized},
		{"prefix_of_key", testAdminKey[:len(testAdminKey)-1], http.StatusUnauthorized},
		{"right_key", testAdminKey, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := adminStack(t)
			// health/detailed needs no DB expectations — isolates auth.
			w := adminGet(t, r, "/admin/v1/health/detailed", tc.key)
			if w.Code != tc.want {
				t.Errorf("status: got %d want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// An admin router must never accept an empty configured key (would match an
// empty header).
func TestAdminAuth_EmptyConfiguredKeyRejectsAll(t *testing.T) {
	t.Parallel()
	r := NewAdminRouter(AdminConfig{APIKey: "", Logger: discardLogger()})
	w := adminGet(t, r, "/admin/v1/health/detailed", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", w.Code)
	}
}

// ----------------------------------------------------------------------------
// GET /admin/v1/ggr/daily
// ----------------------------------------------------------------------------

func ggrMockRows(date time.Time) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"gaming_date", "currency", "total_bets", "total_wins", "ggr"}).
		AddRow(date, "GC", decimal.RequireFromString("1000.0000"),
			decimal.RequireFromString("900.0000"), decimal.RequireFromString("100.0000"))
}

func TestAdminGGRDaily_ByCurrency(t *testing.T) {
	t.Parallel()
	r, mock := adminStack(t)
	date := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT gaming_date, currency, total_bets, total_wins, ggr\s+FROM daily_ggr\s+WHERE gaming_date = \$1\s+AND currency = \$2`).
		WithArgs(date, "GC").
		WillReturnRows(ggrMockRows(date))

	w := adminGet(t, r, "/admin/v1/ggr/daily?date=2026-06-01&currency=GC", testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", w.Code, w.Body.String())
	}
	var row ggrRow
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := ggrRow{GamingDate: "2026-06-01", Currency: "GC",
		TotalBets: "1000.0000", TotalWins: "900.0000", GGR: "100.0000"}
	if row != want {
		t.Errorf("row: got %+v want %+v", row, want)
	}
}

func TestAdminGGRDaily_AllCurrencies(t *testing.T) {
	t.Parallel()
	r, mock := adminStack(t)
	date := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	rows := pgxmock.NewRows([]string{"gaming_date", "currency", "total_bets", "total_wins", "ggr"}).
		AddRow(date, "GC", decimal.RequireFromString("10.0000"),
			decimal.RequireFromString("5.0000"), decimal.RequireFromString("5.0000")).
		AddRow(date, "SC_REDEEMABLE", decimal.RequireFromString("7.0000"),
			decimal.RequireFromString("3.0000"), decimal.RequireFromString("4.0000"))
	mock.ExpectQuery(`SELECT gaming_date, currency, total_bets, total_wins, ggr\s+FROM daily_ggr\s+WHERE gaming_date = \$1\s+ORDER BY currency`).
		WithArgs(date).
		WillReturnRows(rows)

	w := adminGet(t, r, "/admin/v1/ggr/daily?date=2026-06-01", testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Date string   `json:"date"`
		Rows []ggrRow `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(resp.Rows))
	}
	if resp.Rows[1].Currency != "SC_REDEEMABLE" || resp.Rows[1].GGR != "4.0000" {
		t.Errorf("row[1]: got %+v", resp.Rows[1])
	}
}

func TestAdminGGRDaily_BadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"bad_date", "/admin/v1/ggr/daily?date=06-01-2026"},
		{"bad_currency", "/admin/v1/ggr/daily?date=2026-06-01&currency=DOGE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := adminStack(t)
			w := adminGet(t, r, tc.path, testAdminKey)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// ----------------------------------------------------------------------------
// GET /admin/v1/players/:player_id/ledger
// ----------------------------------------------------------------------------

func TestAdminPlayerLedger_FirstPageAndCursor(t *testing.T) {
	t.Parallel()
	r, mock := adminStack(t)
	playerID := uuid.New()
	id1, id2 := uuid.New(), uuid.New()
	now := time.Now().UTC()

	rows := pgxmock.NewRows([]string{"id", "operator_transaction_id", "transaction_type", "status", "created_at", "amount"}).
		AddRow(id1, "op-1", "BET", "COMPLETED", now, decimal.RequireFromString("10.0000")).
		AddRow(id2, "op-2", "WIN", "COMPLETED", now, decimal.RequireFromString("4.5000"))
	// limit=2 and a full page → next_cursor must be the last row's id.
	mock.ExpectQuery(`FROM ledger_transactions t\s+LEFT JOIN ledger_entries e`).
		WithArgs(playerID, 2).
		WillReturnRows(rows)

	w := adminGet(t, r, "/admin/v1/players/"+playerID.String()+"/ledger?limit=2", testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Transactions []ledgerTxRow `json:"transactions"`
		NextCursor   string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Transactions) != 2 {
		t.Fatalf("transactions: got %d want 2", len(resp.Transactions))
	}
	if resp.Transactions[0].Amount != "10.0000" || resp.Transactions[0].TransactionType != "BET" {
		t.Errorf("tx[0]: got %+v", resp.Transactions[0])
	}
	if resp.NextCursor != id2.String() {
		t.Errorf("next_cursor: got %q want %q", resp.NextCursor, id2)
	}

	// Second page: cursor flows into the id < $cursor variant.
	empty := pgxmock.NewRows([]string{"id", "operator_transaction_id", "transaction_type", "status", "created_at", "amount"})
	mock.ExpectQuery(`AND t\.id < \$3`).
		WithArgs(playerID, 2, id2).
		WillReturnRows(empty)
	w2 := adminGet(t, r, "/admin/v1/players/"+playerID.String()+"/ledger?limit=2&cursor="+id2.String(), testAdminKey)
	if w2.Code != http.StatusOK {
		t.Fatalf("page2 status: got %d; body=%s", w2.Code, w2.Body.String())
	}
}

func TestAdminPlayerLedger_LimitClampedTo200(t *testing.T) {
	t.Parallel()
	r, mock := adminStack(t)
	playerID := uuid.New()
	empty := pgxmock.NewRows([]string{"id", "operator_transaction_id", "transaction_type", "status", "created_at", "amount"})
	mock.ExpectQuery(`FROM ledger_transactions t`).
		WithArgs(playerID, adminMaxLedgerLimit). // 5000 must clamp to 200
		WillReturnRows(empty)

	w := adminGet(t, r, "/admin/v1/players/"+playerID.String()+"/ledger?limit=5000", testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminPlayerLedger_BadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"bad_player_id", "/admin/v1/players/not-a-uuid/ledger"},
		{"bad_limit", "/admin/v1/players/" + uuid.NewString() + "/ledger?limit=zero"},
		{"negative_limit", "/admin/v1/players/" + uuid.NewString() + "/ledger?limit=-5"},
		{"bad_cursor", "/admin/v1/players/" + uuid.NewString() + "/ledger?cursor=xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := adminStack(t)
			w := adminGet(t, r, tc.path, testAdminKey)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// ----------------------------------------------------------------------------
// GET /admin/v1/health/detailed
// ----------------------------------------------------------------------------

func TestAdminHealthDetailed(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	r := NewAdminRouter(AdminConfig{
		APIKey: testAdminKey,
		PoolStats: func() PoolStats {
			return PoolStats{TotalConns: 9, IdleConns: 4, InUseConns: 5, MaxConns: 17}
		},
		Redis:  client,
		Logger: discardLogger(),
	})

	w := adminGet(t, r, "/admin/v1/health/detailed", testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", w.Code, w.Body.String())
	}
	var resp detailedHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Pool == nil || resp.Pool.TotalConns != 9 || resp.Pool.MaxConns != 17 {
		t.Errorf("pool stats: got %+v", resp.Pool)
	}
	if resp.Redis == nil {
		t.Error("redis pool stats missing")
	}
	if resp.Goroutines <= 0 {
		t.Errorf("goroutines: got %d want > 0", resp.Goroutines)
	}
	if resp.Memory.SysBytes == 0 {
		t.Error("memory stats missing")
	}
}
