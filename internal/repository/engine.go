// Package repository implements the wallet engine: it sequences Redis
// idempotency, pessimistic SELECT FOR UPDATE locking, the pure domain
// allocators, and double-entry ledger writes into a single tight flow
// per operator transaction.
//
// All exported methods take context.Context first and propagate it to
// every Redis and pgx call. No method touches a global pool — Engine is
// constructed via New() with explicit dependencies (cursor rule §7).
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Gavrielsh/True/internal/cache"
	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/metrics"
	"github.com/Gavrielsh/True/internal/telemetry"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// DB is the minimal pgxpool surface the engine consumes. Defined here so
// production wires *pgxpool.Pool directly and tests inject pgxmock without
// either side knowing about the other.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// engine is the concrete implementation. The unexported name keeps the
// struct out of consumer-facing API; callers depend on the Engine interface.
type engine struct {
	db     DB
	idem   cache.Store
	logger *slog.Logger
	// maxWin is the absolute ceiling on a single THIRD-PARTY win credit.
	// Zero means unbounded (the pre-audit behaviour) and is rejected at boot
	// by cmd/engine — see Option below.
	maxWin domain.Money
}

// Option configures the engine. Variadic so existing three-argument callers
// (including every test double) keep compiling.
type Option func(*engine)

// WithMaxWinAmount caps any single WIN credit accepted from a third-party
// aggregator.
//
// WHY: /win exists for certified external providers that generate their own
// outcomes, so the engine cannot re-derive the payout the way /spin does.
// Without a ceiling, one leaked provider HMAC secret mints unlimited
// SC_REDEEMABLE — the finding this option closes. The ceiling is a blunt
// backstop, not a game-math control: set it above the highest legitimate
// max-win across your provider catalogue and alert on every rejection,
// because a breach means either a provider bug or a compromised secret.
//
// First-party rounds through /spin do not consult this value — their payout
// is already bounded by the paytable's MaxWinMultiplier.
func WithMaxWinAmount(m domain.Money) Option {
	return func(e *engine) { e.maxWin = m }
}

// New constructs the engine from explicit dependencies. Pass a *pgxpool.Pool
// (or any other DB) and a *cache.Redis (or any other Store). nil logger is
// replaced with slog.Default to keep call sites short.
func New(db DB, idem cache.Store, logger *slog.Logger, opts ...Option) Engine {
	if logger == nil {
		logger = slog.Default()
	}
	e := &engine{db: db, idem: idem, logger: logger}
	for _, o := range opts {
		o(e)
	}
	return e
}

// checkWinCeiling enforces the third-party win cap. A zero ceiling disables
// the check (and is refused at boot in production wiring).
func (e *engine) checkWinCeiling(amount domain.Money) error {
	if !e.maxWin.IsPositive() {
		return nil
	}
	if amount.GreaterThan(e.maxWin) {
		metrics.WinCeilingRejections.Inc()
		return fmt.Errorf("%w: win %s exceeds configured ceiling %s",
			errs.ErrWinExceedsCeiling, amount, e.maxWin)
	}
	return nil
}

// idempotencyKey scopes the operator_transaction_id by operator_code so that
// two independent aggregators can never collide on the same string.
func idempotencyKey(operatorCode, opTxID string) string {
	return operatorCode + ":" + opTxID
}

// requestFingerprint binds an idempotency key to WHO is transacting and to
// WHAT they sent. Stored beside the cached payload; a retry whose fingerprint
// differs is refused with ErrIdempotencyMismatch instead of being served the
// original request's result.
//
// bodyHash is the hex SHA-256 of the verified raw body, supplied by the HMAC
// middleware. When it is empty (internal callers, tests) the key is still
// bound to the player, so the cross-player disclosure the audit found cannot
// occur either way.
func requestFingerprint(playerID uuid.UUID, bodyHash string) string {
	return cache.Fingerprint(playerID.String(), bodyHash)
}

// mapIdempotencyErr converts the cache layer's mismatch sentinel into the
// domain error that maps to HTTP 409.
func mapIdempotencyErr(operatorCode string, err error) error {
	if errors.Is(err, cache.ErrFingerprintMismatch) {
		metrics.IdempotencyFingerprintMismatch.WithLabelValues(operatorCode).Inc()
		return fmt.Errorf("%w", errs.ErrIdempotencyMismatch)
	}
	return fmt.Errorf("idempotency acquire: %w", err)
}

