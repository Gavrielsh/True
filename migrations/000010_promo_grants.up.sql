-- 000010_promo_grants.up.sql
--
-- POST /api/v1/store/promo-grant: coins issued with NO purchase behind them.
--
-- Three no-purchase routes exist and they are legally different acts, so the
-- route is a TYPED column rather than a loose metadata convention:
--   * AMOE         — the statutory Alternative Method of Entry ("no purchase
--                    necessary"). It is what makes the business a sweepstakes
--                    rather than a lottery, so every AMOE credit must be
--                    traceable to the entry it answers: channel_reference is
--                    REQUIRED for it (CHECK below).
--   * BONUS        — discretionary marketing (daily wheel, missions, streaks).
--   * COMPENSATION — goodwill credits issued by support.
--
-- The ledger side needs no schema change: the grant posts a PROMO_CREDIT
-- ledger transaction (transaction_type, 000001) whose player CREDITs are
-- balanced by HOUSE_PROMO_POOL DEBITs (account_type, 000001). Like purchases,
-- a grant issues GC and/or SC_UNPLAYED only — never SC_REDEEMABLE — and that
-- is enforced by the domain allocator, not merely by convention.
--
-- promo_grants is the typed per-grant record written in the SAME database
-- transaction as the ledger rows. It is keyed by the ledger transaction id so
-- the two can never disagree about which movement a grant row describes, and
-- it is append-only for every role (same trigger pattern as 000008/000009).
--
-- THE ENGINE DOES NOT CAP GRANTS. It records what an authenticated operator
-- asks for; frequency caps (one AMOE per period, one daily bonus per day) are
-- the Gateway's job, enforced there by unique indexes taken BEFORE this call.

BEGIN;

CREATE TYPE promo_grant_channel AS ENUM (
    'AMOE',
    'BONUS',
    'COMPENSATION'
);

CREATE TABLE promo_grants (
    ledger_transaction_id   UUID                 PRIMARY KEY,
    operator_code           TEXT                 NOT NULL,
    operator_transaction_id TEXT                 NOT NULL,
    player_id               UUID                 NOT NULL REFERENCES users(id),
    channel                 promo_grant_channel  NOT NULL,
    channel_reference       TEXT,
    gc_amount               NUMERIC(18,4)        NOT NULL,
    sc_amount               NUMERIC(18,4)        NOT NULL,
    created_at              TIMESTAMPTZ          NOT NULL DEFAULT now(),

    CONSTRAINT promo_grants_operator_tx_unique
        UNIQUE (operator_code, operator_transaction_id),
    CONSTRAINT promo_grants_amounts_non_negative
        CHECK (gc_amount >= 0 AND sc_amount >= 0),
    CONSTRAINT promo_grants_amount_positive
        CHECK (gc_amount > 0 OR sc_amount > 0),
    CONSTRAINT promo_grants_amoe_is_traceable
        CHECK (channel <> 'AMOE'
               OR (channel_reference IS NOT NULL AND length(btrim(channel_reference)) > 0))
);

CREATE INDEX promo_grants_player_idx ON promo_grants (player_id, created_at DESC);
CREATE INDEX promo_grants_channel_idx ON promo_grants (channel, created_at DESC);

-- Append-only guard. Self-contained (like 000009's) so the ledger's own guard
-- function keeps its narrowly-documented contract.
CREATE OR REPLACE FUNCTION promo_grants_block_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only: % is not permitted '
        '(a correction is a new ADJUSTMENT, never a rewrite of history)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

COMMENT ON FUNCTION promo_grants_block_mutation() IS
    'Enforces the append-only invariant on promo_grants: any UPDATE, DELETE or '
    'TRUNCATE raises SQLSTATE 0A000, for EVERY role including the table owner '
    'and superusers.';

CREATE TRIGGER trg_promo_grants_append_only
    BEFORE UPDATE OR DELETE ON promo_grants
    FOR EACH ROW
    EXECUTE FUNCTION promo_grants_block_mutation();

CREATE TRIGGER trg_promo_grants_no_truncate
    BEFORE TRUNCATE ON promo_grants
    FOR EACH STATEMENT
    EXECUTE FUNCTION promo_grants_block_mutation();

-- ── engine_writer grants ─────────────────────────────────────────────────────
-- Financial history: INSERT + SELECT only (the trigger blocks the rest anyway).
GRANT SELECT, INSERT ON promo_grants TO engine_writer;

COMMIT;
