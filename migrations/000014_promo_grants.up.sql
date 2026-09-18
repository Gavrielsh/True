-- 000014_promo_grants.up.sql
--
-- Milestone: Phase C, task C1 — the AMOE promo-grant record.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHAT THIS MIGRATION DOES *NOT* NEED TO DO
-- ─────────────────────────────────────────────────────────────────────────────
-- Nothing here touches an ENUM. Both values the promo path posts against have
-- existed since 000001:
--
--   transaction_type 'PROMO_CREDIT'    — the ledger classification.
--   account_type     'HOUSE_PROMO_POOL' — the counterparty the credit balances.
--
-- ledger_entries.account_type already admits any non-PLAYER_WALLET account via
-- its account_shape CHECK, exactly as HOUSE_ISSUANCE_POOL does, so the promo
-- pool needs no schema change to be postable. The double-entry machinery is
-- reused verbatim; this migration adds evidence, not mechanism.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHY A TABLE AND NOT A METADATA KEY
-- ─────────────────────────────────────────────────────────────────────────────
-- A PROMO_CREDIT against HOUSE_PROMO_POOL already proves the strong half of the
-- sweepstakes defense: coins were issued with NO fiat leg. That is a structural
-- fact — there is no DEPOSIT, no HOUSE_ISSUANCE_POOL entry, nothing to argue
-- about. "No purchase necessary" is readable straight off the ledger.
--
-- What the ledger alone cannot say is WHICH no-purchase route a grant came
-- through, and that distinction is the whole of the AMOE defense:
--
--   AMOE         — the statutorily-required free entry method. A regulator asks
--                  "show me every mail-in entry you received and prove each one
--                  got the same coins a purchaser would have" and this is the
--                  table that answers.
--   BONUS        — discretionary marketing. Legally a different act.
--   COMPENSATION — a goodwill credit for a service failure.
--
-- Putting the channel in ledger_transactions.request_metadata would make the
-- AMOE defense rest on an unvalidated, unindexed, nullable JSONB key that no
-- constraint protects and any writer can shape differently. A regulator-facing
-- claim cannot rest on a convention. The channel is therefore a typed column on
-- an append-only table with a NOT NULL constraint, written in the SAME
-- transaction as the ledger row, so a grant cannot exist without its origin
-- recorded.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- LEDGER-ADJACENT, NOT LEDGER
-- ─────────────────────────────────────────────────────────────────────────────
-- Same posture as sc_playthrough (000012): these rows hold no balance and move
-- no money. They reference ledger_transaction_id and derive from ledger facts,
-- so the ledger stays the single source of truth and this stays a projection of
-- it. Unlike sc_playthrough it carries no counter, so it is strictly
-- append-only — see the privilege note at the foot of this file.

BEGIN;

DO $promo_grant_channel$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'promo_grant_channel') THEN
        CREATE TYPE promo_grant_channel AS ENUM (
            'AMOE',          -- alternative method of entry: the free route
            'BONUS',         -- discretionary promotional credit
            'COMPENSATION'   -- goodwill credit for a service failure
        );
    END IF;
END
$promo_grant_channel$;

CREATE TABLE IF NOT EXISTS promo_grants (
    id                    UUID                PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id             UUID                NOT NULL REFERENCES users (id),

    -- The ledger transaction that issued the coins. UNIQUE for the same reason
    -- sc_playthrough.ledger_transaction_id is: one grant, one origin record. A
    -- retried grant that reached this table twice would show a single issuance
    -- as two entries — and for the AMOE channel, a single mailed entry as two.
    ledger_transaction_id UUID                NOT NULL UNIQUE,

    channel               promo_grant_channel NOT NULL,

    -- The operator's identifier for the physical entry this grant answers: the
    -- mail-in entry's reference for AMOE, the campaign id for BONUS. Required
    -- for AMOE by the CHECK below, because an AMOE row that cannot be tied back
    -- to a received entry is an assertion, not evidence.
    channel_reference     TEXT,

    -- What was issued, split by currency. Stored rather than derived from the
    -- ledger entries so the AMOE equal-dignity question ("did the free entrant
    -- receive the same as a purchaser") is a column comparison, and so this row
    -- can be reconciled AGAINST the ledger rather than being the same read.
    gc_amount             NUMERIC(18,4)       NOT NULL DEFAULT 0,
    sc_amount             NUMERIC(18,4)       NOT NULL DEFAULT 0,

    created_at            TIMESTAMPTZ         NOT NULL DEFAULT now(),

    CONSTRAINT promo_grants_amounts_non_negative
        CHECK (gc_amount >= 0 AND sc_amount >= 0),
    -- A grant of nothing is not a grant. Mirrors the engine-side validation so
    -- the rule holds for any writer, not just the one that went through Go.
    CONSTRAINT promo_grants_amounts_positive
        CHECK (gc_amount > 0 OR sc_amount > 0),
    -- An AMOE grant MUST name the entry it answers. The other channels may.
    CONSTRAINT promo_grants_amoe_needs_reference
        CHECK (channel <> 'AMOE' OR (channel_reference IS NOT NULL AND length(btrim(channel_reference)) > 0)),
    CONSTRAINT promo_grants_reference_length
        CHECK (channel_reference IS NULL OR length(channel_reference) <= 128)
);

COMMENT ON TABLE promo_grants IS
    'One row per no-purchase coin issuance, recording WHICH free route it came '
    'through. The ledger already proves a PROMO_CREDIT had no fiat leg; this '
    'table distinguishes a statutory AMOE entry from discretionary marketing, '
    'which the ledger cannot. Append-only, ledger-adjacent, holds no balance.';

COMMENT ON COLUMN promo_grants.channel IS
    'AMOE = the statutorily-required free entry method. A typed column rather '
    'than a metadata key: the AMOE defense must not rest on an unvalidated '
    'JSONB convention.';

COMMENT ON COLUMN promo_grants.sc_amount IS
    'SC_UNPLAYED issued. Never SC_REDEEMABLE — a no-purchase path that could '
    'mint cashable tokens would be a cash-out channel with neither purchase nor '
    'gameplay behind it. The engine cannot express it; see domain.AllocatePromoGrant.';

-- "Show me every AMOE entry, newest first." The regulator-facing read, and the
-- one that decides whether this table is usable during an audit or a table scan.
CREATE INDEX IF NOT EXISTS promo_grants_channel_created_idx
    ON promo_grants (channel, created_at DESC);

-- The per-player history read (support answering "where did these coins come
-- from"), and the duplicate-entry check for a mailed AMOE reference.
CREATE INDEX IF NOT EXISTS promo_grants_player_created_idx
    ON promo_grants (player_id, created_at DESC);

CREATE INDEX IF NOT EXISTS promo_grants_channel_reference_idx
    ON promo_grants (channel, channel_reference)
    WHERE channel_reference IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- Privileges
-- ─────────────────────────────────────────────────────────────────────────────
-- SELECT and INSERT only.
--
-- NO UPDATE and NO DELETE, and here that is stricter than sc_playthrough by
-- design: sc_playthrough needs UPDATE because its counter advances as the
-- player wagers. A promo grant has no counter. It is a statement about
-- something that happened, and once written the only legitimate operations are
-- reading it and adding another. UPDATE would let a BONUS be relabelled AMOE
-- after the fact — manufacturing evidence of a free-entry route that was never
-- offered, which is the precise fraud this record exists to make impossible.
--
-- TestLedgerGrants_ExactPrivilegeSet asserts the whole set as an exact match, so
-- this line and that expectation move together or CI fails.

GRANT SELECT, INSERT ON promo_grants TO engine_writer;

COMMIT;