// ----------------------------------------------------------------------------
// GetBalances — non-locking snapshot read.
// ----------------------------------------------------------------------------

func (e *engine) GetBalances(ctx context.Context, playerID uuid.UUID) (domain.Wallet, error) {
	if playerID == uuid.Nil {
		return domain.Wallet{}, fmt.Errorf("%w: nil player id", errs.ErrPlayerNotFound)
	}
	var gc, scU, scR decimal.Decimal
	err := e.db.QueryRow(ctx, sqlSelectWalletBalances, playerID).Scan(&gc, &scU, &scR)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, errs.ErrPlayerNotFound
	}
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("select wallet: %w", err)
	}
	w, err := walletFromDecimals(playerID, gc, scU, scR)
	if err != nil {
		return domain.Wallet{}, err
	}
	return w, nil
}

// ----------------------------------------------------------------------------
// ProcessBet — the canonical golden flow.
// ----------------------------------------------------------------------------

func (e *engine) ProcessBet(ctx context.Context, req BetRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	// ---- Phase 1: Redis idempotency ----
	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		// FAIL CLOSED — never proceed when the idempotency barrier is down.
		// A fingerprint mismatch is a CLIENT error (409), not a barrier fault.
		return TxResult{}, mapIdempotencyErr(req.OperatorCode, err)
	}
	//nolint:exhaustive // StatusAcquired and StatusUnknown deliberately fall THROUGH
	// this switch: acquiring the barrier is the success path and continues into the DB
	// phase below, and an unknown status is handled by mapIdempotencyErr above. Adding
	// cases that merely break would be dead code stating the opposite of the design.
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	// ---- Phase 2: DB transaction ----
	result, err := e.processBetTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	// ---- Phase 3: cache the response ----
	e.cacheResultQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (e *engine) processBetTx(ctx context.Context, req BetRequest) (result TxResult, err error) {
	defer metrics.ObserveDBLockDuration(metrics.OpBet, time.Now())
	// §9: player_id is a UUID (not PII); the amount is deliberately omitted.
	ctx, span := telemetry.StartSpan(ctx, "db.bet_tx",
		attribute.String("player_id", req.PlayerID.String()))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	// Rollback after Commit is a no-op in pgx — safe as an unconditional defer.
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. SELECT ... FOR UPDATE  — lock the wallet row for the duration of the tx.
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 1b. KYC/compliance guard — same tx handle, serialized by the lock above.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}

	// 2. Pure domain math — runs entirely in CPU, no I/O. The lock window
	//    therefore stays bounded by the surrounding SQL roundtrips, not by
	//    any allocator or formatting code.
	alloc, err := wallet.AllocateBet(req.Family, req.Amount)
	if err != nil {
		return TxResult{}, err
	}
	post, err := wallet.ApplyBet(alloc)
	if err != nil {
		// Defensive: an overdraw here means the allocation didn't match the
		// locked wallet. Abort BEFORE any UPDATE so the DB constraint layer is
		// never the first line of defence.
		return TxResult{}, err
	}

	// 3. UPDATE wallets to the post-state.
	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	// 4. INSERT ledger_transactions — the idempotency anchor.
	//    Catch 23505 here and switch to Ghost-Spin recovery.
	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "BET",
		GameID:                req.GameID,
		RoundID:               req.RoundID,
		Reference:             uuid.Nil,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Stale FOR UPDATE rows are released by the deferred Rollback.
			// Then read the committed state to reconstruct the response.
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID, req.PlayerID, "BET", req.Family, req.Amount, req.BodyHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}
	span.SetAttributes(attribute.String("ledger_transaction_id", ledgerTxID.String()))

	// 5. INSERT ledger_entries — one debit row per Debit, plus one balancing
	//    credit row against HOUSE_BET_POOL per debit currency.
	for _, d := range alloc.Debits {
		balanceAfter := post.BalanceFor(d.Currency)
		if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, d.Currency, "DEBIT", d.Amount, balanceAfter); err != nil {
			return TxResult{}, err
		}
		if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_BET_POOL", d.Currency, "CREDIT", d.Amount); err != nil {
			return TxResult{}, err
		}
	}

	// 6. COMMIT — releases the FOR UPDATE lock and durably persists everything.
	if err := tx.Commit(ctx); err != nil {
		return TxResult{}, fmt.Errorf("commit: %w", err)
	}

	return TxResult{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              req.PlayerID,
		TransactionType:       "BET",
		Family:                req.Family.String(),
		Amount:                req.Amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// ----------------------------------------------------------------------------
// ProcessWin — same structural flow as ProcessBet, single credit instead of
// the bet's split debit.
// ----------------------------------------------------------------------------

func (e *engine) ProcessWin(ctx context.Context, req WinRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	// Third-party win ceiling. Checked BEFORE the idempotency barrier so an
	// over-ceiling win never claims a key or reaches the database.
	if err := e.checkWinCeiling(req.Amount); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		return TxResult{}, mapIdempotencyErr(req.OperatorCode, err)
	}
	//nolint:exhaustive // StatusAcquired and StatusUnknown deliberately fall THROUGH
	// this switch: acquiring the barrier is the success path and continues into the DB
	// phase below, and an unknown status is handled by mapIdempotencyErr above. Adding
	// cases that merely break would be dead code stating the opposite of the design.
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	result, err := e.processWinTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (e *engine) processWinTx(ctx context.Context, req WinRequest) (result TxResult, err error) {
	defer metrics.ObserveDBLockDuration(metrics.OpWin, time.Now())
	ctx, span := telemetry.StartSpan(ctx, "db.win_tx",
		attribute.String("player_id", req.PlayerID.String()))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// KYC/compliance guard — same tx handle, serialized by the lock above.
	if err := requirePlayerActive(ctx, tx, req.PlayerID); err != nil {
		return TxResult{}, err
	}

	credit, err := wallet.AllocateWin(req.Family, req.Amount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyWin(credit)

	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "WIN",
		GameID:                req.GameID,
		RoundID:               req.RoundID,
		Reference:             req.ReferenceTransactionID,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID, req.PlayerID, "WIN", req.Family, req.Amount, req.BodyHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}
	span.SetAttributes(attribute.String("ledger_transaction_id", ledgerTxID.String()))

	// One credit row for the player, one balancing debit against HOUSE_WIN_POOL.
	balanceAfter := post.BalanceFor(credit.Credit.Currency)
	if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, credit.Credit.Currency, "CREDIT", credit.Credit.Amount, balanceAfter); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_WIN_POOL", credit.Credit.Currency, "DEBIT", credit.Credit.Amount); err != nil {
		return TxResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TxResult{}, fmt.Errorf("commit: %w", err)
	}

	return TxResult{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              req.PlayerID,
		TransactionType:       "WIN",
		Family:                req.Family.String(),
		Amount:                req.Amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// ----------------------------------------------------------------------------
// ProcessRollback — reverses a previously-committed BET.
// ----------------------------------------------------------------------------

func (e *engine) ProcessRollback(ctx context.Context, req RollbackRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	fp := requestFingerprint(req.PlayerID, req.BodyHash)

	status, payload, err := e.idem.Acquire(ctx, idemKey, fp)
	if err != nil {
		return TxResult{}, mapIdempotencyErr(req.OperatorCode, err)
	}
	//nolint:exhaustive // StatusAcquired and StatusUnknown deliberately fall THROUGH
	// this switch: acquiring the barrier is the success path and continues into the DB
	// phase below, and an unknown status is handled by mapIdempotencyErr above. Adding
	// cases that merely break would be dead code stating the opposite of the design.
	switch status {
	case cache.StatusPending:
		return TxResult{}, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		return decodeCached(payload, StatusCached)
	}

	result, err := e.processRollbackTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, fp, result)
	return result, nil
}

