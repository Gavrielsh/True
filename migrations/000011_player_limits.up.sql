-- 000011_player_limits.up.sql
--
-- Milestone: Phase A, task A3 — player-set wagering limits, and the
-- self-exclusion extension gap A2 left open.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHAT A LIMIT HAS TO SURVIVE
-- ─────────────────────────────────────────────────────────────────────────────
-- A daily loss limit is only a control if it holds when a player fires twenty
-- concurrent spins at it. That is not a schema property — it comes from
-- evaluating the limit inside the SAME pessimistic wallet lock the wager takes,
-- which is what internal/repository/limits.go does. What the schema contributes
-- is somewhere to hold the limit, somewhere to hold the running total, and the
-- guarantee that the two cannot be edited behind the player's back.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- THE COOLING-OFF ASYMMETRY
-- ─────────────────────────────────────────────────────────────────────────────
-- A limit DECREASE takes effect immediately; a limit INCREASE takes effect only
-- after 24 hours. The asymmetry is the entire mechanism: a player in the middle
-- of a losing session who raises their limit is, at that moment, the least able
-- to judge whether they should. The delay returns the decision to a calmer
-- version of the same person, and the evidence for it is the same body of work
-- behind the 180-day exclusion floor.
--
-- It is modelled as a PENDING value on the same row rather than as a scheduled
-- job, because a job that fails to run silently grants the increase late — or,
-- worse, a job that runs while a limit has since been lowered grants it at all.
-- Here the effective value is a pure function of the row and the clock:
--
--     effective = CASE WHEN pending_effective_at <= now() THEN pending ELSE amount END
--
-- so there is nothing to schedule, nothing to miss, and a decrease simply clears
-- the pending increase on its way past.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Kinds and periods
-- ─────────────────────────────────────────────────────────────────────────────

DO $limit_kind$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'limit_kind') THEN
        CREATE TYPE limit_kind AS ENUM (
            'WAGER',  -- total staked within the period
            'LOSS'    -- net loss (stakes less returns) within the period
        );
    END IF;
END
$limit_kind$;

COMMENT ON TYPE limit_kind IS
    'What a player limit caps. WAGER counts every stake. LOSS counts stakes net '
    'of returns, so a winning session consumes none of it. A DEPOSIT limit is '
    'deliberately absent: the engine is never told the fiat price of a purchase '
    '(ProcessPurchase receives coin amounts), so a deposit cap here would be '
    'denominated in Gold Coins, which is not what a player or a regulator means '
    'by one. It belongs with the cashier work that carries the fiat amount.';

DO $limit_period$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'limit_period') THEN
        CREATE TYPE limit_period AS ENUM ('DAILY', 'WEEKLY', 'MONTHLY');
    END IF;
END
$limit_period$;

-- limit_period_start reduces an instant to the start of its period.
--
-- Declared once and used by BOTH the evaluation read and the usage upsert. If
-- the two disagreed about where a day begins — by a timezone, or by one using
-- date_trunc and the other a cast — a player's spend would be checked against
-- one bucket and recorded in another, and the limit would leak a little at every
-- boundary. Sharing the function makes that class of bug unexpressible.
--
-- UTC throughout, matching the ledger's TIMESTAMPTZ columns. A player-local
-- "gaming day" is a product decision that would belong here, and deliberately is
-- not assumed.
CREATE OR REPLACE FUNCTION limit_period_start(p limit_period, at timestamptz)
RETURNS timestamptz
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT date_trunc(
        CASE p
            WHEN 'DAILY'   THEN 'day'
            WHEN 'WEEKLY'  THEN 'week'
            WHEN 'MONTHLY' THEN 'month'
        END,
        at AT TIME ZONE 'UTC'
    ) AT TIME ZONE 'UTC';
$$;

COMMENT ON FUNCTION limit_period_start(limit_period, timestamptz) IS
    'Start of the period containing `at`, in UTC. The single definition of a '
    'period boundary, shared by limit evaluation and usage accounting so the two '
    'can never disagree about which bucket a wager belongs to.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. The limits themselves
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS player_limits (
    player_id            UUID          NOT NULL REFERENCES users (id),
    limit_kind           limit_kind    NOT NULL,
    period               limit_period  NOT NULL,

    -- The value in force now. NUMERIC(18,4) exactly like every other money
    -- column: a limit compared against a stake must be the same kind of number
    -- as the stake, or the comparison acquires a rounding rule nobody chose.
    amount               NUMERIC(18,4) NOT NULL,

    -- A requested INCREASE, not yet in force. Both columns move together.
    pending_amount       NUMERIC(18,4),
    pending_effective_at TIMESTAMPTZ,

    created_at           TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ   NOT NULL DEFAULT now(),

    PRIMARY KEY (player_id, limit_kind, period),

    -- Zero is a legitimate limit — "I do not want to wager at all" — so the
    -- bound is >= 0, not > 0. A negative cap is meaningless.
    CONSTRAINT player_limits_amount_non_negative
        CHECK (amount >= 0),
    CONSTRAINT player_limits_pending_amount_non_negative
        CHECK (pending_amount IS NULL OR pending_amount >= 0),

    -- A half-written pending increase is the dangerous shape: an amount with no
    -- effective time would either never apply or apply immediately, depending on
    -- which side of the CASE it fell. Both or neither.
    CONSTRAINT player_limits_pending_is_complete
        CHECK ((pending_amount IS NULL) = (pending_effective_at IS NULL)),

    -- Only an INCREASE is ever pending. A decrease applies immediately, so a
    -- pending value at or below the current one would be a decrease that was
    -- quietly delayed — exactly the inversion the cooling-off rule exists to
    -- prevent.
    CONSTRAINT player_limits_pending_is_an_increase
        CHECK (pending_amount IS NULL OR pending_amount > amount)
);

