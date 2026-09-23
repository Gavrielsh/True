-- 000011_responsible_gaming.up.sql
--
-- PLAN.md item 6 (engine side): the engine refuses money-moving calls for a
-- player who is self-excluded, in cool-off, or over a loss / deposit limit.
--
-- Two objects:
--
--   1. player_limits — an APPEND-ONLY audit trail of every limit a player (or
--      an operator acting for them) has ever set, exactly like kyc_decisions
--      (000009) and promo_grants (000010). A change is a NEW row, never an
--      UPDATE — so a regulator can always reconstruct the full history of a
--      player's restrictions, not just the current state. "Current state" is
--      derived by reading the most recent row that has taken EFFECT (see
--      effective_at below), never by mutating one.
--
--   2. player_rg_counters — a MUTABLE, per-player running total of loss or
--      deposit activity for the current window (DAY/WEEK/MONTH, UTC
--      boundaries). Unlike player_limits this is NOT financial history: it is
--      a cache of a value fully reconstructable from ledger_transactions /
--      ledger_entries at any time (internal/repository/rg.go seeds or
--      re-seeds it from the ledger whenever its window is missing or stale),
--      so it carries no append-only guarantee and no compliance meaning of
--      its own.
--
-- effective_at encodes the "decreases and new exclusions are immediate;
-- increases and removals wait 24h" rule (self-exclusion/cool-off can never be
-- lifted before their own ends_at at all — enforced in Go, since it depends on
-- reading the prior row, not on a CHECK constraint on this one). Computed by
-- the engine at write time and stored, so every reader (the guard queries in
-- the hot path) only ever needs "effective_at <= now()" — never the 24h rule
-- itself.
--
-- ledger_transactions.usd_amount is added here too: the deposit-limit counter
-- needs the fiat amount of a DEPOSIT, which the ledger did not previously
-- carry (money inside the ledger is always the issued GC/SC amount, not what
-- was paid for it).

BEGIN;

CREATE TYPE rg_limit_type AS ENUM (
    'SELF_EXCLUSION',
    'COOL_OFF',
    'LOSS_LIMIT',
    'DEPOSIT_LIMIT'
);

CREATE TYPE rg_period AS ENUM (
    'DAY',
    'WEEK',
    'MONTH'
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. player_limits — append-only.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE player_limits (
    id            UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id     UUID           NOT NULL REFERENCES users(id),
    limit_type    rg_limit_type  NOT NULL,
    -- period is the counter window for LOSS_LIMIT / DEPOSIT_LIMIT; NULL for
    -- SELF_EXCLUSION / COOL_OFF, which are not windowed.
    period        rg_period,
    -- active = true SETS (or extends) the restriction; active = false LIFTS a
    -- limit or ends an exclusion/cool-off. A lift/removal is still a new row,
    -- never a mutation of the row it lifts.
    active        BOOLEAN        NOT NULL,
    -- amount is the ceiling for an active LOSS_LIMIT / DEPOSIT_LIMIT row.
    -- NULL for SELF_EXCLUSION / COOL_OFF and for any active = false row.
    amount        NUMERIC(18,4),
    -- ends_at is the exclusion/cool-off expiry for an active SELF_EXCLUSION /
    -- COOL_OFF row. COOL_OFF always has one; SELF_EXCLUSION may be NULL only
    -- when active = false (see engine validation — new self-exclusions are
    -- always dated, so "cannot be lifted before its end date" has a date to
    -- compare against). NULL for LOSS_LIMIT / DEPOSIT_LIMIT.
    ends_at       TIMESTAMPTZ,
    -- effective_at is when this row starts governing. Immediate (= created_at)
    -- for a decrease or a new/extended exclusion or cool-off; created_at +
    -- 24h for an increase or a removal. Computed by the engine, not by SQL.
    effective_at  TIMESTAMPTZ    NOT NULL,
    -- event_id is the gateway's idempotency key for THIS set/clear request.
    -- A duplicate delivery raises 23505, resolved the same way as
    -- kyc_decisions: re-read the row the event_id already produced.
    event_id      TEXT           NOT NULL,
    requested_by  TEXT,
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),

    CONSTRAINT player_limits_event_id_unique UNIQUE (event_id),
    CONSTRAINT player_limits_period_shape CHECK (
        (limit_type IN ('LOSS_LIMIT', 'DEPOSIT_LIMIT') AND period IS NOT NULL)
        OR (limit_type IN ('SELF_EXCLUSION', 'COOL_OFF') AND period IS NULL)
    ),
    CONSTRAINT player_limits_amount_shape CHECK (
        (limit_type IN ('LOSS_LIMIT', 'DEPOSIT_LIMIT') AND active AND amount IS NOT NULL AND amount > 0)
        OR (limit_type IN ('LOSS_LIMIT', 'DEPOSIT_LIMIT') AND NOT active AND amount IS NULL)
        OR (limit_type IN ('SELF_EXCLUSION', 'COOL_OFF') AND amount IS NULL)
    ),
    CONSTRAINT player_limits_ends_at_shape CHECK (
        (limit_type IN ('LOSS_LIMIT', 'DEPOSIT_LIMIT') AND ends_at IS NULL)
        OR (limit_type = 'COOL_OFF' AND active AND ends_at IS NOT NULL)
        OR (limit_type = 'COOL_OFF' AND NOT active AND ends_at IS NULL)
        OR (limit_type = 'SELF_EXCLUSION' AND active AND ends_at IS NOT NULL)
        OR (limit_type = 'SELF_EXCLUSION' AND NOT active AND ends_at IS NULL)
    )
);