func (e *engine) processRollbackTx(ctx context.Context, req RollbackRequest) (result TxResult, err error) {
	defer metrics.ObserveDBLockDuration(metrics.OpRollback, time.Now())
	ctx, span := telemetry.StartSpan(ctx, "db.rollback_tx",
		attribute.String("player_id", req.PlayerID.String()))
	defer func() { telemetry.EndSpan(span, err) }()

	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the wallet FIRST (consistent lock order with bet/win — avoids
	//    deadlocks if a /bet for the same player arrives concurrently).
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 2. Read the original tx header. The ledger is STRICTLY APPEND-ONLY, so
	//    this row is immutable and needs no row lock: the wallets FOR UPDATE
	//    above already serializes every rollback of this player's BET (they all
	//    target the same player row), making the guard in step 3 race-free.
	var (
		origPlayerID uuid.UUID
		origType     string
	)
	err = tx.QueryRow(ctx, sqlSelectLedgerTxByID, req.ReferenceTransactionID).
		Scan(&origPlayerID, &origType)
	if errors.Is(err, pgx.ErrNoRows) {
		return TxResult{}, errs.ErrRollbackNotFound
	}
	if err != nil {
		return TxResult{}, fmt.Errorf("lookup original: %w", err)
	}
	if origPlayerID != req.PlayerID {
		return TxResult{}, fmt.Errorf("%w: rollback player_id %s != original %s",
			errs.ErrTransactionConflict, req.PlayerID, origPlayerID)
	}
	if origType != "BET" {
		return TxResult{}, fmt.Errorf("%w: original type=%s, only BET supported",
			errs.ErrRollbackUnsupported, origType)
	}

	// 3. APPEND-ONLY double-rollback guard. Rather than mutating the original
	//    row's status to ROLLED_BACK (forbidden in an append-only ledger), we
	//    detect a prior reversal by the EXISTENCE of a ROLLBACK transaction that
	//    already references this BET. The wallet lock above makes the read
	//    race-free. A genuine retry of THIS rollback (same operator_transaction_
	//    id) is excluded by the query and falls through to the dedup INSERT,
	//    where Ghost-Spin recovery replays the original success instead of
	//    erroring — true idempotency.
	var alreadyRolledBack bool
	if err := tx.QueryRow(ctx, sqlRollbackExistsForReference,
		req.ReferenceTransactionID, req.OperatorCode, req.OperatorTransactionID).
		Scan(&alreadyRolledBack); err != nil {
		return TxResult{}, fmt.Errorf("rollback-exists check: %w", err)
	}
	if alreadyRolledBack {
		return TxResult{}, errs.ErrRollbackAlready
	}

	// 4. Fetch the original BET's player-wallet entries (1-2 rows, ordered by currency).
	entries, err := fetchPlayerEntries(ctx, tx, req.ReferenceTransactionID)
	if err != nil {
		return TxResult{}, err
	}
	if len(entries) == 0 {
		// Shouldn't happen — a COMPLETED BET always wrote PLAYER_WALLET rows.
		return TxResult{}, fmt.Errorf("%w: original tx has no PLAYER_WALLET entries",
			errs.ErrTransactionConflict)
	}

	// 5. Reverse the wallet balances. Each original DEBIT becomes a re-credit.
	post := wallet
	for _, ent := range entries {
		if ent.Direction != "DEBIT" {
			return TxResult{}, fmt.Errorf("%w: unexpected %s entry in BET",
				errs.ErrTransactionConflict, ent.Direction)
		}
		switch ent.Currency {
		case domain.CurrencyGC:
			post.GC = post.GC.Add(ent.Amount)
		case domain.CurrencySCUnplayed:
			post.SCUnplayed = post.SCUnplayed.Add(ent.Amount)
		case domain.CurrencySCRedeemable:
			post.SCRedeemable = post.SCRedeemable.Add(ent.Amount)
		default:
			return TxResult{}, fmt.Errorf("%w: unknown currency %s",
				errs.ErrUnsupportedCurrency, ent.Currency)
		}
	}

	// 6. UPDATE wallets to the restored balances. The wallets table is the
	//    materialized-view CACHE (architecture §3), not the ledger — mutating
	//    it is required; the append-only invariant applies to ledger_* only.
	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	// 7. INSERT the ROLLBACK ledger_transactions header (append-only). Its
	//    reference_transaction_id is what makes this BET show as rolled-back to
	//    every future reader (and to the step-3 guard) — no UPDATE required.
	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "ROLLBACK",
		Reference:             req.ReferenceTransactionID,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "ROLLBACK", domain.FamilyUnknown, domain.ZeroMoney(), req.BodyHash)
		}
		return TxResult{}, fmt.Errorf("insert rollback tx: %w", err)
	}
	span.SetAttributes(attribute.String("ledger_transaction_id", ledgerTxID.String()))

	// 8. INSERT reverse ledger_entries — one CREDIT per original DEBIT.
	for _, ent := range entries {
		balanceAfter := post.BalanceFor(ent.Currency)
		if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID,
			ent.Currency, "CREDIT", ent.Amount, balanceAfter); err != nil {
			return TxResult{}, err
		}
		if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_BET_POOL",
			ent.Currency, "DEBIT", ent.Amount); err != nil {
			return TxResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return TxResult{}, fmt.Errorf("commit: %w", err)
	}

	// Compute total reversed amount for the response.
	total := domain.ZeroMoney()
	for _, e := range entries {
		total = total.Add(e.Amount)
	}

	return TxResult{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              req.PlayerID,
		TransactionType:       "ROLLBACK",
		Family:                "", // multi-currency aggregate; left empty
		Amount:                total,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

type playerEntry struct {
	Currency  domain.Currency
	Direction string
	Amount    domain.Money
}

func fetchPlayerEntries(ctx context.Context, tx pgx.Tx, refTxID uuid.UUID) ([]playerEntry, error) {
	rows, err := tx.Query(ctx, sqlSelectPlayerEntriesByTx, refTxID)
	if err != nil {
		return nil, fmt.Errorf("fetch entries: %w", err)
	}
	defer rows.Close()

	var out []playerEntry
	for rows.Next() {
		var (
			cur     domain.Currency
			dir     string
			amountD decimal.Decimal
		)
		if err := rows.Scan(&cur, &dir, &amountD); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		amount, err := domain.NewMoney(amountD)
		if err != nil {
			return nil, fmt.Errorf("scan amount: %w", err)
		}
		out = append(out, playerEntry{Currency: cur, Direction: dir, Amount: amount})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entry rows: %w", err)
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// Ghost-Spin recovery (architecture §6.A).
//
// Reached when INSERT into ledger_transactions returns 23505 (unique_violation)
// on (operator_code, operator_transaction_id). The previous attempt's DB
// transaction committed but its Redis idempotency cache write was lost (e.g.
// network drop). Reconstruct the success payload from the current DB state,
// re-cache it, and return success — DO NOT re-deduct funds.
// ----------------------------------------------------------------------------

func (e *engine) recoverGhostSpin(
	ctx context.Context,
	operatorCode, opTxID string,
	playerID uuid.UUID,
	txType string,
	family domain.CurrencyFamily,
	amount domain.Money,
	bodyHash string,
) (TxResult, error) {
	var (
		ledgerTxID   uuid.UUID
		storedPlayer uuid.UUID
		storedTxType string
	)
	err := e.db.QueryRow(ctx, sqlSelectLedgerTxByOperator, operatorCode, opTxID).
		Scan(&ledgerTxID, &storedPlayer, &storedTxType)
	if errors.Is(err, pgx.ErrNoRows) {
		// The 23505 came from somewhere else — should be impossible, but
		// surface as conflict rather than masking as success.
		return TxResult{}, fmt.Errorf("%w: %s/%s vanished post-conflict",
			errs.ErrTransactionConflict, operatorCode, opTxID)
	}
	if err != nil {
		return TxResult{}, fmt.Errorf("ghost lookup: %w", err)
	}
	if storedPlayer != playerID {
		// Same operator_transaction_id but different player — the operator
		// is reusing transaction IDs across players. That's a contract
		// violation and we cannot safely replay.
		return TxResult{}, fmt.Errorf("%w: tx id reuse across players", errs.ErrTransactionConflict)
	}
	if storedTxType != txType {
		return TxResult{}, fmt.Errorf("%w: tx %s stored as %s, retried as %s",
			errs.ErrTransactionConflict, opTxID, storedTxType, txType)
	}

	// Read CURRENT wallet state (architecture §6.A explicitly directs this).
	// May not equal the post-state of the original tx if other tx have
	// landed since — but the operator's receipt is still accurate as a
	// "tx X completed; here is your wallet now".
	current, err := e.GetBalances(ctx, playerID)
	if err != nil {
		return TxResult{}, fmt.Errorf("ghost balance read: %w", err)
	}

	result := TxResult{
		OperatorCode:          operatorCode,
		OperatorTransactionID: opTxID,
		LedgerTransactionID:   ledgerTxID,
		PlayerID:              playerID,
		TransactionType:       txType,
		Family:                family.String(),
		Amount:                amount,
		PostBalances:          balanceSummaryOf(current),
		Status:                StatusGhostRecovered,
	}

	// Best-effort cache write so the next retry hits the fast Cached path.
	e.cacheResultQuietly(ctx, idempotencyKey(operatorCode, opTxID),
		requestFingerprint(playerID, bodyHash), result)
	metrics.GhostSpinsRecovered.Inc()
	return result, nil
}

// ----------------------------------------------------------------------------
// SQL helpers — keep the orchestration loop above readable.
// ----------------------------------------------------------------------------

func selectWalletForUpdate(ctx context.Context, tx pgx.Tx, playerID uuid.UUID) (domain.Wallet, error) {
	var gc, scU, scR decimal.Decimal
	err := tx.QueryRow(ctx, sqlSelectWalletForUpdate, playerID).Scan(&gc, &scU, &scR)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, errs.ErrPlayerNotFound
	}
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("select for update: %w", err)
	}
	return walletFromDecimals(playerID, gc, scU, scR)
}

// requirePlayerActive is the KYC/compliance guard: only ACTIVE players may
// move money. It MUST run on the same tx handle, after selectWalletForUpdate,
// so the check is serialized with concurrent status changes by the wallet
// row lock — never on a fresh pool connection, which could read a snapshot
// unordered with respect to this player's queued transactions.
func requirePlayerActive(ctx context.Context, tx pgx.Tx, playerID uuid.UUID) error {
	var status string
	err := tx.QueryRow(ctx, sqlSelectPlayerStatus, playerID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Wallet row exists but the user row doesn't — a provisioning bug, but
		// to the operator the player simply isn't playable.
		return errs.ErrPlayerNotFound
	}
	if err != nil {
		return fmt.Errorf("select player status: %w", err)
	}
	if status != "ACTIVE" {
		return fmt.Errorf("%w: player status is %s", errs.ErrPlayerNotActive, status)
	}
	return nil
}

func updateWalletBalances(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, post domain.Wallet) error {
	tag, err := tx.Exec(ctx, sqlUpdateWallet,
		post.GC.Decimal(),
		post.SCUnplayed.Decimal(),
		post.SCRedeemable.Decimal(),
		playerID,
	)
	if err != nil {
		return fmt.Errorf("update wallet: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// The FOR UPDATE earlier guarantees the row exists; zero rows here
		// means a schema/route bug. Fail closed.
		return fmt.Errorf("update wallet: expected 1 row, got %d", tag.RowsAffected())
	}
	return nil
}

// ledgerTxParams bundles the INSERT arguments — keeping insertLedgerTx's
// signature stable across BET / WIN / future ROLLBACK paths.
type ledgerTxParams struct {
	OperatorCode          string
	OperatorTransactionID string
	PlayerID              uuid.UUID
	Type                  string
	GameID                string
	RoundID               string
	Reference             uuid.UUID
	Metadata              json.RawMessage
}

func insertLedgerTx(ctx context.Context, tx pgx.Tx, p ledgerTxParams) (uuid.UUID, error) {
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, sqlInsertLedgerTx,
		p.OperatorCode,
		p.OperatorTransactionID,
		p.PlayerID,
		p.Type,
		nullableString(p.GameID),
		nullableString(p.RoundID),
		nullableUUID(p.Reference),
		metadata,
	).Scan(&id)
	if err != nil {
		// May still be 23505 from the partition-LOCAL composite unique on a
		// same-instant retry; the caller treats any unique violation as a
		// Ghost-Spin and recovers.
		return id, err
	}

	// GLOBAL idempotency anchor (migration 000005). ledger_transactions is
	// partitioned by created_at, so its unique is only partition-local; this
	// non-partitioned dedup row is what makes a duplicate operator transaction
	// collide no matter when the retry arrives. A 23505 here is THE durable
	// Ghost-Spin signal — caller distinguishes it from other failures.
	_, err = tx.Exec(ctx, sqlInsertLedgerTxDedup, p.OperatorCode, p.OperatorTransactionID, id)
	return id, err // raw — caller distinguishes 23505 vs other failures.
}

func insertPlayerWalletEntry(
	ctx context.Context, tx pgx.Tx,
	ledgerTxID, playerID uuid.UUID,
	currency domain.Currency,
	direction string,
	amount, balanceAfter domain.Money,
) error {
	_, err := tx.Exec(ctx, sqlInsertLedgerEntry,
		ledgerTxID,
		playerID,
		"PLAYER_WALLET",
		string(currency),
		direction,
		amount.Decimal(),
		balanceAfter.Decimal(),
	)
	if err != nil {
		return fmt.Errorf("insert player entry: %w", err)
	}
	return nil
}

func insertHouseEntry(
	ctx context.Context, tx pgx.Tx,
	ledgerTxID uuid.UUID,
	accountType string,
	currency domain.Currency,
	direction string,
	amount domain.Money,
) error {
	_, err := tx.Exec(ctx, sqlInsertLedgerEntry,
		ledgerTxID,
		nil, // HOUSE_* rows carry no player_id (CHECK constraint)
		accountType,
		string(currency),
		direction,
		amount.Decimal(),
		nil, // HOUSE_* rows carry no balance_after (CHECK constraint)
	)
	if err != nil {
		return fmt.Errorf("insert house entry: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Small utilities.
// ----------------------------------------------------------------------------

func walletFromDecimals(playerID uuid.UUID, gc, scU, scR decimal.Decimal) (domain.Wallet, error) {
	gcM, err := domain.NewMoney(gc)
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("scan gc: %w", err)
	}
	uM, err := domain.NewMoney(scU)
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("scan sc_unplayed: %w", err)
	}
	rM, err := domain.NewMoney(scR)
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("scan sc_redeemable: %w", err)
	}
	return domain.Wallet{
		PlayerID:     playerID.String(),
		GC:           gcM,
		SCUnplayed:   uM,
		SCRedeemable: rM,
	}, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

func decodeCached(payload string, replayStatus ResultStatus) (TxResult, error) {
	var out TxResult
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return TxResult{}, fmt.Errorf("decode cached payload: %w", err)
	}
	out.Status = replayStatus
	return out, nil
}

// releaseQuietly best-effort cleans up the PROCESSING marker after a DB
// failure. We use a fresh, bounded context so a cancelled ctx doesn't leave
// the marker for another 10s. Errors are logged at WARN, never bubbled —
// the caller is already returning a failure.
func (e *engine) releaseQuietly(ctx context.Context, idemKey string) {
	relCtx, cancel := context.WithTimeout(detach(ctx), 2*time.Second)
	defer cancel()
	if err := e.idem.Release(relCtx, idemKey); err != nil {
		e.logger.WarnContext(ctx, "idempotency release after failure",
			slog.String("error", err.Error()),
			slog.String("idempotency_key", idemKey),
		)
	}
}

// cacheResultQuietly tries to write the success payload. Failure here is the
// "Ghost Spin" origin condition: a retry will catch 23505 and recover.
func (e *engine) cacheResultQuietly(ctx context.Context, idemKey, fingerprint string, result TxResult) {
	b, err := json.Marshal(result)
	if err != nil {
		// json.Marshal on our well-defined struct cannot fail unless one of
		// the Money fields is malformed — which would mean a wider corruption.
		e.logger.ErrorContext(ctx, "idempotency payload marshal",
			slog.String("error", err.Error()),
			slog.String("idempotency_key", idemKey),
		)
		return
	}
	if err := e.idem.Store(ctx, idemKey, fingerprint, string(b)); err != nil {
		e.logger.WarnContext(ctx, "idempotency cache store",
			slog.String("error", err.Error()),
			slog.String("idempotency_key", idemKey),
		)
	}
}

// detach returns a context that inherits ctx's values but not its
// cancellation — used for cleanup paths that must complete even if the
// caller's context has been cancelled.
func detach(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// ----------------------------------------------------------------------------
// Request validation
// ----------------------------------------------------------------------------

func (r BetRequest) validate() error {
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
	if !r.Amount.IsPositive() {
		return fmt.Errorf("%w: bet amount must be > 0", errs.ErrInvalidAmount)
	}
	return nil
}

func (r RollbackRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if r.ReferenceTransactionID == uuid.Nil {
		return fmt.Errorf("%w: nil reference_transaction_id", errs.ErrRollbackNotFound)
	}
	return nil
}

func (r WinRequest) validate() error {
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
	if !r.Amount.IsPositive() {
		return fmt.Errorf("%w: win amount must be > 0", errs.ErrInvalidAmount)
	}
	return nil
}
