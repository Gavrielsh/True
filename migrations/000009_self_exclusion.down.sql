-- 000009_self_exclusion.down.sql
--
-- Reverses 000009 as far as PostgreSQL permits.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- ⚠️  THIS DOWN MIGRATION IS DELIBERATELY INCOMPLETE
-- ─────────────────────────────────────────────────────────────────────────────
-- PostgreSQL has no `ALTER TYPE ... DROP VALUE`. Once 'SELF_EXCLUDED' is added
-- to user_status it cannot be removed, short of recreating the type — which
-- means dropping every dependent column and default across users and
-- player_status_transitions, rewriting the users table, and destroying any row
-- that currently holds the value. That is a data-destroying operation dressed up
-- as a rollback, and it is not performed here.
--
-- The residue is inert and SAFE, which is what makes leaving it the right call:
--
--   * An unused enum value costs nothing. Nothing reads it, nothing defaults to
--     it, no constraint references it.
--   * Any player already carrying the status stays blocked after the rollback.
--     requirePlayerActive tests `status != 'ACTIVE'`, so pre-000009 code refuses
--     every money path for that player exactly as post-000009 code does. The
--     rollback cannot resurrect a self-excluded player into play — the failure
--     mode runs in the safe direction.
--   * The UP migration uses ADD VALUE IF NOT EXISTS, so a later re-apply is a
--     no-op on the enum rather than a duplicate-value error. down/up cycles
--     work.
--
-- What IS lost on rollback: the audit trail itself. player_status_transitions is
-- dropped, and with it the record of who excluded whom and why. Treat a rollback
-- past 000009 as a compliance event — export the table first. In a deployed
-- environment the correct response to a bad 000009 is a forward migration, not
-- a down.

BEGIN;

-- The audit trail. CASCADE is NOT used: an unexpected dependent object should
-- fail this migration loudly rather than be silently destroyed alongside the
-- table. Its indexes and triggers are dropped with it.
DROP TABLE IF EXISTS player_status_transitions;

-- Safe unconditionally: the trigger that referenced it went with the table, and
-- no other object in this schema uses this function (000008's ledger triggers
-- use ledger_block_mutation(), which is deliberately separate and is left
-- untouched here).
DROP FUNCTION IF EXISTS audit_log_block_mutation();

DROP TYPE IF EXISTS status_actor;

-- Dropping the column also drops users_self_exclusion_until_idx, which is
-- defined on it. The explicit DROP INDEX is kept ahead of it so this migration
-- still reverses cleanly against a database where the column was removed by hand
-- but the index somehow survived.
DROP INDEX IF EXISTS users_self_exclusion_until_idx;
ALTER TABLE users DROP COLUMN IF EXISTS self_exclusion_until;

-- Restore 000001's wording, so a database rolled back to 000008 does not carry a
-- comment describing a status its enum no longer documents. (The VALUE remains —
-- see the header.)
COMMENT ON TYPE user_status IS
    'Player lifecycle. KYC_PENDING: registered, awaiting identity verification '
    '(cannot wager SC). ACTIVE: fully verified, may bet/win/redeem. SUSPENDED: '
    'temporarily blocked (fraud review, self-exclusion). CLOSED: permanently '
    'closed; wallet operations rejected.';

-- NOT REVERSED, and not reversible:
--   ALTER TYPE user_status ADD VALUE 'SELF_EXCLUDED'
-- See the header for why this is safe to leave in place.

COMMIT;
