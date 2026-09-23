-- 000009_kyc.down.sql
--
-- Reverses 000009. kyc_decisions is an audit trail, not financial history —
-- unlike the ledger migrations this DOWN is safe and complete.

BEGIN;

REVOKE UPDATE ON users FROM engine_writer;
REVOKE SELECT, INSERT ON kyc_decisions FROM engine_writer;

DROP TRIGGER IF EXISTS trg_kyc_decisions_no_truncate ON kyc_decisions;
DROP TRIGGER IF EXISTS trg_kyc_decisions_append_only ON kyc_decisions;
DROP FUNCTION IF EXISTS kyc_decisions_block_mutation();

DROP TABLE IF EXISTS kyc_decisions;

ALTER TABLE users DROP COLUMN IF EXISTS kyc_verified_at;

DROP TYPE IF EXISTS kyc_decision;

COMMIT;
