-- 000011_responsible_gaming.down.sql
--
-- CAUTION: drops player_limits and player_rg_counters entirely, losing the
-- responsible-gaming audit trail and counters. ledger_transactions rows
-- themselves survive (append-only, unaffected) — only the usd_amount column
-- added by the up migration is removed.

BEGIN;

ALTER TABLE ledger_transactions DROP CONSTRAINT IF EXISTS ledger_tx_usd_amount_nonneg;
ALTER TABLE ledger_transactions DROP COLUMN IF EXISTS usd_amount;

REVOKE SELECT, INSERT, UPDATE ON player_rg_counters FROM engine_writer;
DROP TABLE IF EXISTS player_rg_counters;

REVOKE SELECT, INSERT ON player_limits FROM engine_writer;
DROP TRIGGER IF EXISTS trg_player_limits_no_truncate ON player_limits;
DROP TRIGGER IF EXISTS trg_player_limits_append_only ON player_limits;
DROP FUNCTION IF EXISTS player_limits_block_mutation();
DROP TABLE IF EXISTS player_limits;

DROP TYPE IF EXISTS rg_period;
DROP TYPE IF EXISTS rg_limit_type;

COMMIT;
