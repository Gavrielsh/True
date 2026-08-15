package repository

// spin.go implements the SERVER-AUTHORITATIVE game round.
//
// This is the fix for the audit's most severe finding. The old flow accepted
// `win_amount` from an external caller and booked it verbatim, unbounded:
// anyone holding a webhook secret could credit themselves arbitrary
// SC_REDEEMABLE — the currency that converts to cash.
//
// ProcessSpin inverts the trust direction. The caller supplies ONLY a stake.
// The server:
//
//	1. draws the reels from crypto/rand (internal/game),
//	2. evaluates them against a version-pinned paytable,
//	3. derives the win itself, and
//	4. commits the debit AND the credit in ONE transaction.
//
// There is no wire field that reaches the credit amount. A caller cannot
// choose the outcome, the multiplier, or the payout.
//
// ATOMICITY: bet and win share a single DB transaction and a single wallet
// lock. That structurally eliminates the "bet committed, win failed"
// compensation path the gateway carries for third-party provider spins — for
// first-party games the two legs cannot diverge.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/game"
	"github.com/Gavrielsh/True/internal/metrics"
	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// Suffixes appended to the caller's spin id to derive the two ledger
// transactions' operator ids. Deterministic, so a retry of the same spin
// collides on the dedup anchor and is ghost-recovered rather than replayed.
const (
	betLegSuffix = ":bet"
	winLegSuffix = ":win"
)

// SpinRequest is a validated first-party game round.
//
// Note what is ABSENT: no win amount, no multiplier, no outcome. The server
// derives all three. Adding any of them to this struct would reopen the
// vulnerability this file exists to close.
type SpinRequest struct {
	OperatorCode          string
	OperatorTransactionID string // stable spin id; the idempotency anchor
	PlayerID              uuid.UUID
	Family                domain.CurrencyFamily // GC or SC
	BetAmount             domain.Money          // > 0, bounded by MaxMoneyUnits
	GameID                string                // resolves the paytable; "" → default
	RoundID               string
	Metadata              json.RawMessage
	// BodyHash — see BetRequest.BodyHash.
	BodyHash string
}

func (r SpinRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if !r.Family.Valid() {
		return fmt.Errorf("%w: family=%s", errs.ErrUnsupportedCurrency, r.Family)
	}
	if !r.BetAmount.IsPositive() {
		return fmt.Errorf("%w: bet amount must be > 0", errs.ErrInvalidAmount)
	}
	// An unbounded stake is an unbounded win, however tightly the paytable is
	// capped — the payout is bet × multiplier.
	return domain.CheckAmountBound(r.BetAmount)
}

// SpinResult is the authoritative record of a completed round.
type SpinResult struct {
	OperatorCode           string         `json:"operator_code"`
	OperatorTransactionID  string         `json:"operator_transaction_id"`
	PlayerID               uuid.UUID      `json:"player_id"`
	BetLedgerTransactionID uuid.UUID      `json:"bet_ledger_transaction_id"`
	WinLedgerTransactionID *uuid.UUID     `json:"win_ledger_transaction_id,omitempty"`
	Family                 string         `json:"family"`
	BetAmount              domain.Money   `json:"bet_amount"`
	WinAmount              domain.Money   `json:"win_amount"`
	Outcome                game.Outcome   `json:"outcome"`
	PostBalances           BalanceSummary `json:"post_balances"`
	Status                 ResultStatus   `json:"status"`
}

// GameEngine is the server-authoritative spin surface. Deliberately separate
// from Engine so existing consumers and their test doubles are untouched.
type GameEngine interface {
	// ProcessSpin draws, evaluates, and settles one round atomically.
	ProcessSpin(ctx context.Context, req SpinRequest) (SpinResult, error)
}

type gameEngine struct {
	core *engine
	rng  game.RNG
}

// NewGame builds the spin engine.
//
// rng defaults to crypto/rand when nil. There is deliberately NO configuration
// path to a weaker source — a deployment cannot accidentally downgrade the
// entropy behind the game.
func NewGame(db DB, idem cache.Store, rng game.RNG, logger *slog.Logger) GameEngine {
	if logger == nil {
		logger = slog.Default()
	}
	if rng == nil {
		rng = game.NewCryptoRNG()
	}
	return &gameEngine{
		core: &engine{db: db, idem: idem, logger: logger},
		rng:  rng,
	}
}

