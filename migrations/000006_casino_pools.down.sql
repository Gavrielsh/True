-- 000006_casino_pools.down.sql
--
-- PostgreSQL has no `ALTER TYPE ... DROP VALUE`. Removing an enum label requires
-- rewriting the type (create new enum, rewrite every column that uses it, drop
-- old) — an expensive, lock-heavy, and risky operation that is never worth doing
-- just to reverse an additive label. The added labels are inert once the casino
-- endpoints stop emitting them (no rows reference a value that isn't written), so
-- the safe "down" is a no-op that documents this rather than a destructive type
-- rewrite. This deliberately mirrors the ledger's refusal to self-destruct.

DO $$
BEGIN
    RAISE NOTICE
        'down 000006: account_type values HOUSE_ISSUANCE_POOL / HOUSE_REDEMPTION_POOL '
        'are left in place. PostgreSQL cannot DROP enum values without a full type '
        'rewrite; the unused labels are harmless. Remove manually (type rewrite) only '
        'if you are certain no ledger_entries rows reference them.';
END
$$;
