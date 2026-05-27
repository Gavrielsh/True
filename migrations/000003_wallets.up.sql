-- 000003_wallets.up.sql
--
-- One wallet row per player. The three currency columns are co-located on a
-- single row so a single `SELECT ... FOR UPDATE` covers all balance mutations
-- for a player atomically (avoids multi-row deadlocks during slot spins where
-- /bet may touch both SC_UNPLAYED and SC_REDEEMABLE in the same transaction).
--
-- The CHECK constraints are the last line of defence: even if a domain bug
-- attempts to overdraw a wallet, Postgres aborts the transaction.

BEGIN;

CREATE TABLE wallets (
    id                     UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id              UUID          NOT NULL
                                         REFERENCES users(id) ON DELETE RESTRICT,
    gc_balance             NUMERIC(18,4) NOT NULL DEFAULT 0.0000,
    sc_unplayed_balance    NUMERIC(18,4) NOT NULL DEFAULT 0.0000,
    sc_redeemable_balance  NUMERIC(18,4) NOT NULL DEFAULT 0.0000,
    created_at             TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ   NOT NULL DEFAULT now(),

    CONSTRAINT wallets_player_id_unique         UNIQUE (player_id),
    CONSTRAINT wallets_gc_nonneg                CHECK (gc_balance            >= 0.0000),
    CONSTRAINT wallets_sc_unplayed_nonneg       CHECK (sc_unplayed_balance   >= 0.0000),
    CONSTRAINT wallets_sc_redeemable_nonneg     CHECK (sc_redeemable_balance >= 0.0000)
);

CREATE TRIGGER wallets_set_updated_at
    BEFORE UPDATE ON wallets
    FOR EACH ROW
    EXECUTE FUNCTION trg_set_updated_at();

COMMIT;
