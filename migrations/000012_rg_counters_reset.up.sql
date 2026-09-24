-- 000012_rg_counters_reset.up.sql
--
-- player_rg_counters is a re-derivable CACHE, not a source of truth — every
-- row is re-seeded from ledger_transactions/ledger_entries on demand (see
-- getOrSeedCounter in internal/repository/rg.go). Rows written by 000011
-- before the SC-only fix (this same PR) may have counted GC activity toward
-- a LOSS_LIMIT that must be SC-only. Deleting them forces the next read of
-- each window to re-seed correctly from the ledger with the corrected
-- (SC-only) aggregate query; no ledger data is touched or lost.

BEGIN;

DELETE FROM player_rg_counters;

COMMIT;