// ProcessSpin runs the full round.
func (g *gameEngine) ProcessSpin(ctx context.Context, req SpinRequest) (SpinResult, error) {
	if err := req.validate(); err != nil {
		return SpinResult{}, err
	}

	// Resolve the paytable BEFORE anything else. An unknown game is rejected
	// outright — never fall back to a default, or a caller could shop for a
	// better-paying table by sending a bogus game_id.
	gameID := req.GameID
	if gameID == "" {
		gameID = game.DefaultGameID
	}
	paytable, ok := game.Lookup(gameID)
	if !ok {
		return SpinResult{}, fmt.Errorf("%w: unknown game_id %q", errs.ErrUnsupportedGame, gameID)
	}

	e := g.core
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	// ---- Phase 1: Redis idempotency ----
	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		return SpinResult{}, mapIdempotencyErr(req.OperatorCode, err)
	}
	switch status {
	case cache.StatusPending:
		return SpinResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCachedSpin(payload)
	}

	// ---- Phase 2: draw the outcome OUTSIDE the transaction ----
	// The RNG reads the OS entropy pool. Drawing here rather than inside the
	// tx keeps that read out of the wallet-lock window.
	//
	// Safe under retry: if the tx below fails, a retry redraws — but that
	// retry can only commit if the original never did. If the original DID
	// commit, the dedup anchor raises 23505 and ghost recovery replays the
	// ORIGINAL outcome from the ledger, discarding the redraw. A player can
	// never re-roll a settled spin.
	outcome, err := game.Spin(paytable, g.rng)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return SpinResult{}, fmt.Errorf("%w: %v", errs.ErrRNGUnavailable, err)
	}
	winAmount, err := domain.NewMoney(game.WinFor(req.BetAmount.Decimal(), outcome, domain.MoneyScale))
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return SpinResult{}, fmt.Errorf("spin: derive win: %w", err)
	}

	// ---- Phase 3: settle both legs atomically ----
	result, err := g.settleSpinTx(ctx, req, paytable, outcome, winAmount)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return SpinResult{}, err
	}

	// ---- Phase 4: cache the response ----
	e.cacheSpinQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (g *gameEngine) settleSpinTx(
	ctx context.Context,
	req SpinRequest,
	paytable game.Paytable,
	outcome game.Outcome,
	winAmount domain.Money,
) (result SpinResult, err error) {
	e := g.core
	defer metrics.ObserveDBLockDuration(metrics.OpSpin, time.Now())
	ctx, span := telemetry.StartSpan(ctx, "db.spin_tx",
		attribute.String("player_id", req.PlayerID.String()),
		attribute.String("game_id", paytable.GameID),
		attribute.String("paytable_version", paytable.Version))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SpinResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the wallet for the duration of the transaction.
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return SpinResult{}, err
	}
	// 1b. KYC/compliance guard, serialized behind the lock above.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return SpinResult{}, err
	}

	// 2. Debit leg — pure domain math, no I/O.
	alloc, err := wallet.AllocateBet(req.Family, req.BetAmount)
	if err != nil {
		return SpinResult{}, err
	}
	postBet, err := wallet.ApplyBet(alloc)
	if err != nil {
		return SpinResult{}, err
	}

	// 3. Credit leg, applied on top of the post-bet wallet so the stake is
	//    already deducted when the win lands. This matters when a player
	//    stakes their entire balance and wins: the intermediate state must be
	//    the real post-debit position, not the pre-bet one.
	post := postBet
	var winAlloc domain.WinAllocation
	hasWin := winAmount.IsPositive()
	if hasWin {
		winAlloc, err = postBet.AllocateWin(req.Family, winAmount)
		if err != nil {
			return SpinResult{}, err
		}
		post = postBet.ApplyWin(winAlloc)
	}

	// 4. ONE UPDATE for the net position of both legs.
	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return SpinResult{}, err
	}

	// 5. BET ledger transaction. The outcome is recorded in its metadata so
	//    any historical round can be re-verified against the exact paytable
	//    version that produced it.
	betMeta, err := spinMetadata(req.Metadata, outcome)
	if err != nil {
		return SpinResult{}, err
	}
	betLedgerID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID + betLegSuffix,
		PlayerID:              req.PlayerID,
		Type:                  "BET",
		GameID:                paytable.GameID,
		RoundID:               req.RoundID,
		Reference:             uuid.Nil,
		Metadata:              betMeta,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return g.recoverGhostSpinRound(ctx, req)
		}
		return SpinResult{}, fmt.Errorf("insert bet tx: %w", err)
	}
	span.SetAttributes(attribute.String("bet_ledger_transaction_id", betLedgerID.String()))

	for _, dbt := range alloc.Debits {
		balanceAfter := postBet.BalanceFor(dbt.Currency)
		if err := insertPlayerWalletEntry(ctx, tx, betLedgerID, req.PlayerID, dbt.Currency, "DEBIT", dbt.Amount, balanceAfter); err != nil {
			return SpinResult{}, err
		}
		if err := insertHouseEntry(ctx, tx, betLedgerID, "HOUSE_BET_POOL", dbt.Currency, "CREDIT", dbt.Amount); err != nil {
			return SpinResult{}, err
		}
	}

	// 6. WIN ledger transaction, referencing the bet leg.
	var winLedgerIDPtr *uuid.UUID
	if hasWin {
		winLedgerID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
			OperatorCode:          req.OperatorCode,
			OperatorTransactionID: req.OperatorTransactionID + winLegSuffix,
			PlayerID:              req.PlayerID,
			Type:                  "WIN",
			GameID:                paytable.GameID,
			RoundID:               req.RoundID,
			Reference:             betLedgerID,
			Metadata:              betMeta,
		})
		if err != nil {
			if isUniqueViolation(err) {
				_ = tx.Rollback(ctx)
				return g.recoverGhostSpinRound(ctx, req)
			}
			return SpinResult{}, fmt.Errorf("insert win tx: %w", err)
		}
		balanceAfter := post.BalanceFor(winAlloc.Credit.Currency)
		if err := insertPlayerWalletEntry(ctx, tx, winLedgerID, req.PlayerID, winAlloc.Credit.Currency, "CREDIT", winAlloc.Credit.Amount, balanceAfter); err != nil {
			return SpinResult{}, err
		}
		if err := insertHouseEntry(ctx, tx, winLedgerID, "HOUSE_WIN_POOL", winAlloc.Credit.Currency, "DEBIT", winAlloc.Credit.Amount); err != nil {
			return SpinResult{}, err
		}
		winLedgerIDPtr = &winLedgerID
	}

	if err := tx.Commit(ctx); err != nil {
		return SpinResult{}, fmt.Errorf("commit: %w", err)
	}

	recordSpinMetrics(paytable.GameID, req.Family.String(), string(outcome.Line), req.BetAmount, winAmount)

	return SpinResult{
		OperatorCode:           req.OperatorCode,
		OperatorTransactionID:  req.OperatorTransactionID,
		PlayerID:               req.PlayerID,
		BetLedgerTransactionID: betLedgerID,
		WinLedgerTransactionID: winLedgerIDPtr,
		Family:                 req.Family.String(),
		BetAmount:              req.BetAmount,
		WinAmount:              winAmount,
		Outcome:                outcome,
		PostBalances:           balanceSummaryOf(post),
		Status:                 StatusProcessed,
	}, nil
}

