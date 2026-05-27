-- 000004_ledger.down.sql

BEGIN;

DROP FUNCTION IF EXISTS create_ledger_entries_partition(DATE);

-- Dropping the partitioned root cascades to all monthly partitions.
DROP TABLE IF EXISTS ledger_entries CASCADE;
DROP FUNCTION IF EXISTS trg_ledger_entries_no_mutation();

DROP TABLE IF EXISTS ledger_transactions;

COMMIT;
