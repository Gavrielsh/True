//go:build integration

package main

// Integration coverage for the ledger append-only invariant (Milestone 0.3).
//
// These tests exist because the invariant was previously asserted only in
// comments. 000004 documented a grant set it never applied and shipped a
// COMMENT ON TABLE claiming UPDATE/DELETE were blocked; 000008 then cited those
// non-existent grants as the reason it only needed to protect
// ledger_transactions. The result was the exact inverse of the documented
// design: the transaction header was guarded while the money lines accepted
// UPDATE, DELETE and TRUNCATE.
//
// A schema-only assertion would not have caught that, because the schema looked
// correct. So these tests assert BEHAVIOUR against a real Postgres as the real
// application role, and pin the privilege set as an EXACT match so a later
// migration cannot widen it unnoticed.
//
//	TEST_POSTGRES_URL=postgres://user:pass@localhost:5432/true_test?sslmode=disable \
//	  go test -tags integration ./cmd/engine/
//
// TEST_POSTGRES_URL must be a privileged (owner/superuser) connection: the
// migrations create roles and grants, and the tests create a throwaway login
// user to exercise the restricted path. Point it at a throwaway database only.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// A throwaway LOGIN user granted membership in engine_writer. The engine
	// connects this way in production: engine_writer itself is NOLOGIN, so the
	// privilege set is versioned in migration SQL while the login credential
	// stays environment-specific.
	testLoginUser = "engine_writer_it_login"
	testLoginPass = "integration-test-only"
)

// wantGrants is the AUTHORITATIVE privilege set for engine_writer, asserted as
// an exact match. Every entry is justified by a call site in the engine:
//
//   - The two ledger tables are APPEND-ONLY: INSERT + SELECT and nothing else.
//     No UPDATE (a settled entry is never rewritten), no DELETE (history is
//     never erased), no TRUNCATE (the table is never emptied).
//   - The rest carry exactly the privileges the code exercises, and no more.
//     000004's documented alternative — INSERT+SELECT on ALL TABLES — would
//     have broken every one of them.
var wantGrants = map[string][]string{
	"ledger_entries":           {"INSERT", "SELECT"},
	"ledger_transactions":      {"INSERT", "SELECT"},
	"ledger_transaction_dedup": {"DELETE", "INSERT", "SELECT"}, // retention-pruned, not financial history
	"wallets":                  {"INSERT", "SELECT", "UPDATE"}, // balances mutate under SELECT ... FOR UPDATE
	"users":                    {"INSERT", "SELECT"},
	"daily_ggr":                {"INSERT", "SELECT", "UPDATE"}, // written by an UPSERT
	"ggr_aggregator_state":     {"SELECT", "UPDATE"},           // watermark row
}

func integrationURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL not set; skipping integration test")
	}
	return url
}

// migrateAndConnect applies every migration and returns a privileged connection.
func migrateAndConnect(ctx context.Context, t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	dbURL := integrationURL(t)
	if err := RunMigrations(ctx, testLogger(), dbURL); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn, dbURL
}

// TestLedgerGrants_ExactPrivilegeSet pins the engine_writer privilege set.
//
// The comparison is EXACT, not a superset check. A superset check would pass
// while a later migration quietly added UPDATE to a ledger table — which is
// precisely the failure mode this milestone remediates.
func TestLedgerGrants_ExactPrivilegeSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _ := migrateAndConnect(ctx, t)

	rows, err := conn.Query(ctx, `
		SELECT table_name, privilege_type
		FROM information_schema.role_table_grants
		WHERE grantee = 'engine_writer'`)
	if err != nil {
		t.Fatalf("query grants: %v", err)
	}
	defer rows.Close()

	got := map[string][]string{}
	for rows.Next() {
		var table, priv string
		if err := rows.Scan(&table, &priv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[table] = append(got[table], priv)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for k := range got {
		sort.Strings(got[k])
	}

	// Tables that must carry NO grant at all are caught here: an unexpected key
	// in got, or a missing key from want, both fail.
	for table, want := range wantGrants {
		g, ok := got[table]
		if !ok {
			t.Errorf("%s: engine_writer has NO grants, want %v", table, want)
			continue
		}
		if strings.Join(g, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v, want exactly %v", table, g, want)
		}
	}
	for table, g := range got {
		if _, ok := wantGrants[table]; !ok {
			t.Errorf("%s: unexpected grants %v — engine_writer should hold none", table, g)
		}
	}
}

// TestLedgerGrants_ForbiddenPrivilegesNeverGranted states the prohibition
// directly, so the intent survives even if wantGrants is ever edited casually.
func TestLedgerGrants_ForbiddenPrivilegesNeverGranted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _ := migrateAndConnect(ctx, t)

	for _, table := range []string{"ledger_entries", "ledger_transactions"} {
		for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
			var n int
			if err := conn.QueryRow(ctx, `
				SELECT count(*) FROM information_schema.role_table_grants
				WHERE grantee = 'engine_writer' AND table_name = $1 AND privilege_type = $2`,
				table, priv).Scan(&n); err != nil {
				t.Fatalf("query: %v", err)
			}
			if n != 0 {
				t.Errorf("%s: %s is granted to engine_writer and must never be", table, priv)
			}
		}
	}

	// TRUNCATE must be granted to engine_writer on NOTHING, anywhere.
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.role_table_grants
		WHERE grantee = 'engine_writer' AND privilege_type = 'TRUNCATE'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Errorf("TRUNCATE is granted to engine_writer on %d table(s); expected none", n)
	}
}

