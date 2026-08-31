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
-- SECURITY DEFINER so the application role can pre-create partitions WITHOUT
-- holding CREATE on the schema. Granting it CREATE would be self-defeating: the
-- role would then OWN every partition it creates, and an owner has full rights
-- on its own tables — which would hand back the UPDATE/DELETE on ledger rows
-- that the grants below exist to remove. Running as the definer keeps every
-- partition owned by the schema owner, so the append-only invariant holds on
-- partitions the application itself caused to exist.
--
-- search_path is pinned: a SECURITY DEFINER function without a fixed
-- search_path can be hijacked by a caller-controlled schema shadowing the
-- objects it references.
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_child text := format('%s_%s', p_parent, to_char(p_day, 'YYYYMMDD'));
BEGIN
    -- A SECURITY DEFINER function runs with the definer's rights, so it must
    -- validate its own inputs rather than trusting the caller. Without this an
    -- engine_writer could create a partition of ANY partitioned relation in the
    -- schema. The allowlist pins it to the two ledger parents the worker
    -- legitimately maintains. (%I quoting already prevents SQL injection; this
    -- is about limiting the definer's authority, not string safety.)
    IF p_parent NOT IN ('ledger_entries', 'ledger_transactions') THEN
        RAISE EXCEPTION 'create_daily_partition: % is not a managed ledger parent', p_parent
            USING ERRCODE = '42501';  -- insufficient_privilege
    END IF;

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
-- SECURITY DEFINER for the same reason as create_daily_partition above; this is
-- the entry point internal/worker/partitioner.go calls on its daily cycle.
SECURITY DEFINER
SET search_path = public, pg_temp
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

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. THE DOUBLE-ENTRY INVARIANT, ENFORCED BY PRIVILEGE (Milestone 0.3)
-- ─────────────────────────────────────────────────────────────────────────────
-- 000004 documented this grant set in a COMMENT and deferred it to "operational"
-- provisioning that was never carried out. The result was that ledger_entries
-- accepted UPDATE, DELETE and TRUNCATE from the application role: a settled
-- money line could be silently rewritten. The grants are therefore declared HERE,
-- in versioned migration SQL, where they are applied and auditable.
--
-- They live in 000005 rather than 000004 for a concrete reason: section 1 above
-- DROPs both ledger tables CASCADE and recreates them, which discards every
-- privilege granted on the old objects. A grant written into 000004 would be
-- silently destroyed by this migration.
--
-- engine_writer is a NOLOGIN GROUP role. The privilege SET is fixed and
-- versioned here; only the LOGIN user is environment-specific, and ops attaches
-- it with:
--
--     CREATE ROLE <env_login_user> LOGIN PASSWORD '<secret>';
--     GRANT engine_writer TO <env_login_user>;
--
-- The engine MUST connect as such a member and MUST NOT connect as the schema
-- owner or a superuser: owners hold implicit full rights on their own tables, so
-- the grants below would be inert. cmd/engine asserts this at boot and refuses
-- to start otherwise.

DO $role$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'engine_writer') THEN
        CREATE ROLE engine_writer NOLOGIN;
    END IF;
END
$role$;

GRANT USAGE ON SCHEMA public TO engine_writer;

-- ── Financial ledger: APPEND-ONLY. INSERT + SELECT, nothing else, ever. ──────
-- No UPDATE: a settled entry is never rewritten. No DELETE: history is never
-- erased. No TRUNCATE: the table is never emptied. Corrections are posted as
-- offsetting ROLLBACK entries.
--
-- Grants are deliberately NOT issued on the individual partitions. PostgreSQL
-- checks privileges on the PARENT for DML routed through it, so INSERT and
-- SELECT work unchanged, while a direct UPDATE against a partition by name is
-- refused for want of any privilege on that partition. Both halves are asserted
-- by TestLedgerGrants_* in cmd/engine.
GRANT INSERT, SELECT ON ledger_transactions TO engine_writer;
GRANT INSERT, SELECT ON ledger_entries      TO engine_writer;

-- ── Operational tables: the privileges the code actually exercises. ──────────
-- Each line below is justified by a call site. 000004's documented alternative
-- (GRANT INSERT, SELECT ON ALL TABLES IN SCHEMA public) would have broken every
-- one of them: wallet balances, GGR aggregation, and dedup pruning all require
-- privileges beyond INSERT+SELECT.

-- ledger_transaction_dedup is the global idempotency anchor, NOT financial
-- history. It is retention-pruned, so DELETE is required
-- (internal/worker/dedup_prune.go).
GRANT SELECT, INSERT, DELETE ON ledger_transaction_dedup TO engine_writer;

-- wallets: balances mutate in place under SELECT ... FOR UPDATE
-- (internal/repository engine.go, queries.go).
GRANT SELECT, INSERT, UPDATE ON wallets TO engine_writer;

-- users: created by the casino wrapper; never updated by the engine.
GRANT SELECT, INSERT ON users TO engine_writer;

-- NOTE: daily_ggr and ggr_aggregator_state are created later, by 000007, so
-- their grants cannot be issued here — they are declared at the end of that
-- migration instead. The complete, final grant set across all migrations is
-- asserted as an exact match by TestLedgerGrants_ExactPrivilegeSet.

-- ── Functions ────────────────────────────────────────────────────────────────
-- Partition pre-creation is reachable ONLY through the SECURITY DEFINER entry
-- point. EXECUTE is revoked from PUBLIC first: functions are granted to PUBLIC
-- by default, which would let any role invoke a definer-rights function.
REVOKE EXECUTE ON FUNCTION create_daily_partition(text, date)  FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION ensure_ledger_partitions(int)       FROM PUBLIC;
-- internal/worker/partitioner.go calls create_daily_partition per parent/day so
-- it can log created-vs-skipped per partition; ensure_ledger_partitions is the
-- pg_cron entry point. Both are safe to expose: the definer function refuses any
-- parent outside the ledger allowlist above.
GRANT  EXECUTE ON FUNCTION create_daily_partition(text, date)  TO engine_writer;
GRANT  EXECUTE ON FUNCTION ensure_ledger_partitions(int)       TO engine_writer;

-- TRUNCATE is granted on NOTHING, to any role, anywhere in this schema.

COMMIT;
