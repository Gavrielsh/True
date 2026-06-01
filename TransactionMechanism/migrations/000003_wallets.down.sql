-- 000003_wallets.down.sql

BEGIN;

DROP TRIGGER IF EXISTS wallets_set_updated_at ON wallets;
DROP TABLE IF EXISTS wallets;

COMMIT;
