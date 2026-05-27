-- 000004_ledger.up.sql
--
-- Double-Entry Ledger. Single source of truth for all balance changes.
--
-- * `ledger_transactions` is the operator-facing transaction header
--   (one row per BET / WIN / ROLLBACK call). The
--   (operator_code, operator_transaction_id) UNIQUE constraint is the
--   idempotency anchor: it raises Postgres error 23505 on duplicate webhook
--   delivery, which the Go layer catches for Ghost-Spin recovery (§6.A).
--
-- * `ledger_entries` holds the immutable Debit/Credit lines. It is
--   RANGE-partitioned by `created_at` (monthly) because at 10k+ TPS the
--   table will reach hundreds of millions of rows; partition pruning keeps
--   reads bounded and lets us detach old partitions cheaply.
--
-- * The double-entry invariant (Σ DEBIT = Σ CREDIT per transaction) is
--   enforced at the application layer; the DB enforces the supporting
--   shape (positive amount, non-null player on PLAYER_WALLET rows, etc.).

BEGIN;

-- =============================================================================
-- ledger_transactions  (parent / header)
-- =============================================================================
CREATE TABLE ledger_transactions (
    id                          UUID                PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_code               TEXT                NOT NULL,
    operator_transaction_id     TEXT                NOT NULL,
    player_id                   UUID                NOT NULL
                                                    REFERENCES users(id) ON DELETE RESTRICT,
    transaction_type            transaction_type    NOT NULL,
    status                      transaction_status  NOT NULL DEFAULT 'PENDING',
    game_id                     TEXT,
    round_id                    TEXT,
    -- For ROLLBACK rows: points at the BET/WIN being reversed.
    -- For WIN rows tied to a specific BET: points at the originating BET.
    reference_transaction_id    UUID                REFERENCES ledger_transactions(id)
                                                    ON DELETE RESTRICT,
    request_metadata            JSONB               NOT NULL DEFAULT '{}'::jsonb,
    created_at                  TIMESTAMPTZ         NOT NULL DEFAULT now(),
    completed_at                TIMESTAMPTZ,

    CONSTRAINT ledger_tx_operator_unique
        UNIQUE (operator_code, operator_transaction_id)
);

-- Hot lookup paths
CREATE INDEX ledger_tx_player_id_idx     ON ledger_transactions (player_id, created_at DESC);
CREATE INDEX ledger_tx_round_id_idx      ON ledger_transactions (round_id) WHERE round_id IS NOT NULL;
CREATE INDEX ledger_tx_reference_idx     ON ledger_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL;
CREATE INDEX ledger_tx_status_idx        ON ledger_transactions (status)
    WHERE status IN ('PENDING', 'FAILED');

-- =============================================================================
-- ledger_entries  (partitioned, append-only)
-- =============================================================================
CREATE TABLE ledger_entries (
    id                      UUID             NOT NULL DEFAULT gen_random_uuid(),
    ledger_transaction_id   UUID             NOT NULL
                                             REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    player_id               UUID,            -- NULL for HOUSE_* account_type rows
    account_type            account_type     NOT NULL,
    currency                currency_type    NOT NULL,
    direction               entry_direction  NOT NULL,
    amount                  NUMERIC(18,4)    NOT NULL,
    -- balance_after is only meaningful for PLAYER_WALLET rows (per-player audit trail).
    balance_after           NUMERIC(18,4),
    created_at              TIMESTAMPTZ      NOT NULL DEFAULT now(),

    -- Partition key MUST be part of the primary key in declarative partitioning.
    PRIMARY KEY (id, created_at),

    CONSTRAINT ledger_entries_amount_positive   CHECK (amount > 0.0000),
    CONSTRAINT ledger_entries_balance_after_nonneg
        CHECK (balance_after IS NULL OR balance_after >= 0.0000),
    -- A PLAYER_WALLET row must identify the player AND record the resulting balance.
    -- A HOUSE_* row must NOT carry a player_id (revenue pools are aggregated).
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

-- Block UPDATE/DELETE entirely. The ledger is append-only; corrections
-- happen by posting offsetting ROLLBACK entries, never by mutating history.
CREATE OR REPLACE FUNCTION trg_ledger_entries_no_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is forbidden', TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

CREATE TRIGGER ledger_entries_block_update
    BEFORE UPDATE ON ledger_entries
    FOR EACH STATEMENT
    EXECUTE FUNCTION trg_ledger_entries_no_mutation();

CREATE TRIGGER ledger_entries_block_delete
    BEFORE DELETE ON ledger_entries
    FOR EACH STATEMENT
    EXECUTE FUNCTION trg_ledger_entries_no_mutation();

-- Indexes are declared on the partitioned root; Postgres propagates them to
-- every existing and future partition automatically (PG 11+).
CREATE INDEX ledger_entries_tx_id_idx
    ON ledger_entries (ledger_transaction_id);

CREATE INDEX ledger_entries_player_currency_idx
    ON ledger_entries (player_id, currency, created_at DESC)
    WHERE player_id IS NOT NULL;

CREATE INDEX ledger_entries_account_currency_idx
    ON ledger_entries (account_type, currency, created_at DESC)
    WHERE player_id IS NULL;  -- house-pool aggregation queries

-- =============================================================================
-- Partition management
-- =============================================================================
-- Idempotent helper: create the monthly partition that contains p_month.
-- Operations runs this from a daily cron one month ahead of `now()`.
CREATE OR REPLACE FUNCTION create_ledger_entries_partition(p_month DATE)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    v_start DATE := date_trunc('month', p_month)::DATE;
    v_end   DATE := (date_trunc('month', p_month) + INTERVAL '1 month')::DATE;
    v_name  TEXT := format('ledger_entries_%s', to_char(v_start, 'YYYYMM'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF ledger_entries '
        || 'FOR VALUES FROM (%L) TO (%L)',
        v_name, v_start, v_end
    );
END;
$$;

-- Bootstrap: current month + 12 months forward.
-- The ops cron extends this rolling window thereafter.
DO $$
DECLARE
    i INTEGER;
BEGIN
    FOR i IN 0..12 LOOP
        PERFORM create_ledger_entries_partition(
            (date_trunc('month', now()) + (i || ' months')::INTERVAL)::DATE
        );
    END LOOP;
END
$$;

COMMIT;
