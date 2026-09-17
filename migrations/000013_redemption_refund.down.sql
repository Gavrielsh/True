BEGIN;

-- PostgreSQL has no DROP VALUE for an enum: once added, 'REDEMPTION_REFUND' is
-- permanent. Removing it would mean recreating transaction_type and rewriting
-- every column that uses it — including the partitioned, append-only
-- ledger_transactions — which is not something a DOWN migration may do to a
-- ledger. The value is inert when unused, so leaving it costs nothing.
--
-- This file exists so the migration has a matching DOWN and the sequence stays
-- reversible for every change that CAN be undone. There are none in 000013.
SELECT 1;

COMMIT;
