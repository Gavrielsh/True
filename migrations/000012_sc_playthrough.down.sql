-- 000012_sc_playthrough.down.sql
--
-- Reverses 000012.
--
-- ⚠️  ROLLING BACK DISCARDS OUTSTANDING WAGERING OBLIGATIONS.
--
-- Every OUTSTANDING row is a promotional grant a player has not yet played
-- through, and the redemption gate reads exactly those rows. Dropping the table
-- does not merely remove a record — it silently satisfies every open obligation,
-- because a gate with nothing to find admits everyone. Export the table before a
-- rollback past 000012 and treat the rollback as a compliance event; the answer
-- to a bad 000012 in a deployed environment is a forward migration.
--
-- The underlying 1x property survives the rollback: it is enforced structurally
-- by the currency model (SC_REDEEMABLE is reachable only by winning, and an SC
-- wager drains SC_UNPLAYED first), not by this table. What is lost is the
-- evidence and the explicit gate, not the mechanism.

BEGIN;

DROP TRIGGER IF EXISTS sc_playthrough_set_updated_at ON sc_playthrough;
DROP TABLE IF EXISTS sc_playthrough;
DROP TYPE IF EXISTS playthrough_status;

COMMIT;
