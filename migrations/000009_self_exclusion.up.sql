-- 000009_self_exclusion.up.sql
--
-- Milestone: Phase A, task A1 — the enforcement substrate.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHY THIS MIGRATION EXISTS
-- ─────────────────────────────────────────────────────────────────────────────
-- requirePlayerActive (internal/repository/engine.go) already refuses every
-- money path — BET, WIN, SPIN, PURCHASE, REDEEM — for any player whose status is
-- not ACTIVE. That guard is correct and is serialized under the wallet row lock.
--
-- What has never existed is a way to REACH a non-ACTIVE status. No statement in
-- the engine writes users.status: `grep "UPDATE users"` across
-- internal/repository returns nothing. The enforcement is a door with no handle.
-- Self-exclusion, operator suspension and regulator-mandated closure are all
-- unimplementable until the state is writable and auditable.
--
-- This migration supplies the missing state and its audit trail. It deliberately
-- adds NO write path: ProcessStatusTransition (task A2) is the only thing that
-- will be allowed to move a player between statuses, and it is reviewed
-- separately from the schema that supports it.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHY SELF_EXCLUDED IS A DISTINCT STATUS AND NOT A REUSE OF SUSPENDED
-- ─────────────────────────────────────────────────────────────────────────────
-- 000001 labelled SUSPENDED as "Temporarily blocked (fraud review,
-- self-exclusion)" — conflating two states with opposite semantics:
--
--   * SUSPENDED is an OPERATOR decision, reversible by the operator at will. It
--     exists to protect the house (fraud review, chargeback investigation).
--   * SELF_EXCLUSION is a PLAYER decision, and is IRREVOCABLE before its expiry
--     — including by the operator, and including at the player's own later
--     request. That irrevocability is the entire point: a player who can talk
--     themselves back in has not been excluded. It exists to protect the player.
--
-- Collapsing them means either self-exclusions become liftable (defeating the
-- control) or fraud suspensions become permanent (breaking operations). They
-- must be separate values so the transition rules in A2 can differ, and so the
-- compliance record can distinguish "we blocked them" from "they blocked
-- themselves" — a distinction any regulator will ask about directly.
--
-- Both statuses are blocked identically by the existing guard: requirePlayerActive
-- tests `status != 'ACTIVE'`, so SELF_EXCLUDED is FAIL-CLOSED on every money path
-- from the instant this migration lands, with no Go change required. The 000001
-- comment is corrected below to match.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- ⚠️  HARD CONSTRAINT ON EDITING THIS FILE — READ BEFORE ADDING ANYTHING
-- ─────────────────────────────────────────────────────────────────────────────
-- PostgreSQL permits ALTER TYPE ... ADD VALUE inside a transaction block (12+),
-- but FORBIDS using the added value in that same transaction:
--
--     ERROR:  unsafe use of new value "SELF_EXCLUDED" of enum type user_status
--     HINT:   New enum values must be committed before they can be used.
--
-- Verified empirically against PostgreSQL 16.13, which is why this file is
-- written the way it is. golang-migrate sends this file as one multi-statement
-- string and the BEGIN/COMMIT below is that transaction, so the restriction
-- applies to EVERY statement in this file.
--
-- Therefore: nothing in this migration may reference the LITERAL 'SELF_EXCLUDED'.
-- Not in a DEFAULT, not in a CHECK, not in a partial-index predicate, not in an
-- INSERT. Declaring a COLUMN of type user_status is fine — that uses the type,
-- not the value. A future constraint that needs the literal (see the deferred
-- note on users.self_exclusion_until below) belongs in 000010, after this one
-- has committed.
--
-- Splitting the enum change into its own migration would lift the restriction,
-- and was considered. It is kept together because the table and column here are
-- meaningless without the status, and a half-applied pair is a worse failure
-- mode than a documented editing rule.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. The status itself
-- ─────────────────────────────────────────────────────────────────────────────

-- IF NOT EXISTS keeps this re-runnable against a database that has already been
-- forward-migrated, which matters because the DOWN migration CANNOT remove the
-- value (see 000009_self_exclusion.down.sql) — so a down/up cycle would
-- otherwise fail on the second up.
ALTER TYPE user_status ADD VALUE IF NOT EXISTS 'SELF_EXCLUDED';

-- Correct 000001's comment, which attributed self-exclusion to SUSPENDED.
COMMENT ON TYPE user_status IS
    'Player lifecycle. KYC_PENDING: registered, unverified, cannot wager SC. '
    'ACTIVE: verified, may bet/win/redeem — the ONLY status any money path '
    'accepts. SUSPENDED: operator-initiated block (fraud review, chargeback), '
    'reversible by the operator. SELF_EXCLUDED: player-initiated, IRREVOCABLE '
    'until users.self_exclusion_until has passed, not liftable by the operator '
    'or on player request. CLOSED: permanent. Every transition between these is '
    'recorded in player_status_transitions.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Self-exclusion expiry
