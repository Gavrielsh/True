-- 000008_hardening.up.sql
--
-- Production hardening that backs two of the application-layer changes with
-- DB-level guarantees. All objects here are OPERATIONAL/DERIVED (no financial
-- rows), so unlike the ledger migrations the DOWN migration cleanly reverses
-- them.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 1. STRICT APPEND-ONLY ledger_transactions.
-- ─────────────────────────────────────────────────────────────────────────────
-- The engine no longer mutates ledger_transactions: the old rollback path did
-- `UPDATE ledger_transactions SET status = 'ROLLED_BACK'`, which rewrote
-- financial history. A reversal is now posted as a NEW ROLLBACK row that
-- references the original (strict double-entry audit trail; see
-- internal/repository processRollbackTx). ledger_entries is already append-only
-- via role grants (000004); this extends the same guarantee to
-- ledger_transactions with a lightweight trigger.
--
-- The trigger fires ONLY on UPDATE/DELETE — never on INSERT — so the 50k-TPS
-- write path pays nothing. On a partitioned table (PG 15) a row-level trigger
-- cascades to every existing and future partition automatically.
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

CREATE OR REPLACE FUNCTION ledger_transactions_block_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        'ledger_transactions is append-only: % is not permitted '
        '(post a compensating ROLLBACK transaction instead)', TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

DROP TRIGGER IF EXISTS trg_ledger_transactions_append_only ON ledger_transactions;
CREATE TRIGGER trg_ledger_transactions_append_only
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW
    EXECUTE FUNCTION ledger_transactions_block_mutation();

COMMENT ON FUNCTION ledger_transactions_block_mutation() IS
    'Enforces the append-only invariant on ledger_transactions: any UPDATE/DELETE '
    'raises SQLSTATE 0A000. Reversals are posted as new ROLLBACK rows referencing '
    'the original (internal/repository processRollbackTx). Never fires on INSERT.';

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
