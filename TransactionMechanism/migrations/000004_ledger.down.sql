-- 000004_ledger.down.sql
--
-- DESTRUCTIVE OPERATIONS DELIBERATELY DISABLED.
--
-- A sweepstakes ledger is the financial system of record. A reversible
-- `DROP TABLE` here would let a fat-fingered `migrate down` (or an automated
-- rollback in CI) erase irreplaceable financial history. We refuse the
-- operation at the SQL level. Anyone who genuinely needs to dismantle this
-- schema must do it manually, with explicit human sign-off, after exporting
-- the ledger to cold storage.

BEGIN;

DO $$
BEGIN
    RAISE EXCEPTION
        'refusing to drop ledger_transactions / ledger_entries: '
        'financial history is irreversible. '
        'For manual teardown (DR rebuild, dev reset on a confirmed-empty DB), '
        'connect as a superuser and DROP the objects explicitly.'
        USING ERRCODE = '0A000';  -- feature_not_supported
END
$$;

ROLLBACK;
