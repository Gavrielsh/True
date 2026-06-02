-- 000005_partitioning.up.sql
--
-- DAILY RANGE PARTITIONING for the ledger (architecture §3 "You MUST implement
-- PostgreSQL Table Partitioning by date", scaled for 10k–50k TPS).
--
-- 000004 created `ledger_entries` partitioned MONTHLY and `ledger_transactions`
-- NOT partitioned at all. At 50k TPS a single day is ~4.3B ledger rows, so a
-- monthly partition would hold ~130B rows — far past the point where partition
-- pruning, autovacuum, and index maintenance stay healthy. This migration moves
-- BOTH tables to DAILY partitions and folds the partition key (created_at) into
-- every primary key and unique constraint, so there is no global (cross-
-- partition) index to serialise inserts on.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- THE IDEMPOTENCY PROBLEM (and why ledger_transaction_dedup exists)
-- ─────────────────────────────────────────────────────────────────────────────
-- PostgreSQL requires every UNIQUE constraint on a partitioned table to include
-- the partition key. So once ledger_transactions is RANGE-partitioned by
-- created_at, its idempotency anchor can only be
--     UNIQUE (operator_code, operator_transaction_id, created_at)
-- which is PARTITION-LOCAL: a webhook replayed seconds/days later carries a new
-- created_at, lands in a different partition, and does NOT raise 23505 — so the
-- engine's Ghost-Spin recovery (architecture §6.A) would silently double-credit
-- the player. That is unacceptable for a money system.
--
-- Resolution: a SEPARATE, NON-partitioned `ledger_transaction_dedup` table whose
-- PRIMARY KEY (operator_code, operator_transaction_id) is the true GLOBAL,
-- cross-time idempotency anchor. The engine writes one dedup row inside the same
-- transaction as the ledger_transactions row; a duplicate operator transaction —
-- no matter how much later it arrives — collides there and raises 23505, exactly
-- as before. The composite unique on ledger_transactions is kept as partition-
-- local defence-in-depth. (Global uniqueness inherently needs a global index;
-- we keep that index tiny by pruning the dedup table to the operator retry
-- horizon — see prune_ledger_transaction_dedup below.)
--
-- ─────────────────────────────────────────────────────────────────────────────
-- DROPPED FOREIGN KEYS (intentional, documented)
-- ─────────────────────────────────────────────────────────────────────────────
-- A FK that references a RANGE-partitioned table must reference its full
-- (id, created_at) key, which would force a partition key onto every child and
-- add a per-row cross-partition check on the 50k-TPS hot path. We therefore drop:
--   * ledger_entries.ledger_transaction_id → ledger_transactions(id)
--   * ledger_transactions.reference_transaction_id → ledger_transactions(id) (self)
-- Referential integrity is preserved by the application: the entries and their
-- parent ledger_transactions row are INSERTed in the SAME pgx transaction, so an
-- entry can never reference a missing parent; the rollback path locks the
-- referenced row (SELECT ... FOR UPDATE) before use. player_id → users(id) is
-- kept (users is not partitioned; a FK from a partitioned child to a regular
-- table is fully supported and cheap).
--
-- ─────────────────────────────────────────────────────────────────────────────
-- SAFETY: this migration RE-CREATES the ledger tables and therefore REFUSES to
-- run if they hold any rows. On a populated production database you must instead
-- use the online runbook (create the new partitioned tables under temp names,
-- backfill in batches, then swap in a transaction) so financial history is never
-- dropped — consistent with 000004_ledger.down.sql's refusal to erase the
-- ledger. On a fresh/CI database (the expected case for this migration) the
-- tables are empty and it runs clean.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 0. Guard: refuse to destroy financial history.
-- ─────────────────────────────────────────────────────────────────────────────
DO $guard$
DECLARE
    n_tx      bigint;
    n_entries bigint;
BEGIN
    EXECUTE 'SELECT count(*) FROM ledger_transactions' INTO n_tx;
    EXECUTE 'SELECT count(*) FROM ledger_entries'      INTO n_entries;
    IF n_tx <> 0 OR n_entries <> 0 THEN
        RAISE EXCEPTION
            'refusing to re-partition a non-empty ledger (ledger_transactions=%, ledger_entries=%). '
            'Run the online partition-migration runbook (create new partitioned tables, '
            'backfill in batches, swap in one transaction) to preserve financial history.',
            n_tx, n_entries
            USING ERRCODE = '0A000';  -- feature_not_supported
    END IF;
END
$guard$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Drop the old (monthly) objects. CASCADE removes the monthly partitions and
--    the cross-table FK from ledger_entries. Order: entries first (it references
--    transactions), then transactions, then the obsolete monthly helper.
-- ─────────────────────────────────────────────────────────────────────────────
DROP TABLE IF EXISTS ledger_entries CASCADE;
DROP TABLE IF EXISTS ledger_transactions CASCADE;
DROP FUNCTION IF EXISTS create_ledger_entries_partition(date);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. ledger_transactions — partitioned daily by created_at.
--    PK and UNIQUE both include created_at (the partition key) — no global index.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE ledger_transactions (
    id                          UUID                NOT NULL DEFAULT gen_random_uuid(),
    operator_code               TEXT                NOT NULL,
    operator_transaction_id     TEXT                NOT NULL,
    player_id                   UUID                NOT NULL
                                                    REFERENCES users(id) ON DELETE RESTRICT,
    transaction_type            transaction_type    NOT NULL,
    status                      transaction_status  NOT NULL DEFAULT 'PENDING',
    game_id                     TEXT,
    round_id                    TEXT,
    -- FK to ledger_transactions(id) intentionally dropped (see header). Still
    -- used by the rollback/win flows, validated in-app under FOR UPDATE.
    reference_transaction_id    UUID,
    request_metadata            JSONB               NOT NULL DEFAULT '{}'::jsonb,
    created_at                  TIMESTAMPTZ         NOT NULL DEFAULT now(),
    completed_at                TIMESTAMPTZ,

    -- Partition key MUST be part of the PK in declarative partitioning.
    PRIMARY KEY (id, created_at),
    -- Partition-LOCAL idempotency / defence-in-depth. The GLOBAL anchor lives in
    -- ledger_transaction_dedup (below). Leading (operator_code,
    -- operator_transaction_id) also serves the Ghost-Spin recovery lookup.
    CONSTRAINT ledger_tx_operator_unique
        UNIQUE (operator_code, operator_transaction_id, created_at),
    CONSTRAINT ledger_tx_metadata_size_cap
        CHECK (octet_length(request_metadata::text) <= 512)
) PARTITION BY RANGE (created_at);

-- Indexes on the partitioned root propagate to every existing & future partition.
CREATE INDEX ledger_tx_player_id_idx  ON ledger_transactions (player_id, created_at DESC);
CREATE INDEX ledger_tx_round_id_idx   ON ledger_transactions (round_id) WHERE round_id IS NOT NULL;
CREATE INDEX ledger_tx_reference_idx  ON ledger_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL;
CREATE INDEX ledger_tx_status_idx     ON ledger_transactions (status)
    WHERE status IN ('PENDING', 'FAILED');

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. ledger_entries — partitioned daily by created_at.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE ledger_entries (
    id                      UUID             NOT NULL DEFAULT gen_random_uuid(),
    -- FK to ledger_transactions(id) intentionally dropped (see header).
    ledger_transaction_id   UUID             NOT NULL,
    player_id               UUID,            -- NULL for HOUSE_* account_type rows
    account_type            account_type     NOT NULL,
    currency                currency_type    NOT NULL,
    direction               entry_direction  NOT NULL,
    amount                  NUMERIC(18,4)    NOT NULL,
    balance_after           NUMERIC(18,4),   -- only meaningful for PLAYER_WALLET rows
    created_at              TIMESTAMPTZ      NOT NULL DEFAULT now(),

    PRIMARY KEY (id, created_at),

    CONSTRAINT ledger_entries_amount_positive       CHECK (amount > 0.0000),
    CONSTRAINT ledger_entries_balance_after_nonneg   CHECK (balance_after IS NULL OR balance_after >= 0.0000),
    CONSTRAINT ledger_entries_account_shape CHECK (
        (account_type = 'PLAYER_WALLET'
            AND player_id IS NOT NULL
            AND balance_after IS NOT NULL)
        OR
        (account_type <> 'PLAYER_WALLET'
            AND player_id IS NULL
            AND balance_after IS NULL)
    )
) PARTITION BY RANGE (created_at);

COMMENT ON TABLE ledger_entries IS
    'Append-only double-entry ledger, RANGE-partitioned daily by created_at. '
    'UPDATE/DELETE blocked at the role-grant layer (engine_writer holds '
    'INSERT+SELECT only). Corrections are posted as offsetting ROLLBACK entries.';

CREATE INDEX ledger_entries_tx_id_idx
    ON ledger_entries (ledger_transaction_id);
CREATE INDEX ledger_entries_player_currency_idx
    ON ledger_entries (player_id, currency, created_at DESC)
    WHERE player_id IS NOT NULL;
-- Drives the house-pool aggregation the GGR worker runs (account_type + window).
CREATE INDEX ledger_entries_account_currency_idx
    ON ledger_entries (account_type, currency, created_at DESC)
    WHERE player_id IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. ledger_transaction_dedup — NON-partitioned GLOBAL idempotency anchor.
--    One row per operator transaction, ever. Its PK is the cross-time uniqueness
--    that the partitioned ledger_transactions can no longer provide. Written in
--    the same tx as the ledger_transactions row; a duplicate raises 23505 →
--    Ghost-Spin recovery.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE ledger_transaction_dedup (
    operator_code           TEXT        NOT NULL,
    operator_transaction_id TEXT        NOT NULL,
    ledger_transaction_id   UUID        NOT NULL,  -- the committed tx this key belongs to
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_transaction_dedup_pkey
        PRIMARY KEY (operator_code, operator_transaction_id)
);
-- Supports retention pruning (keep the global index bounded to the retry horizon).
CREATE INDEX ledger_transaction_dedup_created_at_idx
    ON ledger_transaction_dedup (created_at);

COMMENT ON TABLE ledger_transaction_dedup IS
    'Global, non-partitioned idempotency anchor for (operator_code, '
    'operator_transaction_id). Required because the partitioned ledger_transactions '
    'can only enforce partition-local uniqueness. Prune to the operator retry '
    'horizon via prune_ledger_transaction_dedup() to bound the global index.';

-- Bounded-retention pruning: a duplicate can only arrive within the operator's
-- retry window (hours–days). Keeping ~30 days is far beyond any real retry policy
-- and keeps this global index small even at 50k TPS. Schedule daily (pg_cron).
CREATE OR REPLACE FUNCTION prune_ledger_transaction_dedup(p_retention interval DEFAULT interval '30 days')
RETURNS bigint
LANGUAGE plpgsql
AS $$
DECLARE
    v_deleted bigint;
BEGIN
    DELETE FROM ledger_transaction_dedup WHERE created_at < now() - p_retention;
    GET DIAGNOSTICS v_deleted = ROW_COUNT;
    RETURN v_deleted;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Daily partition management (native Postgres 15+, no extension required).
-- ─────────────────────────────────────────────────────────────────────────────

-- create_daily_partition: idempotently create the [day, day+1) partition of a
-- RANGE(created_at)-partitioned parent. Child name: <parent>_YYYYMMDD.
CREATE OR REPLACE FUNCTION create_daily_partition(p_parent text, p_day date)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    v_child text := format('%s_%s', p_parent, to_char(p_day, 'YYYYMMDD'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
        v_child, p_parent, p_day::timestamptz, (p_day + 1)::timestamptz
    );
END;
$$;

-- ensure_ledger_partitions: create today's partition plus the next p_days_ahead
-- for BOTH partitioned ledger tables. Idempotent (CREATE ... IF NOT EXISTS), so a
-- daily cron can call ensure_ledger_partitions(7) to keep a 7-day runway ahead of
-- ingestion. Run this from pg_cron / an external scheduler once per day.
CREATE OR REPLACE FUNCTION ensure_ledger_partitions(p_days_ahead int DEFAULT 7)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    d int;
    target date;
BEGIN
    IF p_days_ahead < 0 THEN
        RAISE EXCEPTION 'p_days_ahead must be >= 0, got %', p_days_ahead;
    END IF;
    FOR d IN 0..p_days_ahead LOOP
        target := current_date + d;
        PERFORM create_daily_partition('ledger_entries', target);
        PERFORM create_daily_partition('ledger_transactions', target);
    END LOOP;
END;
$$;

-- DEFAULT partitions: a safety net so an INSERT is NEVER rejected for a missing
-- partition (a dropped financial write is worse than a misfiled one). In steady
-- state these stay EMPTY because ensure_ledger_partitions keeps concrete daily
-- partitions ahead of "now"; monitor row counts and alert if a default is ever
-- non-empty. (A non-empty default makes attaching a new overlapping partition
-- scan the default, so it must be drained promptly.)
CREATE TABLE ledger_transactions_default PARTITION OF ledger_transactions DEFAULT;
CREATE TABLE ledger_entries_default       PARTITION OF ledger_entries       DEFAULT;

-- Bootstrap: today + the next 7 days of daily partitions for both tables.
SELECT ensure_ledger_partitions(7);

COMMIT;