-- ─────────────────────────────────────────────────────────────────────────────
-- Held on users (not only in the audit log) because the wager path reads it
-- under the wallet lock in task A3, and that read must not join.
--
-- NULL means "no self-exclusion is or has been in force". A non-NULL value in
-- the past means an exclusion that has run its term; the player is not returned
-- to ACTIVE automatically by this schema — a transition row must record the
-- return, so the audit trail stays complete (task A2).

ALTER TABLE users ADD COLUMN IF NOT EXISTS self_exclusion_until TIMESTAMPTZ;

COMMENT ON COLUMN users.self_exclusion_until IS
    'Instant at which a player-initiated self-exclusion expires. Irrevocable '
    'while in the future: no operator action and no player request may clear or '
    'shorten it. NULL when no exclusion has ever been set. Minimum term is '
    'enforced in the application (ProcessStatusTransition), not here, because '
    'the floor is policy and is expected to differ by jurisdiction.';

-- Supports the expiry sweep that returns served exclusions to ACTIVE. Partial,
-- because the overwhelming majority of rows are NULL and never need visiting.
CREATE INDEX IF NOT EXISTS users_self_exclusion_until_idx
    ON users (self_exclusion_until)
    WHERE self_exclusion_until IS NOT NULL;

-- DEFERRED TO 000010, for the reason in the editing note above: a CHECK
-- asserting that self_exclusion_until is non-NULL if and only if
-- status = 'SELF_EXCLUDED' requires that literal, which this transaction may
-- not reference. Until then the pairing is an application invariant enforced by
-- ProcessStatusTransition (A2) and asserted by its tests.

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Who caused a transition
-- ─────────────────────────────────────────────────────────────────────────────
-- Provenance is a separate axis from the status reached. "Closed" tells a
-- regulator nothing; "closed, actor REGULATOR, ref <order id>" is the record.
--
-- A freshly CREATEd enum type is exempt from the restriction that governs
-- section 1 — its values may be used in the same transaction — but this
-- migration still declares no DEFAULT here. An audit row that does not name its
-- actor is not an audit row, so the column is NOT NULL with no fallback and
-- every caller must state who acted.

DO $status_actor$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'status_actor') THEN
        CREATE TYPE status_actor AS ENUM (
            'PLAYER',     -- the player themselves (self-exclusion, account closure)
            'OPERATOR',   -- staff action (fraud suspension, compliance hold)
            'SYSTEM',     -- automated (KYC webhook rejection, exclusion expiry sweep)
            'REGULATOR'   -- externally mandated (AG order, licence condition)
        );
    END IF;
END
$status_actor$;

COMMENT ON TYPE status_actor IS
    'Provenance of a player status transition. Distinguishes a player-initiated '
    'exclusion from an operator suspension from a regulator-mandated closure — '
    'a distinction the status alone cannot carry, and the first thing asked for '
    'in a compliance review.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. The audit trail
-- ─────────────────────────────────────────────────────────────────────────────
-- APPEND-ONLY, on the same reasoning as the ledger: a compliance record that
-- can be edited after the fact is not evidence. Enforced by trigger below AND
-- by the grant set, which are complementary — see 000008's note. The grants stop
-- the application role; the trigger also stops the table owner and a superuser.

CREATE TABLE IF NOT EXISTS player_status_transitions (
    id                   UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id            UUID         NOT NULL REFERENCES users (id),

    -- Both endpoints are recorded. Storing only the destination would make the
    -- log unverifiable against users.status: a gap or a reordering would be
    -- undetectable. With both, the chain is self-checking — each row's
    -- from_status must equal the previous row's to_status for that player.
    from_status          user_status  NOT NULL,
    to_status            user_status  NOT NULL,

    actor_type           status_actor NOT NULL,
    -- Admin user id, regulator order reference, KYC provider event id, or the
    -- name of the automated sweep. NULL only for a transition with no external
    -- reference to cite.
    actor_ref            TEXT,
    reason               TEXT         NOT NULL,

    -- The expiry SET BY THIS TRANSITION, when it set one. Carrying it here makes
    -- the log self-contained: the term a player actually agreed to is readable
    -- from the row that created it, without trusting the current users value,
    -- which a later transition will have overwritten.
    self_exclusion_until TIMESTAMPTZ,

    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- A transition that changes nothing is a bug in the caller, not an event.
    CONSTRAINT player_status_transitions_is_a_change
        CHECK (from_status <> to_status),
    -- An unexplained compliance action is not defensible; refuse to store one.
    --
    -- The test is "contains at least one non-whitespace character", NOT
    -- length(btrim(...)) > 0. btrim() strips ONLY spaces by default, so the
    -- btrim form accepted a reason of "\t" or "\n" — whitespace-only text that
    -- reads as blank in every report it ever appears in. The POSIX class covers
    -- tabs, newlines and the rest.
    CONSTRAINT player_status_transitions_reason_present
        CHECK (reason ~ '[^[:space:]]'),
    -- Bound the free-text fields so the audit trail cannot be used as a blob
    -- store (and so one poisoned row cannot bloat a compliance export).
    CONSTRAINT player_status_transitions_reason_length
        CHECK (length(reason) <= 500),
    -- Same two properties for the optional reference: absent is fine, present
    -- and blank is not.
    CONSTRAINT player_status_transitions_actor_ref_shape
        CHECK (actor_ref IS NULL OR (actor_ref ~ '[^[:space:]]' AND length(actor_ref) <= 200))
);

