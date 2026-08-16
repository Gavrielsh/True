package repository

// settle.go is the single-round-trip write path.
//
// It contains NO money arithmetic. Every amount and balance it handles was
// computed by internal/domain and is passed through verbatim; this file only
// marshals those values into the parameters of one SQL statement. Keeping the
// boundary that sharp is the point: the audit's whole premise is that money
// math lives in exactly one place, with one set of tests.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY TWO STATEMENTS AND NOT ONE
// ─────────────────────────────────────────────────────────────────────────────
// The lock/read and the write cannot be fused without moving the allocation
// split into SQL. The SC rule — draw from SC_UNPLAYED first, remainder from
// SC_REDEEMABLE — is a function of the CURRENT balance, which is only known
// after the row is locked. A true single statement would therefore have to
// evaluate LEAST(sc_unplayed, bet) server-side, duplicating money math in a
// second language with no shared test suite. That is precisely the class of
// defect this work exists to remove, so the lock/read stays a separate
// round-trip and the domain layer remains authoritative.
//
// Result: 4 round-trips per settled round (BEGIN, lock+status, settle, COMMIT),
// down from 9 for a losing GC spin and 13 for a winning SC spin — and, unlike
// before, the count no longer grows with the number of ledger lines.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// lockWalletAndStatus takes the pessimistic wallet lock and reads the player's
// lifecycle status in one round-trip.
//
// Semantically identical to the previous pair of statements: the status is
// still read inside the transaction, after the lock, so a suspension committed
// mid-flight is seen by every spin queued behind it.
func lockWalletAndStatus(ctx context.Context, tx pgx.Tx, playerID uuid.UUID) (domain.Wallet, string, error) {
	var (
		gc, scU, scR decimal.Decimal
		status       string
	)
	err := tx.QueryRow(ctx, sqlLockWalletAndStatus, playerID).Scan(&gc, &scU, &scR, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		// The join covers both "no wallet" and "no user"; to a caller the
		// player simply is not playable.
		return domain.Wallet{}, "", errs.ErrPlayerNotFound
	}
	if err != nil {
		return domain.Wallet{}, "", fmt.Errorf("lock wallet: %w", err)
	}
	w, err := walletFromDecimals(playerID, gc, scU, scR)
	if err != nil {
		return domain.Wallet{}, "", err
	}
	return w, status, nil
}

// ledgerEntryRow is one double-entry line, already reduced to wire values.
// `Leg` routes the row to the bet or win transaction header inside the CTE.
type ledgerEntryRow struct {
	Leg          string  // "BET" | "WIN"
	PlayerID     *string // nil for HOUSE_* rows (CHECK constraint)
	AccountType  string
	Currency     string
	Direction    string
	Amount       string  // decimal as text; cast to numeric server-side
	BalanceAfter *string // nil for HOUSE_* rows (CHECK constraint)
}

// buildSpinEntries flattens the domain allocations into ledger lines.
//
// This mirrors, one for one, the rows the previous sequential implementation
// inserted — same account types, same directions, same amounts, same
// balance_after values. Only the delivery mechanism changed.
func buildSpinEntries(
	playerID uuid.UUID,
	bet domain.BetAllocation,
	postBet domain.Wallet,
	win domain.WinAllocation,
	post domain.Wallet,
	hasWin bool,
) []ledgerEntryRow {
	pid := playerID.String()
	// Two lines per debit (player + house), plus two for a win.
	rows := make([]ledgerEntryRow, 0, len(bet.Debits)*2+2)

	for _, d := range bet.Debits {
		balanceAfter := postBet.BalanceFor(d.Currency).String()
		rows = append(rows,
			ledgerEntryRow{
				Leg: "BET", PlayerID: &pid, AccountType: "PLAYER_WALLET",
				Currency: string(d.Currency), Direction: "DEBIT",
				Amount: d.Amount.String(), BalanceAfter: &balanceAfter,
			},
			// Balancing house line: no player_id, no balance_after.
			ledgerEntryRow{
				Leg: "BET", AccountType: "HOUSE_BET_POOL",
				Currency: string(d.Currency), Direction: "CREDIT",
				Amount: d.Amount.String(),
			},
		)
	}

	if hasWin {
		balanceAfter := post.BalanceFor(win.Credit.Currency).String()
		rows = append(rows,
			ledgerEntryRow{
				Leg: "WIN", PlayerID: &pid, AccountType: "PLAYER_WALLET",
				Currency: string(win.Credit.Currency), Direction: "CREDIT",
				Amount: win.Credit.Amount.String(), BalanceAfter: &balanceAfter,
			},
			ledgerEntryRow{
				Leg: "WIN", AccountType: "HOUSE_WIN_POOL",
				Currency: string(win.Credit.Currency), Direction: "DEBIT",
				Amount: win.Credit.Amount.String(),
			},
		)
	}
	return rows
}

