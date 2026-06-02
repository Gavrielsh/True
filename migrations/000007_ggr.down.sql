-- 000007_ggr.down.sql
--
-- daily_ggr and ggr_aggregator_state are DERIVED analytics, fully
-- reconstructable from ledger_entries by replaying the aggregator. Unlike the
-- ledger itself they hold no irreplaceable source-of-truth data, so dropping
-- them is safe and reversible (re-run 000007 up, then let the worker rebuild).

BEGIN;

DROP TABLE IF EXISTS ggr_aggregator_state;
DROP TABLE IF EXISTS daily_ggr;

COMMIT;
