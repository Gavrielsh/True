-- 000012_sc_playthrough.up.sql
--
-- Milestone: Phase A, task A4 — the explicit 1× playthrough record.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHAT WAS ALREADY TRUE, AND WHY IT WAS NOT ENOUGH
-- ─────────────────────────────────────────────────────────────────────────────
-- The engine's currency model already makes 1× playthrough STRUCTURALLY true.
-- SC_REDEEMABLE can only be reached by winning (domain.AllocateWin credits it),
-- and an SC wager drains SC_UNPLAYED before it touches SC_REDEEMABLE
-- (domain.AllocateBet). A player therefore cannot convert promotional coins into
-- redeemable value without wagering them first. That is the control working.
--
-- What did not exist was the EVIDENCE. "Prove this player played through their
-- grant before redeeming" could only be answered by reasoning about the
-- allocator and replaying the ledger — an argument, not a record. A regulator
-- asking the question during an audit, or a support agent asking it during a
-- redemption dispute, gets a derivation rather than a row.
--
-- This migration makes the requirement a first-class record: one row per grant,
-- carrying what was granted, what must be wagered, what has been wagered, and
-- when it was satisfied — each tied to the ledger transaction that issued it.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- LEDGER-ADJACENT, NOT LEDGER
-- ─────────────────────────────────────────────────────────────────────────────
-- These rows hold no balance and move no money. They reference
-- ledger_transaction_id and derive from ledger facts, so the ledger stays the
-- single source of truth and this table stays a projection of it — which is what
-- makes it safe to UPDATE a counter here while the ledger itself remains
-- strictly append-only.
--
-- The counter is nonetheless reconstructible from the ledger alone (sum the
-- SC_UNPLAYED debits per player against the SC_UNPLAYED credits), so a
-- reconciliation pass can verify it rather than trust it. That pass belongs with
-- the existing balance reconciler and is not written here.

BEGIN;

DO $playthrough_status$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'playthrough_status') THEN
        CREATE TYPE playthrough_status AS ENUM (
            'OUTSTANDING',  -- wagering requirement not yet met
            'SATISFIED'     -- met; the grant no longer holds redemption back
        );
    END IF;
END
$playthrough_status$;

CREATE TABLE IF NOT EXISTS sc_playthrough (
    id                    UUID               PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id             UUID               NOT NULL REFERENCES users (id),

    -- The grant that created this obligation. UNIQUE because a ledger
    -- transaction issues SC_UNPLAYED exactly once: without it, a retried
    -- purchase that somehow reached this table twice would double a player's
    -- wagering requirement off the back of a single grant.
    ledger_transaction_id UUID               NOT NULL UNIQUE,

    granted_amount        NUMERIC(18,4)      NOT NULL,

    -- The requirement is STORED, not computed from granted_amount at read time.
    -- 1× today, but a promotion with a different multiplier must not
    -- retroactively change what a player already agreed to when the constant
    -- moves — the obligation is fixed at the moment of the grant.
    required_amount       NUMERIC(18,4)      NOT NULL,

    wagered_amount        NUMERIC(18,4)      NOT NULL DEFAULT 0,

    status                playthrough_status NOT NULL DEFAULT 'OUTSTANDING',
    satisfied_at          TIMESTAMPTZ,

    created_at            TIMESTAMPTZ        NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ        NOT NULL DEFAULT now(),

    -- A grant of nothing creates no obligation and should never be recorded.
    CONSTRAINT sc_playthrough_granted_positive  CHECK (granted_amount > 0),
    CONSTRAINT sc_playthrough_required_positive CHECK (required_amount > 0),
    -- Progress only ever moves forward, and never past the requirement: the
    -- waterfall that applies a wager caps each row at its remaining balance, so
    -- an overshoot here means that arithmetic is wrong.
    CONSTRAINT sc_playthrough_wagered_in_range
        CHECK (wagered_amount >= 0 AND wagered_amount <= required_amount),
    -- The status and the counter must agree. Without this a row could read
    -- SATISFIED while still short — the single most dangerous inconsistency
    -- available here, since the redemption gate reads the status.
    CONSTRAINT sc_playthrough_status_matches_progress
        CHECK (
            (status = 'SATISFIED'   AND wagered_amount >= required_amount AND satisfied_at IS NOT NULL)
         OR (status = 'OUTSTANDING' AND wagered_amount <  required_amount AND satisfied_at IS NULL)
        )
);

COMMENT ON TABLE sc_playthrough IS
    'One row per promotional SC_UNPLAYED grant: the 1x wagering requirement it '
    'carries, the progress against it, and when it was met. Ledger-adjacent — '
    'holds no balance, references the granting ledger transaction, and is '
    'reconstructible from the ledger. ProcessRedeem refuses while any row for a '
    'player is OUTSTANDING.';

COMMENT ON COLUMN sc_playthrough.required_amount IS
    'SC that must be wagered to discharge this grant. Fixed at grant time so a '
    'later change to the multiplier cannot retroactively alter an obligation a '
    'player already accepted.';

-- The redemption gate's query: "does this player have anything outstanding".
-- Partial, because a satisfied grant is never consulted again and the table is
-- append-mostly — the index stays proportional to open obligations, not to
-- lifetime grants.
CREATE INDEX IF NOT EXISTS sc_playthrough_outstanding_idx
    ON sc_playthrough (player_id)
    WHERE status = 'OUTSTANDING';

-- FIFO ordering for the progress waterfall, and the player-history read.
CREATE INDEX IF NOT EXISTS sc_playthrough_player_created_idx
    ON sc_playthrough (player_id, created_at, id);

CREATE TRIGGER sc_playthrough_set_updated_at
    BEFORE UPDATE ON sc_playthrough
    FOR EACH ROW
    EXECUTE FUNCTION trg_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Privileges
-- ─────────────────────────────────────────────────────────────────────────────
-- SELECT (the gate), INSERT (record a grant), UPDATE (advance the counter).
--
-- NO DELETE, deliberately, and this is the load-bearing one: DELETE would let an
-- outstanding obligation be made to disappear, which is precisely the move the
-- record exists to prevent. A grant is discharged by being wagered, never by
-- being removed.
--
-- TestLedgerGrants_ExactPrivilegeSet asserts the whole set as an exact match, so
-- this line and that expectation move together or CI fails.

GRANT SELECT, INSERT, UPDATE ON sc_playthrough TO engine_writer;

COMMIT;