COMMENT ON TABLE player_limits IS
    'Player-set wagering limits. Evaluated inside the pessimistic wallet lock '
    '(internal/repository/limits.go) so concurrent wagers cannot race past a cap. '
    'The effective value is a pure function of this row and the clock: the '
    'pending increase applies once pending_effective_at has passed, and a '
    'decrease clears it. Absence of a row means no limit of that kind.';

COMMENT ON COLUMN player_limits.pending_effective_at IS
    'When a requested INCREASE becomes effective — the 24-hour cooling-off '
    'deadline. Never set for a decrease, which applies at once.';

-- Supports the cooling-off visibility query ("which increases land soon"), and
-- is partial because the overwhelming majority of rows have nothing pending.
CREATE INDEX IF NOT EXISTS player_limits_pending_idx
    ON player_limits (pending_effective_at)
    WHERE pending_effective_at IS NOT NULL;

CREATE TRIGGER player_limits_set_updated_at
    BEFORE UPDATE ON player_limits
    FOR EACH ROW
    EXECUTE FUNCTION trg_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Running usage
-- ─────────────────────────────────────────────────────────────────────────────
-- Counters rather than an aggregate over the ledger. Summing ledger_entries per
-- player per period on every wager is correct but unbounded: the cost grows with
-- the player's activity inside the period, on the hot path, inside the lock. A
-- counter is one row and O(1).
--
-- The ledger stays the authority. These rows are a derived accounting aid, and
-- the honest consequence is that they could in principle drift from it — a
-- reconciliation pass over the ledger belongs with the existing balance
-- reconciler rather than being hand-waved here.
--
-- Usage is recorded ONLY for (kind, period) pairs the player actually has a
-- limit on. A player with no limits pays nothing for this table, and a player
-- with one daily loss limit pays a single upsert. The consequence, stated
-- plainly: a limit set midway through a period counts from the moment it is
-- set, not from the start of the period.

CREATE TABLE IF NOT EXISTS player_limit_usage (
    player_id    UUID          NOT NULL REFERENCES users (id),
    limit_kind   limit_kind    NOT NULL,
    period       limit_period  NOT NULL,
    period_start TIMESTAMPTZ   NOT NULL,

    -- MAY GO NEGATIVE, deliberately, and only for LOSS: a player who is up for
    -- the period has a negative net loss. Clamping at zero would quietly grant
    -- extra headroom on the way back down — a player up 500 who then loses
    -- 500 + L would have exceeded their net-loss limit L while the clamped
    -- counter still read L. The comparison, not the storage, is where the
    -- semantics live.
    used         NUMERIC(18,4) NOT NULL DEFAULT 0,

    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),

    PRIMARY KEY (player_id, limit_kind, period, period_start)
);

COMMENT ON TABLE player_limit_usage IS
    'Running total consumed against a player limit within one period bucket. '
    'Written in the SAME transaction as the wager that consumes it, under the '
    'wallet lock, so the total a concurrent wager reads is never stale. Rows '
    'exist only for limits the player has set.';

CREATE TRIGGER player_limit_usage_set_updated_at
    BEFORE UPDATE ON player_limit_usage
    FOR EACH ROW
    EXECUTE FUNCTION trg_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. The audit trail for limit changes
-- ─────────────────────────────────────────────────────────────────────────────
-- Setting a limit is a compliance action, and so is raising one. Append-only for
-- the same reason player_status_transitions is: the question a regulator asks is
-- "when did this player raise their limit, and did you make them wait" — and an
-- answer that could be edited afterwards is not an answer.

