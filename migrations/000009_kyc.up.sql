-- 000009_kyc.up.sql
--
-- C4: KYC status reconciliation between Zone 2 (the QueenRoyal Gateway, which
-- relays identity-verification results from the KYC provider) and Zone 1
-- (this engine, the single source of truth for player status per cursor
-- rule G1 — the Gateway signs and forwards, it never asserts a post-state).
--
-- Two objects:
--   1. kyc_decisions — an APPEND-ONLY audit trail of every decision the
--      engine received, whether or not it was applied. A decision is
--      recorded even when superseded by a newer one (see users.kyc_verified_at
--      below), so a compliance reviewer can always reconstruct exactly what
--      arrived, when the provider decided it, and why it was or wasn't acted
--      on. Same append-only trigger pattern as the ledger (000008): applied to
--      EVERY role, including the table owner and superusers.
--   2. users.kyc_verified_at — the decided_at of the last APPLIED VERIFIED
--      decision. This is the out-of-order resolution baseline: an incoming
--      decision is applied only if its decided_at is strictly newer than this
--      value, so a stale/delayed webhook (of either decision type) can never
--      downgrade or redundantly re-apply a player already verified by a
--      later decision. See internal/repository/kyc.go RecordDecision.

BEGIN;

CREATE TYPE kyc_decision AS ENUM (
    'VERIFIED',
    'REJECTED'
);

-- NULL until the player's first APPLIED VERIFIED decision.
ALTER TABLE users ADD COLUMN kyc_verified_at TIMESTAMPTZ;

CREATE TABLE kyc_decisions (
    id          UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id   UUID          NOT NULL REFERENCES users(id),
    -- external_id is denormalised from users at write time (rather than
    -- joined at read time) so the audit row is self-contained even if the
    -- operator's external_id mapping ever changes later.
    external_id TEXT          NOT NULL,
    -- event_id is the Gateway/provider's idempotency key for this decision
    -- delivery. UNIQUE makes a duplicate webhook delivery a no-op replay
    -- (internal/repository/kyc.go recoverKYCReplay) instead of a second,
    -- possibly-divergent audit row.
    event_id    TEXT          NOT NULL,
    decision    kyc_decision  NOT NULL,
    -- decided_at is the KYC PROVIDER's decision timestamp (carried through
    -- Zone 2), never the time this row was written. It is the value
    -- out-of-order resolution compares against users.kyc_verified_at.
    decided_at  TIMESTAMPTZ   NOT NULL,
    received_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    -- applied records whether THIS decision changed users.status/kyc_verified_at
    -- at the time it was processed — the durable answer to "was this one acted
    -- on or just audited", independent of any later row.
    applied     BOOLEAN       NOT NULL,
    reason      TEXT,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT now(),

    CONSTRAINT kyc_decisions_event_id_unique UNIQUE (event_id)
);

CREATE INDEX kyc_decisions_player_id_idx ON kyc_decisions (player_id, decided_at DESC);

-- Append-only guard (mirrors ledger_block_mutation, 000008). A self-contained
-- function rather than reusing the ledger one: that function's name and
-- COMMENT are documented specifically as a ledger (financial-history)
-- control, and kyc_decisions is a compliance audit trail, not money — keeping
-- them separate means either can evolve without touching the other's
-- contract.
CREATE OR REPLACE FUNCTION kyc_decisions_block_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only: % is not permitted '
        '(a correction is a NEW decision row, never a rewrite of history)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

COMMENT ON FUNCTION kyc_decisions_block_mutation() IS
    'Enforces the append-only invariant on kyc_decisions: any UPDATE, DELETE or '
    'TRUNCATE raises SQLSTATE 0A000, for EVERY role including the table owner '
    'and superusers.';

CREATE TRIGGER trg_kyc_decisions_append_only
    BEFORE UPDATE OR DELETE ON kyc_decisions
    FOR EACH ROW
    EXECUTE FUNCTION kyc_decisions_block_mutation();

CREATE TRIGGER trg_kyc_decisions_no_truncate
    BEFORE TRUNCATE ON kyc_decisions
    FOR EACH STATEMENT
    EXECUTE FUNCTION kyc_decisions_block_mutation();

-- ── engine_writer grants ─────────────────────────────────────────────────────
-- kyc_decisions is an audit trail like the ledger: INSERT + SELECT only, never
-- UPDATE/DELETE (the trigger above blocks it anyway for every role, but the
-- grant is the first layer — see 000008's rationale for defence-in-depth).
GRANT SELECT, INSERT ON kyc_decisions TO engine_writer;

-- users previously granted SELECT, INSERT only (000005), documented there as
-- "never updated by the engine" — true until this migration. RecordDecision
-- is now the engine's first user-row mutation: applying a VERIFIED decision
-- sets status = 'ACTIVE' and kyc_verified_at. Scoped to UPDATE only; row
-- creation remains the casino wrapper's job.
GRANT UPDATE ON users TO engine_writer;

COMMIT;
