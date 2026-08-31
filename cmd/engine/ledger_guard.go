package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sqlLedgerMutationPrivileges asks Postgres the only question that matters:
// can THIS connection mutate the ledger?
//
// has_table_privilege answers for the effective role, and it reports true for a
// table OWNER and for a SUPERUSER even when no explicit GRANT exists — which is
// exactly the blind spot being closed. Checking the privilege directly is
// therefore stronger than comparing role names: it catches ownership, superuser
// status, a stray GRANT, and inherited membership in one query.
const sqlLedgerMutationPrivileges = `
SELECT
    current_user,
    current_setting('is_superuser') = 'on'                              AS is_superuser,
    has_table_privilege('ledger_entries',      'UPDATE')                AS entries_update,
    has_table_privilege('ledger_entries',      'DELETE')                AS entries_delete,
    has_table_privilege('ledger_entries',      'TRUNCATE')              AS entries_truncate,
    has_table_privilege('ledger_transactions', 'UPDATE')                AS tx_update,
    has_table_privilege('ledger_transactions', 'DELETE')                AS tx_delete,
    has_table_privilege('ledger_transactions', 'TRUNCATE')              AS tx_truncate`

// assertLedgerAppendOnly refuses to boot if the runtime connection can UPDATE,
// DELETE or TRUNCATE either ledger table.
//
// WHY THIS EXISTS (Milestone 0.3): the append-only invariant is enforced by
// granting engine_writer INSERT+SELECT and nothing else (migration 000005).
// Those grants are INERT for a connection that owns the tables or is a
// superuser — an owner holds implicit full rights regardless of what was
// granted. Without this check, pointing POSTGRES_URL at the admin user would
// silently restore the ability to rewrite settled money lines while every
// migration and comment still claimed the ledger was append-only. That gap
// between documented control and actual behaviour is the precise defect this
// milestone exists to remove, so it fails the boot rather than warning.
//
// The trigger installed by 000008 is the other half of the defence and does
// stop even a superuser. This check is still worth failing on: a deployment
// running as owner has no privilege isolation at all, and relying on a single
// control for a money invariant is how the original hole shipped.
func assertLedgerAppendOnly(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	var (
		user                                   string
		superuser                              bool
		eUpd, eDel, eTrunc, tUpd, tDel, tTrunc bool
	)
	if err := pool.QueryRow(ctx, sqlLedgerMutationPrivileges).Scan(
		&user, &superuser, &eUpd, &eDel, &eTrunc, &tUpd, &tDel, &tTrunc,
	); err != nil {
		return fmt.Errorf("ledger privilege check: %w", err)
	}

	var held []string
	for _, p := range []struct {
		name string
		got  bool
	}{
		{"ledger_entries.UPDATE", eUpd},
		{"ledger_entries.DELETE", eDel},
		{"ledger_entries.TRUNCATE", eTrunc},
		{"ledger_transactions.UPDATE", tUpd},
		{"ledger_transactions.DELETE", tDel},
		{"ledger_transactions.TRUNCATE", tTrunc},
	} {
		if p.got {
			held = append(held, p.name)
		}
	}

	if len(held) == 0 {
		logger.Info("ledger append-only invariant verified",
			slog.String("db_user", user))
		return nil
	}

	return fmt.Errorf(
		"REFUSING TO BOOT: database user %q can mutate the ledger (holds: %v; superuser=%t). "+
			"The ledger is append-only and the engine must connect as a member of the "+
			"engine_writer role, which holds INSERT and SELECT only (migration 000005). "+
			"Connecting as the schema owner or a superuser makes those grants inert. "+
			"Fix: CREATE ROLE <app_user> LOGIN PASSWORD '<secret>'; "+
			"GRANT engine_writer TO <app_user>; and point POSTGRES_URL at it, keeping the "+
			"privileged credentials in MIGRATION_POSTGRES_URL for schema changes only. "+
			"For local development only, set LEDGER_ROLE_CHECK=disabled",
		user, held, superuser)
}
