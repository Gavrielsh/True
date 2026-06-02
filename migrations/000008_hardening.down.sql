-- 000008_hardening.down.sql
--
-- Reverses 000008. Every object created there is operational/derived (a trigger,
-- a guard function, and a maintenance procedure) — NO financial rows — so unlike
-- the ledger migrations this DOWN is safe and complete.

BEGIN;

DROP TRIGGER IF EXISTS trg_ledger_transactions_append_only ON ledger_transactions;
DROP FUNCTION IF EXISTS ledger_transactions_block_mutation();
DROP PROCEDURE IF EXISTS prune_ledger_transaction_dedup_batched(interval, integer, integer);

COMMIT;
