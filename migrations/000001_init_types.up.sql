-- 000001_init_types.up.sql
-- Strongly-typed domain ENUMs. ENUMs are validated by the engine itself,
-- so a bad value (e.g. 'SCC' instead of 'SC_REDEEMABLE') can never be inserted.

BEGIN;

-- Player lifecycle
CREATE TYPE user_status AS ENUM (
    'KYC_PENDING',  -- Registered, awaiting identity verification (cannot wager SC)
    'ACTIVE',       -- Fully verified, may bet/win/redeem
    'SUSPENDED',    -- Temporarily blocked (fraud review, self-exclusion)
    'CLOSED'        -- Permanently closed; wallet operations rejected
);

-- Currency segregation per US sweepstakes legal model.
-- GC has no real value; SC_UNPLAYED must be wagered before SC_REDEEMABLE
-- becomes eligible for prize redemption.
CREATE TYPE currency_type AS ENUM (
    'GC',
    'SC_UNPLAYED',
    'SC_REDEEMABLE'
);

-- Logical wallet/account a ledger entry posts against.
-- PLAYER_WALLET rows track real per-player balances; HOUSE_* rows are
-- aggregated asynchronously to avoid single-row hotspots (§6.C).
CREATE TYPE account_type AS ENUM (
    'PLAYER_WALLET',
    'HOUSE_BET_POOL',
    'HOUSE_WIN_POOL',
    'HOUSE_PROMO_POOL',
    'HOUSE_ADJUSTMENT_POOL'
);

-- Top-level operator transaction classifications
CREATE TYPE transaction_type AS ENUM (
    'BET',
    'WIN',
    'ROLLBACK',
    'DEPOSIT',
    'WITHDRAWAL',
    'PROMO_CREDIT',
    'ADJUSTMENT'
);

CREATE TYPE transaction_status AS ENUM (
    'PENDING',
    'COMPLETED',
    'ROLLED_BACK',
    'FAILED'
);

-- Double-entry direction
CREATE TYPE entry_direction AS ENUM (
    'DEBIT',
    'CREDIT'
);

COMMIT;