// TestLedgerGrants_NoPartitionGrants guards the bypass route.
//
// Privileges on a partitioned parent cover DML routed through it, so the
// partitions need no grants of their own. If a partition DID carry one, a
// statement naming that partition directly would sidestep the parent's
// restrictions entirely.
func TestLedgerGrants_NoPartitionGrants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _ := migrateAndConnect(ctx, t)

	rows, err := conn.Query(ctx, `
		SELECT table_name, privilege_type
		FROM information_schema.role_table_grants
		WHERE grantee = 'engine_writer'
		  AND (table_name LIKE 'ledger_entries\_%' OR table_name LIKE 'ledger_transactions\_%')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, priv string
		if err := rows.Scan(&table, &priv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("partition %s carries grant %s; partitions must hold none", table, priv)
	}
}

// appLoginURL provisions the throwaway login user and returns its DSN.
func appLoginURL(ctx context.Context, t *testing.T, conn *pgx.Conn, adminURL string) string {
	t.Helper()
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`DO $$ BEGIN
		    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') THEN
		        CREATE ROLE %s LOGIN PASSWORD '%s';
		    END IF;
		 END $$;`, testLoginUser, testLoginUser, testLoginPass)); err != nil {
		t.Fatalf("create login role: %v", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("GRANT engine_writer TO %s", testLoginUser)); err != nil {
		t.Fatalf("grant membership: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_URL: %v", err)
	}
	u.User = url.UserPassword(testLoginUser, testLoginPass)
	return u.String()
}

// sqlState returns the Postgres SQLSTATE of err, or "" if it is not a PgError.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestLedgerGrants_AppendOnlyEnforced is the behavioural core: connect as the
// real application role and prove what it can and cannot do.
//
// Schema assertions alone would not have caught the original defect, because
// the schema looked right. This exercises the actual statements.
func TestLedgerGrants_AppendOnlyEnforced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, adminURL := migrateAndConnect(ctx, t)
	appURL := appLoginURL(ctx, t, admin, adminURL)

	// Seed a player through the privileged connection.
	playerID := "11111111-1111-1111-1111-111111111111"
	if _, err := admin.Exec(ctx, `
		INSERT INTO users (id, external_id, username, country_code)
		VALUES ($1, 'it-ext-1', 'it-player-1', 'GB')
		ON CONFLICT (id) DO NOTHING`, playerID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	app, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatalf("connect as %s: %v", testLoginUser, err)
	}
	defer func() { _ = app.Close(context.Background()) }()

	// --- The write path must still work end to end. -------------------------
	txID := "22222222-2222-2222-2222-222222222222"
	if _, err := app.Exec(ctx, `
		INSERT INTO ledger_transactions (id, operator_code, operator_transaction_id, player_id, transaction_type)
		VALUES ($1, 'IT_OP', $2, $3, 'BET')`,
		txID, fmt.Sprintf("it-tx-%d", time.Now().UnixNano()), playerID); err != nil {
		t.Fatalf("INSERT ledger_transactions must succeed for engine_writer: %v", err)
	}
	if _, err := app.Exec(ctx, `
		INSERT INTO ledger_entries (ledger_transaction_id, player_id, account_type, currency, direction, amount, balance_after)
		VALUES ($1, $2, 'PLAYER_WALLET', 'SC_REDEEMABLE', 'DEBIT', 10.0000, 90.0000)`,
		txID, playerID); err != nil {
		t.Fatalf("INSERT ledger_entries must succeed for engine_writer: %v", err)
	}

	// --- Mutation must be refused, on the parent and the partition alike. ---
	// 42501 = insufficient_privilege (grants), 0A000 = feature_not_supported
	// (the 000008 trigger). Either is a correct refusal; both are asserted to
	// be a refusal rather than a silent success.
	mutations := []struct {
		name string
		sql  string
	}{
		{"UPDATE ledger_entries", `UPDATE ledger_entries SET amount = 999999.0000`},
		{"DELETE ledger_entries", `DELETE FROM ledger_entries`},
		{"TRUNCATE ledger_entries", `TRUNCATE ledger_entries`},
		{"UPDATE ledger_transactions", `UPDATE ledger_transactions SET status = 'COMPLETED'`},
		{"DELETE ledger_transactions", `DELETE FROM ledger_transactions`},
		{"TRUNCATE ledger_transactions", `TRUNCATE ledger_transactions`},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			_, err := app.Exec(ctx, m.sql)
			if err == nil {
				t.Fatalf("%s SUCCEEDED; the ledger must be append-only", m.name)
			}
			if got := sqlState(err); got != "42501" && got != "0A000" {
				t.Errorf("%s: refused with SQLSTATE %q, want 42501 or 0A000 (err: %v)", m.name, got, err)
			}
		})
	}

	// --- The partition bypass. ----------------------------------------------
	// Naming a partition directly must not evade the parent's restrictions.
	var partition string
	if err := admin.QueryRow(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'ledger_entries' AND c.relname <> 'ledger_entries_default'
		LIMIT 1`).Scan(&partition); err != nil {
		t.Fatalf("find a partition: %v", err)
	}
	t.Run("UPDATE partition directly", func(t *testing.T) {
		_, err := app.Exec(ctx, fmt.Sprintf("UPDATE %s SET amount = 999999.0000", partition))
		if err == nil {
			t.Fatalf("UPDATE %s SUCCEEDED; partitions must not be a bypass", partition)
		}
		if got := sqlState(err); got != "42501" && got != "0A000" {
			t.Errorf("UPDATE %s: SQLSTATE %q, want 42501 or 0A000 (err: %v)", partition, got, err)
		}
	})

	// --- Partition maintenance must still work, without schema DDL rights. --
	t.Run("partitioner path works", func(t *testing.T) {
		if _, err := app.Exec(ctx, `SELECT ensure_ledger_partitions(1)`); err != nil {
			t.Errorf("engine_writer must be able to pre-create partitions: %v", err)
		}
		if _, err := app.Exec(ctx, `SELECT create_daily_partition('ledger_entries', current_date + 30)`); err != nil {
			t.Errorf("engine_writer must be able to call create_daily_partition: %v", err)
		}
	})

	t.Run("raw DDL still denied", func(t *testing.T) {
		if _, err := app.Exec(ctx, `CREATE TABLE it_should_not_exist (x int)`); err == nil {
			t.Error("engine_writer created a table; it must hold no CREATE on the schema")
		}
	})

	// The definer function must refuse a parent outside the ledger allowlist,
	// or it would be a general-purpose table-creation primitive.
	t.Run("definer function rejects unmanaged parent", func(t *testing.T) {
		if _, err := app.Exec(ctx, `SELECT create_daily_partition('users', current_date)`); err == nil {
			t.Error("create_daily_partition accepted a non-ledger parent")
		}
	})
}

