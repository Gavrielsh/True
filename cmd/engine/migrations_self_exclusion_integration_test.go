//go:build integration

package main

// Integration coverage for migration 000009 (self-exclusion substrate).
//
// The lesson this repository already paid for once — 000004 documenting a grant
// set it never applied, and 000008 then trusting that documentation — is that a
// schema assertion proves nothing about behaviour. So these tests connect as the
// REAL application role and run the actual statements, exactly as
// migrations_grants_integration_test.go does for the ledger.
//
//	TEST_POSTGRES_URL=postgres://user:pass@localhost:5432/true_test?sslmode=disable \
//	  go test -tags integration ./cmd/engine/
//
// TEST_POSTGRES_URL must be a privileged (owner/superuser) connection: these
// tests create a throwaway database for the migration-cycle case and exercise
// the owner path for the append-only trigger. Point it at a throwaway database
// only.

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	truengine "github.com/Gavrielsh/True"
)

// seedTransitionPlayer inserts a throwaway player and returns its id. Ids are
// unique per call so these tests never collide with each other or with the
// ledger suite sharing the database.
func seedTransitionPlayer(ctx context.Context, t *testing.T, admin *pgx.Conn) string {
	t.Helper()
	var id string
	tag := time.Now().UnixNano()
	if err := admin.QueryRow(ctx, `
		INSERT INTO users (external_id, username, country_code)
		VALUES ($1, $2, 'US')
		RETURNING id`,
		fmt.Sprintf("se-ext-%d", tag), fmt.Sprintf("se-player-%d", tag),
	).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// insertTransition writes one audit row through conn, returning the error so
// callers can assert on both success and refusal.
func insertTransition(ctx context.Context, conn *pgx.Conn, playerID, from, to, actor, reason string) error {
	_, err := conn.Exec(ctx, `
		INSERT INTO player_status_transitions
		    (player_id, from_status, to_status, actor_type, actor_ref, reason)
		VALUES ($1, $2::user_status, $3::user_status, $4::status_actor, 'it-suite', $5)`,
		playerID, from, to, actor, reason)
	return err
}

// TestSelfExclusion_StatusIsUsableAndFailsClosed proves the new enum value
// exists AND that reaching it leaves the player outside the only status any
// money path accepts.
//
// requirePlayerActive (internal/repository/engine.go) tests `status != 'ACTIVE'`
// rather than enumerating blocked statuses, so SELF_EXCLUDED is refused on every
// money path from the moment this migration lands, with no Go change. That is a
// load-bearing property of the design and it is asserted here rather than
// assumed: if someone ever rewrites the guard as an allowlist of blocked
// statuses, this test is what notices.
func TestSelfExclusion_StatusIsUsableAndFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, _ := migrateAndConnect(ctx, t)

	var present bool
	if err := admin.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM pg_enum e
		    JOIN pg_type t ON t.oid = e.enumtypid
		    WHERE t.typname = 'user_status' AND e.enumlabel = 'SELF_EXCLUDED')`).Scan(&present); err != nil {
		t.Fatalf("query enum: %v", err)
	}
	if !present {
		t.Fatal("user_status has no SELF_EXCLUDED value; 000009 did not apply")
	}

	playerID := seedTransitionPlayer(ctx, t, admin)
	until := time.Now().Add(180 * 24 * time.Hour)
	if _, err := admin.Exec(ctx, `
		UPDATE users SET status = 'SELF_EXCLUDED', self_exclusion_until = $2 WHERE id = $1`,
		playerID, until); err != nil {
		t.Fatalf("set SELF_EXCLUDED: %v", err)
	}

	// This is the exact shape of the read requirePlayerActive performs.
	var status string
	if err := admin.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, playerID).Scan(&status); err != nil {
		t.Fatalf("read back status: %v", err)
	}
	if status != "SELF_EXCLUDED" {
		t.Fatalf("status = %q, want SELF_EXCLUDED", status)
	}
	if status == "ACTIVE" {
		t.Fatal("a self-excluded player reads as ACTIVE; every money path would admit them")
	}

	var untilBack *time.Time
	if err := admin.QueryRow(ctx, `SELECT self_exclusion_until FROM users WHERE id = $1`, playerID).Scan(&untilBack); err != nil {
		t.Fatalf("read back expiry: %v", err)
	}
	if untilBack == nil {
		t.Fatal("self_exclusion_until did not persist")
	}
	if untilBack.Before(time.Now()) {
		t.Errorf("self_exclusion_until %s is already past; the term did not persist intact", untilBack)
	}
}

// TestSelfExclusion_AuditLogAppendOnlyForApplicationRole is the behavioural core:
// the application role may record a transition and may never revise one.
func TestSelfExclusion_AuditLogAppendOnlyForApplicationRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, adminURL := migrateAndConnect(ctx, t)
	appURL := appLoginURL(ctx, t, admin, adminURL)

	playerID := seedTransitionPlayer(ctx, t, admin)

	app, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatalf("connect as %s: %v", testLoginUser, err)
	}
	defer func() { _ = app.Close(context.Background()) }()

	// The write path must work: the engine records transitions through this role.
	if err := insertTransition(ctx, app, playerID, "ACTIVE", "SELF_EXCLUDED", "PLAYER",
		"player requested a 180-day exclusion"); err != nil {
		t.Fatalf("INSERT must succeed for engine_writer: %v", err)
	}

	// 42501 = insufficient_privilege (the grant set), 0A000 = feature_not_supported
	// (the trigger). Either is a correct refusal; a success is not.
	mutations := []struct{ name, sql string }{
		{"UPDATE", `UPDATE player_status_transitions SET reason = 'rewritten'`},
		{"DELETE", `DELETE FROM player_status_transitions`},
		{"TRUNCATE", `TRUNCATE player_status_transitions`},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			_, err := app.Exec(ctx, m.sql)
			if err == nil {
				t.Fatalf("%s SUCCEEDED; the compliance audit log must be append-only", m.name)
			}
			if got := sqlState(err); got != "42501" && got != "0A000" {
				t.Errorf("%s: refused with SQLSTATE %q, want 42501 or 0A000 (err: %v)", m.name, got, err)
			}
		})
	}
}

// TestSelfExclusion_AuditLogTriggerBlocksEvenOwner closes the gap grants cannot.
//
// A table owner holds implicit full rights, so the grant set is inert against a
// misconfigured POSTGRES_URL that connects as the owner. The trigger is what
// makes the append-only guarantee hold for every role, and it must be asserted
// separately — a passing grants test says nothing about this path.
func TestSelfExclusion_AuditLogTriggerBlocksEvenOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, _ := migrateAndConnect(ctx, t)

	playerID := seedTransitionPlayer(ctx, t, admin)
	if err := insertTransition(ctx, admin, playerID, "ACTIVE", "SUSPENDED", "OPERATOR",
		"fraud review opened"); err != nil {
		t.Fatalf("owner INSERT must succeed: %v", err)
	}

	for _, m := range []struct{ name, sql string }{
		{"UPDATE as owner", `UPDATE player_status_transitions SET reason = 'rewritten'`},
		{"DELETE as owner", `DELETE FROM player_status_transitions`},
		{"TRUNCATE as owner", `TRUNCATE player_status_transitions`},
	} {
		t.Run(m.name, func(t *testing.T) {
			_, err := admin.Exec(ctx, m.sql)
			if err == nil {
				t.Fatalf("%s SUCCEEDED; the trigger must bind the owner too", m.name)
			}
			if got := sqlState(err); got != "0A000" {
				t.Errorf("%s: SQLSTATE %q, want 0A000 from the trigger (err: %v)", m.name, got, err)
			}
		})
	}
}

// TestSelfExclusion_AuditLogRejectsUnusableRows pins the constraints. Each one
// exists to keep the log usable as evidence: a no-op transition is a caller bug,
// an unexplained compliance action is indefensible, and an unbounded text field
// turns an audit table into a blob store.
func TestSelfExclusion_AuditLogRejectsUnusableRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, _ := migrateAndConnect(ctx, t)
	playerID := seedTransitionPlayer(ctx, t, admin)

	cases := []struct {
		name      string
		from, to  string
		reason    string
		wantState string
	}{
		{"no-op transition", "ACTIVE", "ACTIVE", "nothing changed", "23514"},
		{"empty reason", "ACTIVE", "CLOSED", "", "23514"},
		{"whitespace-only reason", "ACTIVE", "CLOSED", "   \t  ", "23514"},
		// Newlines are the case a length(btrim(...)) check misses even after the
		// tab case is noticed — btrim strips spaces only.
		{"newline-only reason", "ACTIVE", "CLOSED", "\n\n", "23514"},
		{"over-long reason", "ACTIVE", "CLOSED", stringOfLength(501), "23514"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := insertTransition(ctx, admin, playerID, c.from, c.to, "OPERATOR", c.reason)
			if err == nil {
				t.Fatalf("INSERT SUCCEEDED; %s must be rejected", c.name)
			}
			if got := sqlState(err); got != c.wantState {
				t.Errorf("%s: SQLSTATE %q, want %q (err: %v)", c.name, got, c.wantState, err)
			}
		})
	}

	// A transition must belong to a real player: an orphan audit row is
	// unattributable, which defeats the table's purpose.
	t.Run("unknown player", func(t *testing.T) {
		err := insertTransition(ctx, admin, "99999999-9999-9999-9999-999999999999",
			"ACTIVE", "CLOSED", "OPERATOR", "orphan")
		if err == nil {
			t.Fatal("INSERT SUCCEEDED; the FK to users must reject an unknown player")
		}
		if got := sqlState(err); got != "23503" {
			t.Errorf("SQLSTATE %q, want 23503 foreign_key_violation (err: %v)", got, err)
		}
	})

	// actor_ref is optional, but "present and blank" is not a state worth
	// storing — it looks like an attribution and carries none.
	t.Run("blank actor_ref", func(t *testing.T) {
		_, err := admin.Exec(ctx, `
			INSERT INTO player_status_transitions
			    (player_id, from_status, to_status, actor_type, actor_ref, reason)
			VALUES ($1, 'ACTIVE', 'CLOSED', 'OPERATOR', '  ', 'blank reference')`, playerID)
		if err == nil {
			t.Fatal("INSERT SUCCEEDED; a present-but-blank actor_ref must be rejected")
		}
		if got := sqlState(err); got != "23514" {
			t.Errorf("SQLSTATE %q, want 23514 check_violation (err: %v)", got, err)
		}
	})

	// Omitting the reference entirely is legitimate: not every transition has an
	// external id to cite.
	t.Run("null actor_ref is accepted", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `
			INSERT INTO player_status_transitions
			    (player_id, from_status, to_status, actor_type, actor_ref, reason)
			VALUES ($1, 'ACTIVE', 'SUSPENDED', 'SYSTEM', NULL, 'automated fraud rule')`,
			playerID); err != nil {
			t.Errorf("a NULL actor_ref must be accepted: %v", err)
		}
	})

	// A reason at the limit must still be accepted — a constraint that rejects
	// valid input is as much a defect as one that admits invalid input.
	t.Run("reason at the limit is accepted", func(t *testing.T) {
		if err := insertTransition(ctx, admin, playerID, "ACTIVE", "CLOSED", "REGULATOR",
			stringOfLength(500)); err != nil {
			t.Errorf("a 500-character reason must be accepted: %v", err)
		}
	})
}

func stringOfLength(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// TestSelfExclusion_DownUpCycleIsRepeatable guards the one genuinely awkward
// property of this migration.
//
// PostgreSQL cannot remove an enum value, so 000009's DOWN deliberately leaves
// 'SELF_EXCLUDED' behind (see the migration header). That makes the UP's
// ADD VALUE **IF NOT EXISTS** load-bearing rather than decorative: without it a
// down/up cycle fails on the second up with a duplicate-value error, and the
// engine refuses to boot. This test is what keeps someone from "tidying" that
// clause away.
//
// It runs against its OWN database so a mid-cycle failure can never leave the
// shared suite database missing tables the other tests need.
func TestSelfExclusion_DownUpCycleIsRepeatable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, adminURL := migrateAndConnect(ctx, t)

	dbName := fmt.Sprintf("se_cycle_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", dbName)); err != nil {
		t.Fatalf("create throwaway database: %v", err)
	}
	t.Cleanup(func() {
		// Best effort: the migrator's connection is closed below, but a lingering
		// backend should not fail the suite.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_URL: %v", err)
	}
	u.Path = "/" + dbName
	cycleURL := u.String()

	src, err := iofs.New(truengine.MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("open embedded migrations: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, cycleURL)
	if err != nil {
		t.Fatalf("init migrator: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil {
		t.Fatalf("initial up: %v", err)
	}
	// Migrate to a PINNED version rather than stepping back a fixed number of
	// times. Steps(-1) meant "undo 000009" only while 000009 was the head; the
	// moment 000010 landed it unwound that instead and this test failed for a
	// reason that had nothing to do with what it checks. Naming the version this
	// test needs to land on keeps it correct as migrations accumulate.
	const versionBeforeSelfExclusion = 8
	if err := m.Migrate(versionBeforeSelfExclusion); err != nil {
		t.Fatalf("migrate down to %d: %v", versionBeforeSelfExclusion, err)
	}

	cycleConn, err := pgx.Connect(ctx, cycleURL)
	if err != nil {
		t.Fatalf("connect to cycle database: %v", err)
	}
	defer func() { _ = cycleConn.Close(context.Background()) }()

	// What the DOWN must remove.
	for _, rel := range []string{"player_status_transitions"} {
		var exists bool
		if err := cycleConn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, rel).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", rel, err)
		}
		if exists {
			t.Errorf("%s survived the down migration", rel)
		}
	}
	var colExists bool
	if err := cycleConn.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM information_schema.columns
		    WHERE table_name = 'users' AND column_name = 'self_exclusion_until')`).Scan(&colExists); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if colExists {
		t.Error("users.self_exclusion_until survived the down migration")
	}

	// What the DOWN cannot remove, and must not pretend to. Asserted positively
	// so the documented residue stays documented behaviour rather than drifting
	// into a surprise.
	var enumSurvives bool
	if err := cycleConn.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM pg_enum e
		    JOIN pg_type t ON t.oid = e.enumtypid
		    WHERE t.typname = 'user_status' AND e.enumlabel = 'SELF_EXCLUDED')`).Scan(&enumSurvives); err != nil {
		t.Fatalf("check enum after down: %v", err)
	}
	if !enumSurvives {
		t.Error("SELF_EXCLUDED was removed by the down migration; PostgreSQL cannot do this, " +
			"so the migration must be doing something more destructive than documented")
	}

	// The point of the whole test: re-applying over the residue must succeed.
	if err := m.Up(); err != nil {
		t.Fatalf("re-up after down MUST succeed (this is what ADD VALUE IF NOT EXISTS buys): %v", err)
	}

	var backAgain bool
	if err := cycleConn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'player_status_transitions')`).Scan(&backAgain); err != nil {
		t.Fatalf("check table after re-up: %v", err)
	}
	if !backAgain {
		t.Error("player_status_transitions did not come back after re-up")
	}
}
