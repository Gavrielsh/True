-- 000007_ggr.up.sql
--
-- Platform Gross Gaming Revenue (GGR) aggregation tables (architecture §6.C
-- "Platform Revenue Hotspots"). Routing every spin's rake to one master-wallet
-- row would serialise the whole database on a single hot row, so platform
-- revenue is DERIVED asynchronously by the GGR worker (internal/worker) which
-- rolls up ledger_entries on an interval.
--
-- Two tables:
--   * daily_ggr            — the rollup the worker maintains (one row per
--                            gaming_date × currency). ggr = bets - wins.
--   * ggr_aggregator_state — a single-row HIGH WATERMARK so each run processes
--                            only ledger rows it hasn't seen, exactly once.
--
-- Unlike the ledger these are DERIVED and fully reconstructable from
-- ledger_entries, so the down migration may safely drop them.

BEGIN;

-- Per-day, per-currency revenue rollup. NUMERIC(18,4) matches the ledger money
-- type exactly (no float, ever). Values accumulate across worker runs.
CREATE TABLE daily_ggr (
    gaming_date  DATE          NOT NULL,
    currency     currency_type NOT NULL,
    total_bets   NUMERIC(18,4) NOT NULL DEFAULT 0.0000,  -- net HOUSE_BET_POOL inflow (bets - bet rollbacks)
    total_wins   NUMERIC(18,4) NOT NULL DEFAULT 0.0000,  -- net HOUSE_WIN_POOL outflow (wins - win reversals)
    ggr          NUMERIC(18,4) NOT NULL DEFAULT 0.0000,  -- total_bets - total_wins
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),

    CONSTRAINT daily_ggr_pkey PRIMARY KEY (gaming_date, currency)
);

COMMENT ON TABLE daily_ggr IS
    'Asynchronous platform GGR rollup (gaming_date x currency), maintained by the '
    'GGR aggregator worker from ledger_entries. Derived/reconstructable — never a '
    'source of truth. ggr = total_bets - total_wins.';

-- Single-row high-watermark. The boolean PK + CHECK(id) makes a second row
-- impossible, so the worker can SELECT ... FOR UPDATE the one row to serialise
-- overlapping runs (and multiple replicas) without a separate lock table.
CREATE TABLE ggr_aggregator_state (
    id                BOOLEAN     NOT NULL DEFAULT TRUE,
    last_processed_at TIMESTAMPTZ NOT NULL,  -- exclusive lower bound for the next run's window
    last_run_at       TIMESTAMPTZ,           -- observability: when the worker last completed a cycle

    CONSTRAINT ggr_aggregator_state_pkey PRIMARY KEY (id),
    CONSTRAINT ggr_aggregator_state_singleton CHECK (id)
);

COMMENT ON TABLE ggr_aggregator_state IS
    'Single-row high-watermark for the GGR aggregator. last_processed_at is the '
    'exclusive lower bound of the next aggregation window; advancing it inside the '
    'same transaction as the rollup upsert guarantees exactly-once accounting.';

-- Seed the watermark at -infinity so the very first run sweeps from genesis.
-- (On a fresh DB the ledger is empty, so this is a no-op-sized first window.)
INSERT INTO ggr_aggregator_state (id, last_processed_at)
VALUES (TRUE, '-infinity')
ON CONFLICT (id) DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- Grants for the GGR tables (Milestone 0.3).
-- ─────────────────────────────────────────────────────────────────────────────
-- The engine_writer role and the ledger grant set are defined in 000005; these
-- two tables are created here, after that migration runs, so their grants must
-- be issued here. Both privileges are load-bearing:
--
--   * daily_ggr is written by an UPSERT (ON CONFLICT DO UPDATE) in
--     internal/worker/aggregator.go, so UPDATE is required alongside INSERT.
--   * ggr_aggregator_state holds a single watermark row advanced after every
--     aggregation cycle.
--
-- These are DERIVED reporting tables, not financial history: the ledger remains
-- the sole source of truth, and daily_ggr can be rebuilt from it. They are
-- therefore not append-only. TRUNCATE is still granted on neither.
GRANT SELECT, INSERT, UPDATE ON daily_ggr            TO engine_writer;
GRANT SELECT, UPDATE         ON ggr_aggregator_state TO engine_writer;

COMMIT;