COMMENT ON TABLE player_status_transitions IS
    'Immutable record of every player status change: who moved them, from what, '
    'to what, why, and when. Append-only by trigger and by grant. This is the '
    'evidence file for self-exclusion and for operator and regulator actions; '
    'ProcessStatusTransition (internal/repository) writes exactly one row per '
    'transition, inside the same transaction as the users UPDATE, so a status '
    'can never move without a record of why.';

-- History for one player, newest first: the admin player-detail read.
CREATE INDEX IF NOT EXISTS player_status_transitions_player_idx
    ON player_status_transitions (player_id, created_at DESC);

-- "Every self-exclusion in a period", across players: the regulator-facing
-- query, and the one a periodic compliance export runs.
CREATE INDEX IF NOT EXISTS player_status_transitions_to_status_idx
    ON player_status_transitions (to_status, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Append-only enforcement
-- ─────────────────────────────────────────────────────────────────────────────
-- A sibling of 000008's ledger_block_mutation() rather than a reuse of it. The
-- mechanism is identical; the REMEDIATION is not. The ledger's guard tells the
-- caller to post a compensating ROLLBACK transaction, which is meaningless for
-- an audit log — the correct response here is to record a further transition.
-- An error message that misdirects the operator at 3am is worth more than the
-- dozen lines saved by sharing one function.

CREATE OR REPLACE FUNCTION audit_log_block_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        '% is an append-only audit log: % is not permitted '
        '(record a further transition instead)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = '0A000';  -- feature_not_supported
END;
$$;

COMMENT ON FUNCTION audit_log_block_mutation() IS
    'Enforces append-only on compliance audit tables: any UPDATE, DELETE or '
    'TRUNCATE raises SQLSTATE 0A000 for EVERY role, including the table owner '
    'and superusers. Deliberately separate from ledger_block_mutation() (000008) '
    'so the error names the correct remedy for an audit log. Never fires on INSERT.';

DROP TRIGGER IF EXISTS trg_player_status_transitions_append_only ON player_status_transitions;
CREATE TRIGGER trg_player_status_transitions_append_only
    BEFORE UPDATE OR DELETE ON player_status_transitions
    FOR EACH ROW
    EXECUTE FUNCTION audit_log_block_mutation();

-- Row triggers do not see TRUNCATE; a statement-level guard is required to close
-- that path for the owner. engine_writer additionally holds no TRUNCATE anywhere.
DROP TRIGGER IF EXISTS trg_player_status_transitions_no_truncate ON player_status_transitions;
CREATE TRIGGER trg_player_status_transitions_no_truncate
    BEFORE TRUNCATE ON player_status_transitions
    FOR EACH STATEMENT
    EXECUTE FUNCTION audit_log_block_mutation();

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. Privileges
-- ─────────────────────────────────────────────────────────────────────────────
-- INSERT + SELECT only, matching the ledger tables. No UPDATE (a recorded
-- transition is never rewritten), no DELETE (an exclusion is never erased), no
-- TRUNCATE. TestLedgerGrants_ExactPrivilegeSet asserts the whole grant set as an
-- EXACT match, so this table is added to that test's expectation in the same
-- commit — a grant added here without the test update fails CI, which is the
-- intended ratchet.
--
-- users is NOT granted UPDATE here. ProcessStatusTransition needs it and will
-- request it in task A2, where the write path it unlocks is reviewed alongside
-- it. Widening the role's rights one migration ahead of the code that uses them
-- is how a privilege set drifts.

GRANT SELECT, INSERT ON player_status_transitions TO engine_writer;

COMMIT;
