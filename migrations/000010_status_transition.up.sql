-- 000010_status_transition.up.sql
--
-- Milestone: Phase A, task A2 — the status write path.
--
-- 000009 added the state (SELF_EXCLUDED, users.self_exclusion_until) and the
-- evidence trail (player_status_transitions), but deliberately granted nothing
-- new: widening the application role one migration ahead of the code that uses
-- it is how a privilege set drifts. ProcessStatusTransition now exists, so this
-- migration grants exactly what it needs and adds the constraint 000009 could
-- not express.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- A CORRECTION TO 000009's DEFERRAL NOTE
-- ─────────────────────────────────────────────────────────────────────────────
-- 000009 described the deferred constraint as asserting that
-- self_exclusion_until is non-NULL "if and only if" status = 'SELF_EXCLUDED'.
-- That biconditional is wrong, and it contradicts 000009's own column
-- semantics, which state that a past value means "an exclusion that has run its
-- term". The column is deliberately NOT cleared when a served exclusion ends:
-- the fact that a player has previously excluded themselves is exactly the
-- signal a later responsible-gaming or fraud rule needs, and erasing it on
-- return to ACTIVE would destroy it.
--
-- So the converse direction is false BY DESIGN, and only the forward
-- implication is real:
--
--     status = 'SELF_EXCLUDED'  →  self_exclusion_until IS NOT NULL
--
-- which is what is declared below. An exclusion must always have a term. A term
-- may outlive the exclusion that set it.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. The status/term pairing
-- ─────────────────────────────────────────────────────────────────────────────
-- Expressible only now: PostgreSQL forbids using a newly-added enum value in the
-- transaction that added it, so 000009 could not reference the
-- 'SELF_EXCLUDED' literal at all (verified against 16.13). It has since
-- committed, and the literal is legal here.
--
-- This backs the application-level check in ProcessStatusTransition rather than
-- replacing it. The Go validation produces a precise, actionable error for the
-- caller; this constraint is the backstop that makes the invariant true for
-- EVERY writer, including a future migration, an admin console, or a hand-run
-- UPDATE at 3am. A rule enforced only in the application is a rule that holds
-- only where that application is the sole writer.
--
-- Added without NOT VALID because no violating row can exist: before A2 there
-- was no code path in the repository that wrote users.status at all, so nothing
-- has ever set SELF_EXCLUDED in a deployed database.

ALTER TABLE users
    ADD CONSTRAINT users_self_exclusion_has_term
    CHECK (status <> 'SELF_EXCLUDED' OR self_exclusion_until IS NOT NULL);

COMMENT ON CONSTRAINT users_self_exclusion_has_term ON users IS
    'A self-exclusion must always carry a term: status SELF_EXCLUDED requires '
    'self_exclusion_until. The converse does NOT hold — the column survives the '
    'exclusion that set it, so a player returned to ACTIVE keeps the record of '
    'having previously excluded themselves.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. The privilege ProcessStatusTransition needs
-- ─────────────────────────────────────────────────────────────────────────────
-- users previously held INSERT + SELECT: the casino wrapper created players and
-- never modified them. A2 introduces the first and only writer of users.status,
-- so UPDATE is granted here, in the same change that introduces the code.
--
-- UPDATE, and nothing else. No DELETE: a player row is never removed — closure
-- is a status, not a deletion, and an erased player takes their audit trail's
-- referent with them. TestLedgerGrants_ExactPrivilegeSet asserts the whole set
-- as an exact match, so this line and that expectation move together or CI
-- fails, which is the intended ratchet.

GRANT UPDATE ON users TO engine_writer;

COMMIT;
