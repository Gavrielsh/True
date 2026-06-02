-- 000005_partitioning.down.sql
--
-- DESTRUCTIVE OPERATION DELIBERATELY DISABLED (same stance as 000004.down).
--
-- Reverting to the monthly scheme means dropping and recreating the daily-
-- partitioned ledger_transactions / ledger_entries (and the dedup anchor), which
-- would erase irreplaceable financial history. We refuse at the SQL level. A
-- genuine teardown (DR rebuild, dev reset on a confirmed-empty DB) must be done
-- manually, as a superuser, after exporting the ledger to cold storage.

BEGIN;

DO $$
BEGIN
    RAISE EXCEPTION
        'refusing to revert ledger partitioning: financial history is irreversible. '
        'For a manual teardown, connect as a superuser and DROP/rebuild explicitly '
        'after exporting ledger_transactions, ledger_entries, and ledger_transaction_dedup.'
        USING ERRCODE = '0A000';  -- feature_not_supported
END
$$;

ROLLBACK;
