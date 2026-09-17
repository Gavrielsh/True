BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- REDEMPTION_REFUND — returning SC_REDEEMABLE a redemption took but never paid.
--
-- WHY A NEW TYPE RATHER THAN 'ROLLBACK' OR 'ADJUSTMENT'
--   ROLLBACK is reserved for reversing a BET and carries a reference to the bet
--   it undoes; reusing it here would put withdrawals into a bucket every report
--   reads as gameplay. ADJUSTMENT is the manual-correction bucket, and a refund
--   is not a correction — it is a scheduled, automatic consequence of a payout
--   that did not happen. Naming it for what it is keeps the one question a
--   regulator asks ("how much did you take and not pay?") answerable with a
--   WHERE clause instead of a forensic exercise.
--
-- CRITICAL EDITING RULE: nothing in this migration may USE the literal
-- 'REDEMPTION_REFUND'. PostgreSQL permits ALTER TYPE … ADD VALUE inside a
-- transaction block but refuses to let that value be USED until the transaction
-- commits ("unsafe use of new value"). A CHECK, a DEFAULT or a seeded row
-- referencing it here would fail at deploy time, not at review time.
--
-- IF NOT EXISTS makes a down/up cycle work: the DOWN cannot remove an enum
-- value (PostgreSQL has no DROP VALUE), so a re-run would otherwise collide.
ALTER TYPE transaction_type ADD VALUE IF NOT EXISTS 'REDEMPTION_REFUND';

COMMIT;
