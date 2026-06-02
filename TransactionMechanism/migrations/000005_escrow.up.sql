-- 000005_escrow.up.sql
--
-- Escrow Mechanism (Withdrawal Double-Spend Fix).
--
-- A withdrawal must LOCK the player's SC_REDEEMABLE in the engine before any
-- ops review, otherwise the player can request a payout and immediately gamble
-- the same funds. We model the lock as a two-phase escrow:
--
--   ESCROW_RESERVE  : DEBIT  player SC_REDEEMABLE  / CREDIT HOUSE_ESCROW_POOL
--                     (status PENDING — funds are now out of the wallet)
--   ESCROW_COMMIT   : DEBIT  HOUSE_ESCROW_POOL     / CREDIT HOUSE_WITHDRAWAL_POOL
--                     (ops approved — the escrowed SC is burned / paid out)
--   ESCROW_RELEASE  : DEBIT  HOUSE_ESCROW_POOL     / CREDIT player SC_REDEEMABLE
--                     (ops rejected — the SC is returned to the player)
--
-- These add new values to two existing ENUMs. PostgreSQL 12+ permits
-- `ALTER TYPE ... ADD VALUE` inside a transaction block PROVIDED the new value
-- is not *used* in the same transaction — which is the case here (this
-- migration only declares them; the engine uses them later, in its own txns).
-- IF NOT EXISTS makes the migration safely re-runnable.

-- New house ledger accounts -----------------------------------------------------
ALTER TYPE account_type ADD VALUE IF NOT EXISTS 'HOUSE_ESCROW_POOL';
ALTER TYPE account_type ADD VALUE IF NOT EXISTS 'HOUSE_WITHDRAWAL_POOL';

-- New transaction classifications ----------------------------------------------
ALTER TYPE transaction_type ADD VALUE IF NOT EXISTS 'ESCROW_RESERVE';
ALTER TYPE transaction_type ADD VALUE IF NOT EXISTS 'ESCROW_COMMIT';
ALTER TYPE transaction_type ADD VALUE IF NOT EXISTS 'ESCROW_RELEASE';