CREATE TABLE IF NOT EXISTS player_limit_changes (
    id                   UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id            UUID          NOT NULL REFERENCES users (id),
    limit_kind           limit_kind    NOT NULL,
    period               limit_period  NOT NULL,

    -- NULL previous_amount means this is the first limit of its kind.
    previous_amount      NUMERIC(18,4),
    requested_amount     NUMERIC(18,4) NOT NULL,

    -- The direction as the system classified it, stored rather than recomputed:
    -- it is what determines whether the player had to wait, so it is the fact
    -- under audit.
    direction            TEXT          NOT NULL,
    -- NULL when the change applied immediately (a decrease or a first limit).
    effective_at         TIMESTAMPTZ,

    actor_type           status_actor  NOT NULL,
    actor_ref            TEXT,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT now(),

    CONSTRAINT player_limit_changes_direction
        CHECK (direction IN ('SET', 'DECREASE', 'INCREASE')),
    CONSTRAINT player_limit_changes_amounts_non_negative
        CHECK (requested_amount >= 0 AND (previous_amount IS NULL OR previous_amount >= 0)),
    -- An increase is the only kind that waits, and it always does.
    CONSTRAINT player_limit_changes_increase_waits
        CHECK ((direction = 'INCREASE') = (effective_at IS NOT NULL)),
    CONSTRAINT player_limit_changes_actor_ref_shape
        CHECK (actor_ref IS NULL OR (actor_ref ~ '[^[:space:]]' AND length(actor_ref) <= 200))
);

COMMENT ON TABLE player_limit_changes IS
    'Immutable record of every player limit change: the previous value, the '
    'requested value, whether it was treated as an increase, and when it became '
    'effective. Append-only by trigger and by grant. This is the evidence that '
    'the cooling-off period was actually served.';

CREATE INDEX IF NOT EXISTS player_limit_changes_player_idx
    ON player_limit_changes (player_id, created_at DESC);

-- Reuses 000009's audit guard: same mechanism, same remediation advice (record
-- a further change), so unlike the ledger guard there is nothing to specialise.
DROP TRIGGER IF EXISTS trg_player_limit_changes_append_only ON player_limit_changes;
CREATE TRIGGER trg_player_limit_changes_append_only
    BEFORE UPDATE OR DELETE ON player_limit_changes
    FOR EACH ROW
    EXECUTE FUNCTION audit_log_block_mutation();

DROP TRIGGER IF EXISTS trg_player_limit_changes_no_truncate ON player_limit_changes;
CREATE TRIGGER trg_player_limit_changes_no_truncate
    BEFORE TRUNCATE ON player_limit_changes
    FOR EACH STATEMENT
    EXECUTE FUNCTION audit_log_block_mutation();

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. The self-exclusion extension gap (carried over from A2)
-- ─────────────────────────────────────────────────────────────────────────────
-- A2 shipped with a hole worth naming plainly: a self-excluded player could not
-- ask for LONGER. player_status_transitions required from_status <> to_status,
-- so SELF_EXCLUDED → SELF_EXCLUDED had nowhere to be recorded, and a control
-- that lets a player protect themselves less but not more is the wrong way
-- round.
--
-- The constraint is relaxed for exactly that case and no other. A transition
-- must still CHANGE something — either the status, or, for a self-exclusion, the
-- term. Every other same-status pair stays refused, because a no-op transition
-- is still a caller bug.
--
-- That the new term must be strictly LATER than the current one is enforced in
-- ProcessStatusTransition, not here: this row records what happened, and it has
-- no access to the term the player was already serving.

ALTER TABLE player_status_transitions
    DROP CONSTRAINT IF EXISTS player_status_transitions_is_a_change;

ALTER TABLE player_status_transitions
    ADD CONSTRAINT player_status_transitions_is_a_change
    CHECK (
        from_status <> to_status
        OR (to_status = 'SELF_EXCLUDED' AND self_exclusion_until IS NOT NULL)
    );

COMMENT ON CONSTRAINT player_status_transitions_is_a_change ON player_status_transitions IS
    'A transition must change something. The one same-status case permitted is '
    'SELF_EXCLUDED → SELF_EXCLUDED carrying a term, which is a player extending '
    'their own exclusion; ProcessStatusTransition additionally requires the new '
    'term to be strictly later than the one in force.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. Privileges
-- ─────────────────────────────────────────────────────────────────────────────
-- player_limits: SELECT (evaluate), INSERT and UPDATE (set and revise). No
-- DELETE — a limit is removed by raising it, which serves the cooling-off
-- period, rather than by deleting the row and skipping the wait.
--
-- player_limit_usage: SELECT, INSERT, UPDATE for the running total. No DELETE:
-- expired period buckets are retention-pruned by a maintenance path, not erased
-- by the wager path, so a player cannot have today's spend forgotten.
--
-- player_limit_changes: INSERT + SELECT, matching every other audit table.
--
-- TestLedgerGrants_ExactPrivilegeSet asserts the whole set as an exact match, so
-- these three lines and that expectation move together or CI fails.

GRANT SELECT, INSERT, UPDATE ON player_limits       TO engine_writer;
GRANT SELECT, INSERT, UPDATE ON player_limit_usage  TO engine_writer;
GRANT SELECT, INSERT         ON player_limit_changes TO engine_writer;

GRANT EXECUTE ON FUNCTION limit_period_start(limit_period, timestamptz) TO engine_writer;

COMMIT;