-- Serves both the "current effective row" guard queries (player_id,
-- limit_type[, period], effective_at <= now(), latest created_at) and the
-- history read endpoint (player_id, created_at DESC).
CREATE INDEX player_limits_lookup_idx
    ON player_limits (player_id, limit_type, created_at DESC);

COMMENT ON TABLE player_limits IS
    'Append-only audit trail of every responsible-gaming limit set for a '
    'player. Current state is the latest row per (player_id, limit_type, '
    'period) with effective_at <= now() — never mutate a row in place.';

CREATE OR REPLACE FUNCTION player_limits_block_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only: % is not permitted '
        '(a correction is a NEW limit row, never a rewrite of history)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

COMMENT ON FUNCTION player_limits_block_mutation() IS
    'Enforces the append-only invariant on player_limits: any UPDATE, DELETE '
    'or TRUNCATE raises SQLSTATE 0A000, for EVERY role including the table '
    'owner and superusers.';

CREATE TRIGGER trg_player_limits_append_only
    BEFORE UPDATE OR DELETE ON player_limits
    FOR EACH ROW
    EXECUTE FUNCTION player_limits_block_mutation();

CREATE TRIGGER trg_player_limits_no_truncate
    BEFORE TRUNCATE ON player_limits
    FOR EACH STATEMENT
    EXECUTE FUNCTION player_limits_block_mutation();

GRANT SELECT, INSERT ON player_limits TO engine_writer;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. player_rg_counters — mutable running total, NOT append-only, NOT
--    financial history. Reconstructable from the ledger at any time.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE player_rg_counters (
    player_id    UUID          NOT NULL REFERENCES users(id),
    counter_type TEXT          NOT NULL,
    period       rg_period     NOT NULL,
    -- window_start pins which DAY/WEEK/MONTH window `amount` belongs to. When
    -- a read finds window_start stale (the window has rolled over) the row is
    -- re-seeded from the ledger rather than trusted — see rg.go.
    window_start TIMESTAMPTZ   NOT NULL,
    amount       NUMERIC(18,4) NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),

    PRIMARY KEY (player_id, counter_type, period),
    CONSTRAINT player_rg_counters_type_valid CHECK (counter_type IN ('LOSS', 'DEPOSIT')),
    CONSTRAINT player_rg_counters_amount_nonneg CHECK (amount >= 0)
);

COMMENT ON TABLE player_rg_counters IS
    'Mutable cache of accumulated loss/deposit activity for a player''s '
    'current limit window. Reconstructable from the ledger — not an audit '
    'trail, so ordinary UPDATE is permitted (unlike player_limits).';

GRANT SELECT, INSERT, UPDATE ON player_rg_counters TO engine_writer;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. ledger_transactions.usd_amount — the fiat amount behind a DEPOSIT, needed
--    to compute the deposit-limit counter. NULL for every other transaction
--    type and, for now, optionally NULL on DEPOSIT too (see PurchaseRequest.
--    USDAmount / repository/casino.go: fails closed when a DEPOSIT_LIMIT is
--    active and the caller omitted it, rather than silently under-counting).
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE ledger_transactions ADD COLUMN usd_amount NUMERIC(18,4);
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_tx_usd_amount_nonneg
    CHECK (usd_amount IS NULL OR usd_amount >= 0);

COMMIT;
