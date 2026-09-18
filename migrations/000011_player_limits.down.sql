-- 000011_player_limits.down.sql
--
-- Reverses 000011. Every object here is new in this migration, so unlike 000009
-- there is no enum-value residue — with one exception worth reading before you
-- run it.
--
-- ⚠️  ROLLING BACK DESTROYS LIVE PLAYER PROTECTIONS.
--
-- player_limits holds caps players set on themselves, and player_limit_changes
-- is the evidence that the cooling-off period was served. Dropping them is not
-- a neutral schema operation: a player who set a £50 daily loss limit is
-- unprotected the moment this runs, silently, with no event anywhere marking it.
-- Export both tables before a rollback past 000011, and treat the rollback
-- itself as a compliance event. In a deployed environment the answer to a bad
-- 000011 is a forward migration.

BEGIN;

-- Restore A2's stricter form of the constraint, so a database rolled back to
-- 000010 does not permit same-status rows that the code at that version cannot
-- produce. Any extension row already recorded would now violate it, which is why
-- this is a DROP-and-ADD rather than a silent widening: if such a row exists the
-- migration fails loudly instead of leaving the table in a state its own
-- constraint disagrees with.
ALTER TABLE player_status_transitions
    DROP CONSTRAINT IF EXISTS player_status_transitions_is_a_change;

ALTER TABLE player_status_transitions
    ADD CONSTRAINT player_status_transitions_is_a_change
    CHECK (from_status <> to_status);

COMMENT ON CONSTRAINT player_status_transitions_is_a_change ON player_status_transitions IS
    'A transition that changes nothing is a bug in the caller, not an event.';

-- Triggers go with their tables; dropped explicitly first so this migration also
-- reverses cleanly against a database where a table was removed by hand.
DROP TRIGGER IF EXISTS trg_player_limit_changes_append_only ON player_limit_changes;
DROP TRIGGER IF EXISTS trg_player_limit_changes_no_truncate ON player_limit_changes;
DROP TRIGGER IF EXISTS player_limit_usage_set_updated_at    ON player_limit_usage;
DROP TRIGGER IF EXISTS player_limits_set_updated_at         ON player_limits;

DROP TABLE IF EXISTS player_limit_changes;
DROP TABLE IF EXISTS player_limit_usage;
DROP TABLE IF EXISTS player_limits;

DROP FUNCTION IF EXISTS limit_period_start(limit_period, timestamptz);

-- Dropped AFTER the tables that use them. audit_log_block_mutation() is NOT
-- dropped: it belongs to 000009 and still guards player_status_transitions.
DROP TYPE IF EXISTS limit_period;
DROP TYPE IF EXISTS limit_kind;

COMMIT;
