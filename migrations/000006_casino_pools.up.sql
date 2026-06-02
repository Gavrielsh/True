-- 000006_casino_pools.up.sql
--
-- Adds the two house pools the real-money wrapper endpoints post against:
--   * HOUSE_ISSUANCE_POOL   — counterparty for /store/purchase. Player wallet is
--                             CREDITed (GC + SC_UNPLAYED promo); this pool is
--                             DEBITed by the same amounts (double-entry balance).
--   * HOUSE_REDEMPTION_POOL — counterparty for /store/redeem. Player wallet
--                             SC_REDEEMABLE is DEBITed; this pool is CREDITed.
--
-- ENUM VALUES CANNOT BE ADDED INSIDE A TRANSACTION THAT THEN USES THEM, and on
-- PostgreSQL < 12 not inside a transaction at all. We therefore intentionally
-- do NOT wrap these statements in BEGIN/COMMIT: run as autonomous, autocommitted
-- statements. They are idempotent via IF NOT EXISTS, and nothing in this
-- migration consumes the new values, so it is safe under every runner (psql
-- autocommit, golang-migrate, etc.).
--
-- ledger_entries.account_type already permits any account_type via its
-- account_shape CHECK ("account_type <> 'PLAYER_WALLET' AND player_id IS NULL
-- AND balance_after IS NULL"), so these pools need no further schema changes —
-- they behave exactly like HOUSE_BET_POOL / HOUSE_WIN_POOL.

ALTER TYPE account_type ADD VALUE IF NOT EXISTS 'HOUSE_ISSUANCE_POOL';
ALTER TYPE account_type ADD VALUE IF NOT EXISTS 'HOUSE_REDEMPTION_POOL';
