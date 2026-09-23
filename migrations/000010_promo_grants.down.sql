-- 000010_promo_grants.down.sql
--
-- Reverses 000010. CAUTION: promo_grants is a typed companion to ledger rows.
-- Dropping it does not remove the PROMO_CREDIT ledger transactions it
-- describes (those are append-only and stay), but it does lose the channel
-- (AMOE / BONUS / COMPENSATION) and channel_reference of every grant. Run this
-- only on a database that has never issued a grant.

BEGIN;

REVOKE SELECT, INSERT ON promo_grants FROM engine_writer;

DROP TRIGGER IF EXISTS trg_promo_grants_no_truncate ON promo_grants;
DROP TRIGGER IF EXISTS trg_promo_grants_append_only ON promo_grants;
DROP FUNCTION IF EXISTS promo_grants_block_mutation();

DROP TABLE IF EXISTS promo_grants;

DROP TYPE IF EXISTS promo_grant_channel;

COMMIT;
