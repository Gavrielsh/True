package repository

// session.go — the read that answers "may this player transact, and on what?"
// in ONE snapshot.
//
// WHY THIS EXISTS SEPARATELY FROM GetBalances
//
//	GetBalances answers a presentation question: what should the player see in
//	the header. The gateway needs a different, compliance-shaped answer before
//	it will let a redemption through — the player's lifecycle status and how
//	much playthrough is still owed — and until this existed it had no way to
//	ask. The result was two policy gates that were present in Zone 2's code and
//	inert in reality, because the facts they judged were not observable there.
//
// WHY ONE QUERY AND NOT THREE
//
//	Balance, status and outstanding playthrough are read in a single statement
//	so they come from ONE MVCC snapshot. Three separate reads can straddle a
//	concurrent transition and return a combination that never actually existed
//	— an ACTIVE status beside a balance from after the suspension, say — which
//	is precisely the sort of inconsistency a caller would then encode into a
//	decision.
//
// THIS IS A SNAPSHOT, NOT A LOCK. It takes no FOR UPDATE and grants nothing.
// A caller may use it to refuse early; it must never be used to authorise a
// debit, because the wallet is unlocked the instant this returns. The engine's
// own guards inside the money path remain the authority — see requirePlayerActive
// and assertPlaythroughSatisfied.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// sqlSelectSessionSnapshot reads wallet, status and outstanding playthrough
// together. The playthrough term is the SAME expression the redemption gate
// uses (sqlOutstandingPlaythrough) rather than a second formulation of it: two
// definitions of "how much is owed" would eventually disagree, and the one the
// player is shown must be the one the engine enforces.
const sqlSelectSessionSnapshot = `
	SELECT w.gc_balance,
	       w.sc_unplayed_balance,
	       w.sc_redeemable_balance,
	       u.status,
	       COALESCE((SELECT SUM(required_amount - wagered_amount)
	                 FROM sc_playthrough
	                 WHERE player_id = $1 AND status = 'OUTSTANDING'), 0)
	FROM wallets w
	JOIN users u ON u.id = w.player_id
	WHERE w.player_id = $1`

// SessionSnapshot is everything the gateway needs to decide whether to attempt
// a money operation, and nothing it needs to perform one.
type SessionSnapshot struct {
	Wallet domain.Wallet
	// Status is the player's lifecycle status (ACTIVE, SUSPENDED,
	// SELF_EXCLUDED, KYC_PENDING, CLOSED) — the same column every money path
	// checks under the wallet lock.
	Status string
	// PlaythroughOutstanding is the SC still owed to the 1x wagering
	// requirement, summed across every OUTSTANDING grant. Zero means a
	// redemption will not be refused on playthrough grounds.
	PlaythroughOutstanding domain.Money
}

// GetSessionSnapshot loads the consistent read described above.
func (e *engine) GetSessionSnapshot(ctx context.Context, playerID uuid.UUID) (SessionSnapshot, error) {
	if playerID == uuid.Nil {
		return SessionSnapshot{}, fmt.Errorf("%w: nil player id", errs.ErrPlayerNotFound)
	}

	var (
		gc, scU, scR decimal.Decimal
		status       string
		outstanding  decimal.Decimal
	)
	err := e.db.QueryRow(ctx, sqlSelectSessionSnapshot, playerID).
		Scan(&gc, &scU, &scR, &status, &outstanding)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionSnapshot{}, errs.ErrPlayerNotFound
	}
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("select session snapshot: %w", err)
	}

	wallet, err := walletFromDecimals(playerID, gc, scU, scR)
	if err != nil {
		return SessionSnapshot{}, err
	}
	owed, err := domain.NewMoney(outstanding)
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("outstanding playthrough: %w", err)
	}

	return SessionSnapshot{Wallet: wallet, Status: status, PlaythroughOutstanding: owed}, nil
}