// TestLedgerGrants_TriggerBlocksEvenOwner proves the second layer.
//
// Grants are INERT for a table owner or superuser, which is why a misconfigured
// POSTGRES_URL could otherwise reopen the hole. The 000008 triggers fire
// regardless of role, so the money lines survive even a privileged connection.
func TestLedgerGrants_TriggerBlocksEvenOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, _ := migrateAndConnect(ctx, t)

	var superuser bool
	if err := admin.QueryRow(ctx, `SELECT current_setting('is_superuser') = 'on'`).Scan(&superuser); err != nil {
		t.Fatalf("check superuser: %v", err)
	}

	for _, m := range []struct{ name, sql string }{
		{"UPDATE ledger_entries", `UPDATE ledger_entries SET amount = 999999.0000`},
		{"DELETE ledger_entries", `DELETE FROM ledger_entries`},
		{"TRUNCATE ledger_entries", `TRUNCATE ledger_entries`},
		{"UPDATE ledger_transactions", `UPDATE ledger_transactions SET status = 'COMPLETED'`},
	} {
		t.Run(m.name, func(t *testing.T) {
			_, err := admin.Exec(ctx, m.sql)
			if err == nil {
				t.Fatalf("%s SUCCEEDED as a privileged user (superuser=%t); "+
					"the 000008 trigger must block it regardless of role", m.name, superuser)
			}
			if got := sqlState(err); got != "0A000" {
				t.Errorf("%s: SQLSTATE %q, want 0A000 from the append-only trigger (err: %v)",
					m.name, got, err)
			}
		})
	}
}

// TestAssertLedgerAppendOnly_BootGuard covers the boot-time check itself.
//
// The guard is the reason a correct grant set cannot be quietly bypassed by
// repointing POSTGRES_URL at the admin user: grants are inert for an owner or
// superuser, so identity has to be checked at startup rather than assumed.
func TestAssertLedgerAppendOnly_BootGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, adminURL := migrateAndConnect(ctx, t)
	appURL := appLoginURL(ctx, t, admin, adminURL)

	// A privileged connection MUST be refused.
	t.Run("rejects owner/superuser", func(t *testing.T) {
		pool, err := pgxpool.New(ctx, adminURL)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer pool.Close()

		err = assertLedgerAppendOnly(ctx, pool, testLogger())
		if err == nil {
			t.Fatal("boot guard accepted a connection that can mutate the ledger")
		}
		for _, want := range []string{"REFUSING TO BOOT", "engine_writer"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must mention %q, got: %v", want, err)
			}
		}
	})

	// The correctly-provisioned application role MUST be accepted.
	t.Run("accepts engine_writer member", func(t *testing.T) {
		pool, err := pgxpool.New(ctx, appURL)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer pool.Close()

		if err := assertLedgerAppendOnly(ctx, pool, testLogger()); err != nil {
			t.Fatalf("boot guard rejected the correctly-provisioned role: %v", err)
		}
	})
}
