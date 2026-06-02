-- 000005_escrow.down.sql
--
-- ENUM value additions are non-destructive and cannot be safely reverted:
-- PostgreSQL has no `ALTER TYPE ... DROP VALUE`, and rebuilding the ENUM would
-- require rewriting every column (and partition) that references it — an
-- offline, lock-heavy operation that has no place in an automated rollback.
--
-- The added values are inert unless the engine writes them, so leaving them in
-- place on a `migrate down` is harmless. This is intentionally a no-op.

DO $$
BEGIN
    RAISE NOTICE
        'migration 000005 down: leaving escrow ENUM values in place '
        '(PostgreSQL cannot drop ENUM values without a full type rebuild).';
END
$$;
