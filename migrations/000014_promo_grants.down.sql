-- 000014_promo_grants.down.sql
--
-- Reverses 000014.
--
-- ⚠️  ROLLING BACK DESTROYS THE AMOE EVIDENCE TRAIL.
--
-- The coins themselves survive: every grant is a PROMO_CREDIT in the ledger and
-- the ledger is untouched here. What is lost is the answer to "which of these
-- were statutory free entries", and that answer exists nowhere else — it is not
-- derivable from the ledger, which is the entire reason this table was created
-- rather than a metadata key.
--
-- A sweepstakes operator that cannot produce its AMOE entries on request has a
-- compliance problem that no amount of correct ledger data repairs. Export
-- promo_grants before any rollback past 000014 and treat the rollback as a
-- compliance event; the answer to a bad 000014 in a deployed environment is a
-- forward migration.

BEGIN;

DROP TABLE IF EXISTS promo_grants;
DROP TYPE IF EXISTS promo_grant_channel;

COMMIT;
