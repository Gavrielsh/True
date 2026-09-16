package repository

// playthrough.go makes the 1× sweepstakes wagering requirement explicit.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THIS DOES AND DOES NOT CHANGE
// ─────────────────────────────────────────────────────────────────────────────
// It changes no money and it does not make the requirement true — the currency
// model already did that. SC_REDEEMABLE is reachable only by winning
// (domain.AllocateWin), and an SC wager drains SC_UNPLAYED before it touches
// SC_REDEEMABLE (domain.AllocateBet), so promotional coins cannot become
// redeemable value without being wagered.
//
// What it adds is the record and the gate. Before this, "did this player play
// through their grant" was answerable only by reasoning about the allocator and
// replaying the ledger; now it is a row, and ProcessRedeem consults it rather
// than trusting the mechanism to have held.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY ONLY SC_UNPLAYED DEBITS COUNT
// ─────────────────────────────────────────────────────────────────────────────
// Playing through a GRANT means wagering the granted coins. SC_REDEEMABLE is
// money the player has already won and already played through once; counting it
// again would discharge a new grant with old wagering and make the requirement
// meaningless for any player carrying a redeemable balance.
//
// The allocator makes this exact: an SC bet's Debits carry the SC_UNPLAYED
// portion separately, so the contribution is read off the allocation rather than
// inferred from the stake.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHERE THE SERIALIZATION COMES FROM
// ─────────────────────────────────────────────────────────────────────────────
// Nowhere new. Every function here runs on a tx that already holds
// SELECT ... FOR UPDATE on the player's wallet row, so concurrent wagers against
// the same grant are serialized by the lock the money path already needed. Two
// simultaneous spins cannot both read the same remaining balance and both
// discharge it.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// PlaythroughMultiplier is the wagering requirement applied to a promotional SC
// grant, as a multiple of the amount granted.
//
// 1×, matching 10 of the 15 benchmarked sweepstakes operators; Stake.us is the
// sole 3× outlier. It is a constant rather than configuration because raising it
// is a material change to what a player is agreeing to when they accept coins,
// and because the stored required_amount fixes each grant's obligation at grant
// time — so changing this affects only FUTURE grants, never one already made.
const PlaythroughMultiplier = 1

const (
	// sqlInsertPlaythroughGrant records the obligation a promotional grant
	// creates. ON CONFLICT DO NOTHING on the ledger transaction id makes it
	// idempotent: the purchase path's own Ghost-Spin recovery can re-enter with
	// the same ledger transaction, and a player must not acquire two wagering
	// requirements from one grant.
	sqlInsertPlaythroughGrant = `
		INSERT INTO sc_playthrough (player_id, ledger_transaction_id, granted_amount, required_amount)
		VALUES ($1, $2, $3::numeric, $4::numeric)
		ON CONFLICT (ledger_transaction_id) DO NOTHING`

	// sqlApplyPlaythroughProgress distributes a wagered amount across the
	// player's outstanding grants, oldest first, in ONE statement.
	//
	// The window function is doing the work that would otherwise be a read, a
	// loop in Go, and a write per grant — three round trips inside the lock
	// window instead of one, with the remaining balances re-derived in
	// application code where they could drift from what the database holds.
	//
	// `prior` is the requirement already absorbed by older grants, so
	// GREATEST(wager - prior, 0) is what reaches this row and LEAST caps it at
	// what this row still needs. A grant is marked SATISFIED in the same
	// statement that completes it, which is what keeps the status and the
	// counter consistent with the CHECK constraint that binds them.
	sqlApplyPlaythroughProgress = `
		WITH outstanding AS (
			SELECT id,
			       required_amount - wagered_amount AS remaining,
			       COALESCE(SUM(required_amount - wagered_amount) OVER (
			           ORDER BY created_at, id
			           ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
			       ), 0) AS prior
			FROM sc_playthrough
			WHERE player_id = $1 AND status = 'OUTSTANDING'
		),
		applied AS (
			SELECT id, LEAST(remaining, GREATEST($2::numeric - prior, 0)) AS amount
			FROM outstanding
		)
		UPDATE sc_playthrough p
		SET wagered_amount = p.wagered_amount + a.amount,
		    status = CASE
		        WHEN p.wagered_amount + a.amount >= p.required_amount THEN 'SATISFIED'
		        ELSE 'OUTSTANDING'
		    END::playthrough_status,
		    satisfied_at = CASE
		        WHEN p.wagered_amount + a.amount >= p.required_amount THEN now()
		        ELSE NULL
		    END
		FROM applied a
		WHERE p.id = a.id AND a.amount > 0`

	// sqlOutstandingPlaythrough is the redemption gate's question, answered as a
	// single number so the caller can report HOW MUCH is left rather than only
	// that something is.
	sqlOutstandingPlaythrough = `
		SELECT COALESCE(SUM(required_amount - wagered_amount), 0)
		FROM sc_playthrough
		WHERE player_id = $1 AND status = 'OUTSTANDING'`
)