// settleSpinParams bundles the arguments of the single settle statement.
type settleSpinParams struct {
	Post         domain.Wallet // the final wallet state, computed by the domain
	PlayerID     uuid.UUID
	OperatorCode string
	BetOpTxID    string
	WinOpTxID    string
	GameID       string
	RoundID      string
	Metadata     json.RawMessage
	HasWin       bool
	Entries      []ledgerEntryRow
}

// settleSpinStatement executes the whole write side in one round-trip and
// returns the two ledger transaction ids. winLedgerID is uuid.Nil when the
// round did not pay.
//
// A unique violation here is the Ghost-Spin signal: the caller checks for it
// and recovers. Because everything is one statement, the wallet update rolls
// back with it — there is no window in which a duplicate leaves a debit
// applied without its ledger trail.
func settleSpinStatement(ctx context.Context, tx pgx.Tx, p settleSpinParams) (betID, winID uuid.UUID, err error) {
	if len(p.Entries) == 0 {
		return uuid.Nil, uuid.Nil, fmt.Errorf("settle: refusing to write a transaction with no ledger entries")
	}

	n := len(p.Entries)
	legs := make([]string, n)
	playerIDs := make([]*string, n)
	accountTypes := make([]string, n)
	currencies := make([]string, n)
	directions := make([]string, n)
	amounts := make([]string, n)
	balanceAfters := make([]*string, n)

	for i, e := range p.Entries {
		legs[i] = e.Leg
		playerIDs[i] = e.PlayerID
		accountTypes[i] = e.AccountType
		currencies[i] = e.Currency
		directions[i] = e.Direction
		amounts[i] = e.Amount
		balanceAfters[i] = e.BalanceAfter
	}

	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}

	// betID is always returned; winID is NULL when the round did not pay, so it
	// scans into a nullable and is normalised to uuid.Nil.
	var winScan *uuid.UUID
	err = tx.QueryRow(ctx, sqlSettleSpin,
		p.Post.GC.Decimal().String(),           // $1
		p.Post.SCUnplayed.Decimal().String(),   // $2
		p.Post.SCRedeemable.Decimal().String(), // $3
		p.PlayerID,                             // $4
		p.OperatorCode,                         // $5
		p.BetOpTxID,                            // $6
		nullableString(p.GameID),               // $7
		nullableString(p.RoundID),              // $8
		string(metadata),                       // $9
		p.WinOpTxID,                            // $10
		p.HasWin,                               // $11
		legs,                                   // $12
		playerIDs,                              // $13
		accountTypes,                           // $14
		currencies,                             // $15
		directions,                             // $16
		amounts,                                // $17
		balanceAfters,                          // $18
	).Scan(&betID, &winScan)
	if err != nil {
		return uuid.Nil, uuid.Nil, err // raw: the caller distinguishes 23505
	}

	// A NULL bet id means the wallet UPDATE matched no row, so the CTE chain
	// produced nothing. The FOR UPDATE above proves the row exists, so this is
	// a schema or routing bug — fail closed rather than reporting success.
	if betID == uuid.Nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("settle: wallet row vanished between lock and update")
	}
	if p.HasWin {
		if winScan == nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("settle: win leg produced no ledger transaction")
		}
		winID = *winScan
	}
	return betID, winID, nil
}