// recoverGhostSpinRound reconstructs a settled round after a 23505 on either
// leg: the previous attempt committed but its Redis cache write was lost.
//
// The ORIGINAL outcome is read back from the ledger metadata — this attempt's
// fresh draw is discarded. That is what prevents a player from re-rolling a
// settled spin by replaying the request until they like the result.
func (g *gameEngine) recoverGhostSpinRound(ctx context.Context, req SpinRequest) (SpinResult, error) {
	e := g.core
	betOpTxID := req.OperatorTransactionID + betLegSuffix

	var (
		betLedgerID  uuid.UUID
		storedPlayer uuid.UUID
		storedMeta   json.RawMessage
	)
	err := e.db.QueryRow(ctx, sqlSelectLedgerTxForGhost, req.OperatorCode, betOpTxID).
		Scan(&betLedgerID, &storedPlayer, &storedMeta)
	if errors.Is(err, pgx.ErrNoRows) {
		return SpinResult{}, fmt.Errorf("%w: %s/%s vanished post-conflict",
			errs.ErrTransactionConflict, req.OperatorCode, betOpTxID)
	}
	if err != nil {
		return SpinResult{}, fmt.Errorf("ghost spin lookup: %w", err)
	}
	if storedPlayer != req.PlayerID {
		return SpinResult{}, fmt.Errorf("%w: spin id reuse across players", errs.ErrTransactionConflict)
	}

	var stored struct {
		Outcome game.Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(storedMeta, &stored); err != nil {
		return SpinResult{}, fmt.Errorf("ghost spin: decode stored outcome: %w", err)
	}

	current, err := e.GetBalances(ctx, req.PlayerID)
	if err != nil {
		return SpinResult{}, fmt.Errorf("ghost spin balance read: %w", err)
	}
	winAmount, err := domain.NewMoney(game.WinFor(req.BetAmount.Decimal(), stored.Outcome, domain.MoneyScale))
	if err != nil {
		return SpinResult{}, fmt.Errorf("ghost spin: re-derive win: %w", err)
	}

	result := SpinResult{
		OperatorCode:           req.OperatorCode,
		OperatorTransactionID:  req.OperatorTransactionID,
		PlayerID:               req.PlayerID,
		BetLedgerTransactionID: betLedgerID,
		Family:                 req.Family.String(),
		BetAmount:              req.BetAmount,
		WinAmount:              winAmount,
		Outcome:                stored.Outcome,
		PostBalances:           balanceSummaryOf(current),
		Status:                 StatusGhostRecovered,
	}
	e.cacheSpinQuietly(ctx, idempotencyKey(req.OperatorCode, req.OperatorTransactionID),
		requestFingerprint(req.PlayerID, req.BodyHash), result)
	metrics.GhostSpinsRecovered.Inc()
	return result, nil
}

// recordSpinMetrics feeds the realised-RTP alerting series. Amounts are
// converted to float ONLY here, for the metric — never for a balance.
func recordSpinMetrics(gameID, family, line string, bet, win domain.Money) {
	metrics.SpinsSettled.WithLabelValues(gameID, line).Inc()
	betF, _ := bet.Decimal().Float64()
	metrics.SpinWagerTotal.WithLabelValues(gameID, family).Add(betF)
	if win.IsPositive() {
		winF, _ := win.Decimal().Float64()
		metrics.SpinPayoutTotal.WithLabelValues(gameID, family).Add(winF)
	}
}

// spinMetadata merges the caller's metadata with the authoritative outcome.
//
// The outcome always wins on key collision — a caller must not be able to
// forge the audit record by sending its own "outcome" key.
func spinMetadata(caller json.RawMessage, outcome game.Outcome) (json.RawMessage, error) {
	merged := map[string]json.RawMessage{}
	if len(caller) > 0 {
		if err := json.Unmarshal(caller, &merged); err != nil {
			return nil, fmt.Errorf("%w: metadata must be a JSON object", errs.ErrInvalidAmount)
		}
	}
	delete(merged, "outcome")

	encoded, err := json.Marshal(outcome)
	if err != nil {
		return nil, fmt.Errorf("spin: encode outcome: %w", err)
	}
	merged["outcome"] = encoded

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("spin: encode metadata: %w", err)
	}
	// ledger_transactions caps request_metadata at 512 bytes. Fail here with
	// a clean 400 rather than letting the DB CHECK abort the transaction.
	if len(out) > 512 {
		return nil, fmt.Errorf("%w: metadata too large (%d bytes, max 512)", errs.ErrInvalidAmount, len(out))
	}
	return out, nil
}

func decodeCachedSpin(payload string) (SpinResult, error) {
	var out SpinResult
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return SpinResult{}, fmt.Errorf("decode cached spin: %w", err)
	}
	out.Status = StatusCached
	return out, nil
}

func (e *engine) cacheSpinQuietly(ctx context.Context, idemKey, fingerprint string, result SpinResult) {
	b, err := json.Marshal(result)
	if err != nil {
		e.logger.ErrorContext(ctx, "spin idempotency payload marshal",
			slog.String("error", err.Error()), slog.String("idempotency_key", idemKey))
		return
	}
	if err := e.idem.Store(ctx, idemKey, fingerprint, string(b)); err != nil {
		e.logger.WarnContext(ctx, "spin idempotency cache store",
			slog.String("error", err.Error()), slog.String("idempotency_key", idemKey))
	}
}