// recordPlaythroughGrant registers the obligation created by issuing
// promotional SC_UNPLAYED. A zero grant creates no obligation and writes no row.
func recordPlaythroughGrant(
	ctx context.Context,
	tx pgx.Tx,
	playerID uuid.UUID,
	ledgerTxID uuid.UUID,
	granted domain.Money,
) error {
	if !granted.IsPositive() {
		return nil
	}
	required := granted.Decimal().Mul(decimal.NewFromInt(PlaythroughMultiplier))
	if _, err := tx.Exec(ctx, sqlInsertPlaythroughGrant,
		playerID, ledgerTxID, granted.Decimal(), required); err != nil {
		return fmt.Errorf("record playthrough grant: %w", err)
	}
	return nil
}

// applyPlaythroughProgress credits wagered SC_UNPLAYED against the player's
// outstanding grants, oldest first.
func applyPlaythroughProgress(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, wagered domain.Money) error {
	if !wagered.IsPositive() {
		return nil
	}
	if _, err := tx.Exec(ctx, sqlApplyPlaythroughProgress, playerID, wagered.Decimal()); err != nil {
		return fmt.Errorf("apply playthrough progress: %w", err)
	}
	return nil
}

// scUnplayedDebited extracts the SC_UNPLAYED portion of a bet allocation — the
// part that is genuinely playing through a grant.
//
// Read off the allocation rather than from the stake: an SC bet that straddles
// both buckets debits SC_UNPLAYED first and SC_REDEEMABLE for the remainder, and
// only the first part discharges an obligation.
func scUnplayedDebited(alloc domain.BetAllocation) domain.Money {
	out := domain.ZeroMoney()
	for _, d := range alloc.Debits {
		if d.Currency == domain.CurrencySCUnplayed {
			out = out.Add(d.Amount)
		}
	}
	return out
}

// assertPlaythroughSatisfied is the redemption gate.
//
// Refuses while ANY grant is outstanding, which is the strict reading of a 1×
// requirement and the conservative one: a player who has accepted new
// promotional coins has accepted new wagering with them, and redeeming around
// that would let repeated small purchases wash promotional value straight back
// out as cash. The cost is that a fresh grant holds back a balance earned under
// an already-satisfied one — which is the behaviour the sweepstakes model
// intends, and is stated in the player-facing terms rather than discovered at
// the cashier.
//
// Runs on the same tx handle as the redemption, after the wallet lock, so a
// concurrent wager that discharges the last grant is either fully visible or not
// yet committed — never half-applied.
func assertPlaythroughSatisfied(ctx context.Context, tx pgx.Tx, playerID uuid.UUID) error {
	var outstanding decimal.Decimal
	if err := tx.QueryRow(ctx, sqlOutstandingPlaythrough, playerID).Scan(&outstanding); err != nil {
		return fmt.Errorf("read outstanding playthrough: %w", err)
	}
	if outstanding.IsPositive() {
		return fmt.Errorf("%w: %s SC of wagering remains", errs.ErrPlaythroughOutstanding,
			outstanding.StringFixed(domain.MoneyScale))
	}
	return nil
}
