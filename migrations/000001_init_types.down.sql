-- 000001_init_types.down.sql

BEGIN;

DROP TYPE IF EXISTS entry_direction;
DROP TYPE IF EXISTS transaction_status;
DROP TYPE IF EXISTS transaction_type;
DROP TYPE IF EXISTS account_type;
DROP TYPE IF EXISTS currency_type;
DROP TYPE IF EXISTS user_status;

COMMIT;
