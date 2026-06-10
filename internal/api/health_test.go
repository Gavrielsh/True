package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"
)

// healthStack builds the full router with an injectable pgxmock pool and a
// real miniredis, returning all three so tests can degrade either dependency.
func healthStack(t *testing.T) (*gin.Engine, pgxmock.PgxPoolIface, *miniredis.Miniredis) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	r := NewRouter(Config{
		Engine:  &fakeEngine{},
		DB:      mock,
		Redis:   client,
		Secrets: map[string]string{"OP1": "s"},
		Logger:  discardLogger(),
	})
	return r, mock, mr
}

func getHealth(t *testing.T, r http.Handler) (int, healthResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health body %q: %v", w.Body.String(), err)
	}
	return w.Code, resp, w.Body.String()
}

func TestHealthz_AllUp(t *testing.T) {
	t.Parallel()
	r, mock, _ := healthStack(t)
	mock.ExpectPing()

	code, resp, _ := getHealth(t, r)
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200", code)
	}
	want := healthResponse{Status: "ok", Postgres: "up", Redis: "up"}
	if resp != want {
		t.Errorf("body: got %+v want %+v", resp, want)
	}
}

func TestHealthz_Degraded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		breakPG      bool
		breakRedis   bool
		wantPostgres string
		wantRedis    string
	}{
		{"postgres_down", true, false, "down", "up"},
		{"redis_down", false, true, "up", "down"},
		{"both_down", true, true, "down", "down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, mock, mr := healthStack(t)
			if tc.breakPG {
				// Driver errors often embed connection-string fragments —
				// exactly what must never reach the response body.
				mock.ExpectPing().WillReturnError(
					errors.New("failed to connect to host=db.internal user=casino_admin"))
			} else {
				mock.ExpectPing()
			}
			if tc.breakRedis {
				mr.Close() // connection refused from now on
			}

			code, resp, raw := getHealth(t, r)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status: got %d want 503", code)
			}
			if resp.Status != "degraded" {
				t.Errorf("status field: got %q want degraded", resp.Status)
			}
			if resp.Postgres != tc.wantPostgres {
				t.Errorf("postgres: got %q want %q", resp.Postgres, tc.wantPostgres)
			}
			if resp.Redis != tc.wantRedis {
				t.Errorf("redis: got %q want %q", resp.Redis, tc.wantRedis)
			}
			if resp.Error == "" {
				t.Error("degraded response must carry an error description")
			}
			// DATA PRIVACY: no driver detail, hosts, or credentials in the body.
			for _, leak := range []string{"db.internal", "casino_admin", "host=", "user="} {
				if strings.Contains(raw, leak) {
					t.Errorf("response body leaks %q: %s", leak, raw)
				}
			}
		})
	}
}

// A nil DB (miswired boot) must read as down — fail closed, never fake-ok.
func TestHealthz_NilDependencyFailsClosed(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	r := NewRouter(Config{
		Engine:  &fakeEngine{},
		DB:      nil,
		Redis:   client,
		Secrets: map[string]string{"OP1": "s"},
		Logger:  discardLogger(),
	})
	code, resp, _ := getHealth(t, r)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", code)
	}
	if resp.Postgres != "down" || resp.Redis != "up" {
		t.Errorf("got postgres=%q redis=%q want down/up", resp.Postgres, resp.Redis)
	}
}
