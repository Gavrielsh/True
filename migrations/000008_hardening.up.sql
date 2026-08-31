-- 000008_hardening.up.sql
--
-- Production hardening that backs two of the application-layer changes with
-- DB-level guarantees. All objects here are OPERATIONAL/DERIVED (no financial
-- rows), so unlike the ledger migrations the DOWN migration cleanly reverses
-- them.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 1. STRICT APPEND-ONLY on BOTH ledger tables.
-- ─────────────────────────────────────────────────────────────────────────────
-- The engine no longer mutates ledger_transactions: the old rollback path did
-- `UPDATE ledger_transactions SET status = 'ROLLED_BACK'`, which rewrote
-- financial history. A reversal is now posted as a NEW ROLLBACK row that
-- references the original (strict double-entry audit trail; see
-- internal/repository processRollbackTx).
--
-- CORRECTION (Milestone 0.3). This section previously protected only
-- ledger_transactions, on the stated grounds that "ledger_entries is already
-- append-only via role grants (000004)". That was false: 000004 carried those
-- grants only in a comment and explicitly deferred them, so nothing was ever
-- granted. The effect was the exact inverse of the documented design — the
-- transaction HEADER was protected while the money LINES accepted UPDATE,
-- DELETE and TRUNCATE. A settled ledger_entries row could be silently rewritten.
-- The trigger is therefore applied to BOTH tables here, and the real grant set
-- now lives in 000005.
--
-- Triggers and grants are complementary, not redundant:
--   * GRANTS (000005) stop the application role, but are INERT against a
--     connection that owns the tables — an owner holds implicit full rights.
--   * TRIGGERS (here) fire regardless of role, including for the owner and for
--     a superuser, so a misconfigured POSTGRES_URL cannot silently reopen the
--     hole. cmd/engine additionally refuses to boot as an owner/superuser.
--
-- Cost is nil on the write path: the row trigger fires ONLY on UPDATE/DELETE,
-- never on INSERT. (000004's original claim that a trigger was too expensive at
-- 10k+ TPS was simply mistaken.) On a partitioned table a row-level trigger
-- cascades to every existing and future partition automatically.
--
-- A statement-level BEFORE TRUNCATE trigger is added alongside, because row
-- triggers do not see TRUNCATE. Verified behaviour: it blocks TRUNCATE of the
-- PARENT even for the owner. It does NOT block a TRUNCATE aimed directly at an
-- individual partition — that case is covered by the grant set, since
-- engine_writer holds no privilege at all on any partition.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 2. BATCHED dedup pruning PROCEDURE (pg_cron alternative to the Go worker).
-- ─────────────────────────────────────────────────────────────────────────────
-- prune_ledger_transaction_dedup() (000005) deletes the whole expired backlog in
-- one statement: one big lock, heavy B-Tree bloat on a large backlog. This
-- PROCEDURE deletes in bounded batches with a COMMIT between each, keeping locks
-- short and letting autovacuum keep the global index tight. It is the SQL-side
-- equivalent of the Go DedupPruner worker (internal/worker) — deploy EXACTLY ONE
-- of the two (the Go worker by default; this PROCEDURE if you prefer pg_cron).

BEGIN;

-- 1. Append-only guard --------------------------------------------------------

-- One guard for both tables. TG_TABLE_NAME names the offending table in the
-- error, so a single function serves every ledger relation without duplicating
-- the logic per table.
CREATE OR REPLACE FUNCTION ledger_block_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only: % is not permitted '
        '(post a compensating ROLLBACK transaction instead)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

COMMENT ON FUNCTION ledger_block_mutation() IS
    'Enforces the append-only invariant on the ledger tables: any UPDATE, DELETE '
    'or TRUNCATE raises SQLSTATE 0A000, for EVERY role including the table owner '
    'and superusers. Reversals are posted as new ROLLBACK rows referencing the '
    'original (internal/repository processRollbackTx). Never fires on INSERT.';

-- Retire the ledger_transactions-only guard this migration used to install.
DROP TRIGGER IF EXISTS trg_ledger_transactions_append_only ON ledger_transactions;
DROP FUNCTION IF EXISTS ledger_transactions_block_mutation();

-- ledger_transactions (the header)
CREATE TRIGGER trg_ledger_transactions_append_only
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW
    EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS trg_ledger_transactions_no_truncate ON ledger_transactions;
CREATE TRIGGER trg_ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT
    EXECUTE FUNCTION ledger_block_mutation();

-- ledger_entries (the money lines) — the gap this migration previously left open.
DROP TRIGGER IF EXISTS trg_ledger_entries_append_only ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW
    EXECUTE FUNCTION ledger_block_mutation();

DROP TRIGGER IF EXISTS trg_ledger_entries_no_truncate ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT
    EXECUTE FUNCTION ledger_block_mutation();

-- 2. Batched, low-lock dedup pruning -----------------------------------------

CREATE OR REPLACE PROCEDURE prune_ledger_transaction_dedup_batched(
    p_retention   interval DEFAULT interval '30 days',
    p_batch_size  integer  DEFAULT 5000,
    p_max_batches integer  DEFAULT 1000000
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_deleted bigint;
    v_total   bigint := 0;
    v_batches integer := 0;
BEGIN
    IF p_batch_size <= 0 THEN
        RAISE EXCEPTION 'p_batch_size must be > 0, got %', p_batch_size;
    END IF;

    LOOP
        -- The ctid sub-select picks the oldest expired rows via
        -- ledger_transaction_dedup_created_at_idx (000005): bounded, index-
        -- ordered work per statement rather than a full-table monster DELETE.
        DELETE FROM ledger_transaction_dedup
        WHERE ctid IN (
            SELECT ctid
            FROM ledger_transaction_dedup
            WHERE created_at < now() - p_retention
            ORDER BY created_at
            LIMIT p_batch_size
        );
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        v_total   := v_total + v_deleted;
        v_batches := v_batches + 1;

        -- COMMIT between batches releases row locks and lets autovacuum keep the
        -- B-Tree tight, instead of one long transaction holding locks for minutes.
        COMMIT;

        EXIT WHEN v_deleted < p_batch_size;   -- backlog drained
        EXIT WHEN v_batches >= p_max_batches;  -- safety stop
    END LOOP;

    RAISE NOTICE 'prune_ledger_transaction_dedup_batched: deleted % rows in % batches',
        v_total, v_batches;
END;
$$;

COMMENT ON PROCEDURE prune_ledger_transaction_dedup_batched(interval, integer, integer) IS
    'Batched retention prune for ledger_transaction_dedup: deletes expired rows in '
    'p_batch_size chunks with a COMMIT between batches (short locks, minimal B-Tree '
    'bloat). Schedule daily via pg_cron, OR run the Go DedupPruner worker '
    '(internal/worker) — not both.';

COMMIT;
