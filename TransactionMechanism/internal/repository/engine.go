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
	"database/sql"
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

	"github.com/Gavrielsh/TransactionMechanism/internal/cache"
	"github.com/Gavrielsh/TransactionMechanism/internal/domain"
	errs "github.com/Gavrielsh/TransactionMechanism/pkg/errors"
)

// DB is the minimal pgxpool surface the engine consumes. Defined here so
// production wires *pgxpool.Pool directly and tests inject pgxmock without
// either side knowing about the other.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// engine is the concrete implementation. The unexported name keeps the
// struct out of consumer-facing API; callers depend on the Engine interface.
type engine struct {
	db     DB
	idem   cache.Store
	logger *slog.Logger
}

// New constructs the engine from explicit dependencies. Pass a *pgxpool.Pool
// (or any other DB) and a *cache.Redis (or any other Store). nil logger is
// replaced with slog.Default to keep call sites short.
func New(db DB, idem cache.Store, logger *slog.Logger) Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &engine{db: db, idem: idem, logger: logger}
}

// idempotencyKey scopes the operator_transaction_id by operator_code so that
// two independent aggregators can never collide on the same string.
func idempotencyKey(operatorCode, opTxID string) string {
	return operatorCode + ":" + opTxID
}

// acquireBarrier runs Phase 1 (Redis idempotency) for one operation. reqHash is
// the SHA-256 of the raw request body, compared against the hash stored under
// the key on the first attempt.
//
// When handled is true the caller MUST return (replay, err) immediately:
//   - Redis error                → fail closed (wrapped error)
//   - StatusPending              → ErrTransactionPending (409, retryable)
//   - StatusCached               → the decoded cached payload (idempotent replay)
//   - StatusMismatch             → ErrIdempotencyMismatch (409, body changed)
//
// When handled is false the lock was freshly acquired (StatusAcquired) and the
// caller proceeds to the DB transaction.
func (e *engine) acquireBarrier(ctx context.Context, idemKey, reqHash string) (TxResult, bool, error) {
	status, payload, err := e.idem.Acquire(ctx, idemKey, reqHash)
	if err != nil {
		// FAIL CLOSED — never proceed when the idempotency barrier is down.
		return TxResult{}, true, fmt.Errorf("idempotency acquire: %w", err)
	}
	switch status {
	case cache.StatusPending:
		return TxResult{}, true, fmt.Errorf("%w: %s in flight", errs.ErrTransactionPending, idemKey)
	case cache.StatusCached:
		res, derr := decodeCached(payload, StatusCached)
		return res, true, derr
	case cache.StatusMismatch:
		return TxResult{}, true, fmt.Errorf(
			"%w: %s replayed with a different request body", errs.ErrIdempotencyMismatch, idemKey)
	}
	return TxResult{}, false, nil // StatusAcquired — proceed to the DB tx.
}

// ----------------------------------------------------------------------------
// CreatePlayer — provisions user + wallet in one atomic transaction.
// ----------------------------------------------------------------------------

func (e *engine) CreatePlayer(ctx context.Context, req CreatePlayerRequest) error {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, sqlCreateUser, req.PlayerID, req.ExternalID, req.Email, req.Username)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}

	_, err = tx.Exec(ctx, sqlCreateWallet, req.PlayerID)
	if err != nil {
		return fmt.Errorf("create wallet: %w", err)
	}

	return tx.Commit(ctx)
}

// ----------------------------------------------------------------------------
// ProcessDeposit — credits wallet and posts a DEPOSIT ledger transaction.
// ----------------------------------------------------------------------------

func (e *engine) ProcessDeposit(ctx context.Context, req DepositRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	result, err := e.processDepositTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processDepositTx(ctx context.Context, req DepositRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// Route the credit to the correct currency bucket and house pool.
	var currency domain.Currency
	var housePool string
	post := wallet
	switch req.Currency {
	case domain.FamilyGC:
		post.GC = post.GC.Add(req.Amount)
		currency = domain.CurrencyGC
		housePool = "HOUSE_ADJUSTMENT_POOL"
	case domain.FamilySC:
		post.SCUnplayed = post.SCUnplayed.Add(req.Amount)
		currency = domain.CurrencySCUnplayed
		housePool = "HOUSE_PROMO_POOL"
	default:
		return TxResult{}, fmt.Errorf("%w: family=%s", errs.ErrUnsupportedCurrency, req.Currency)
	}

	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "DEPOSIT",
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "DEPOSIT", req.Currency, req.Amount, req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}

	balanceAfter := post.BalanceFor(currency)
	if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, currency, "CREDIT", req.Amount, balanceAfter); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, housePool, currency, "DEBIT", req.Amount); err != nil {
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
		TransactionType:       "DEPOSIT",
		Family:                req.Currency.String(),
		Amount:                req.Amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

func (r DepositRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if !r.Currency.Valid() {
		return fmt.Errorf("%w: currency must be GC or SC", errs.ErrUnsupportedCurrency)
	}
	if !r.Amount.IsPositive() {
		return fmt.Errorf("%w: deposit amount must be > 0", errs.ErrInvalidAmount)
	}
	return nil
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

	// ---- Phase 1: Redis idempotency (with request-body hash verification) ----
	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	// ---- Phase 2: DB transaction ----
	result, err := e.processBetTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	// ---- Phase 3: cache the response ----
	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processBetTx(ctx context.Context, req BetRequest) (TxResult, error) {
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

	// 2. Pure domain math — runs entirely in CPU, no I/O. The lock window
	//    therefore stays bounded by the surrounding SQL roundtrips, not by
	//    any allocator or formatting code.
	alloc, err := wallet.AllocateBet(req.Family, req.Amount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyBet(alloc)

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
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID, req.PlayerID, "BET", req.Family, req.Amount, req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}

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
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	result, err := e.processWinTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processWinTx(ctx context.Context, req WinRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
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
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID, req.PlayerID, "WIN", req.Family, req.Amount, req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert ledger tx: %w", err)
	}

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

	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	result, err := e.processRollbackTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processRollbackTx(ctx context.Context, req RollbackRequest) (TxResult, error) {
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

	// 2. Lock the original tx row. The FOR UPDATE here serializes two
	//    concurrent rollbacks of the same reference, so only one wins.
	var (
		origPlayerID uuid.UUID
		origType     string
		origStatus   string
	)
	err = tx.QueryRow(ctx, sqlSelectLedgerTxByIDForUpdate, req.ReferenceTransactionID).
		Scan(&origPlayerID, &origType, &origStatus)
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
	if origStatus == "ROLLED_BACK" {
		return TxResult{}, errs.ErrRollbackAlready
	}
	if origStatus != "COMPLETED" {
		return TxResult{}, fmt.Errorf("%w: original status=%s, expected COMPLETED",
			errs.ErrTransactionConflict, origStatus)
	}
	if origType != "BET" {
		return TxResult{}, fmt.Errorf("%w: original type=%s, only BET supported",
			errs.ErrRollbackUnsupported, origType)
	}

	// 3. Fetch the original BET's player-wallet entries (1-2 rows, ordered by currency).
	entries, err := fetchPlayerEntries(ctx, tx, req.ReferenceTransactionID)
	if err != nil {
		return TxResult{}, err
	}
	if len(entries) == 0 {
		// Shouldn't happen — a COMPLETED BET always wrote PLAYER_WALLET rows.
		return TxResult{}, fmt.Errorf("%w: original tx has no PLAYER_WALLET entries",
			errs.ErrTransactionConflict)
	}

	// 4. Reverse the wallet balances. Each original DEBIT becomes a re-credit.
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

	// 5. UPDATE wallets to the restored balances.
	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	// 6. Flip the original tx's status. RowsAffected==0 would mean a concurrent
	//    rollback raced us, but the FOR UPDATE earlier prevented that — treat
	//    zero as a bug, not a race.
	tag, err := tx.Exec(ctx, sqlMarkTxRolledBack, req.ReferenceTransactionID)
	if err != nil {
		return TxResult{}, fmt.Errorf("mark original rolled-back: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return TxResult{}, fmt.Errorf("%w: mark rolled-back affected %d rows",
			errs.ErrTransactionConflict, tag.RowsAffected())
	}

	// 7. INSERT the ROLLBACK ledger_transactions header.
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
				req.PlayerID, "ROLLBACK", domain.FamilyUnknown, domain.ZeroMoney(), req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert rollback tx: %w", err)
	}

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
// Escrow — withdrawal double-spend fix.
//
// A withdrawal first RESERVES the SC_REDEEMABLE (deducting it from the wallet
// and parking it in HOUSE_ESCROW_POOL) so the player cannot gamble funds that
// are pending payout. Ops later COMMIT (burn → HOUSE_WITHDRAWAL_POOL) or
// RELEASE (return to the player). All three share the Redis idempotency barrier
// and the 23505 Ghost-Spin recovery used by bet/win/rollback.
// ----------------------------------------------------------------------------

// ProcessEscrowReserve — Phase 1 idempotency → reserve DB tx → cache response.
func (e *engine) ProcessEscrowReserve(ctx context.Context, req EscrowReserveRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	result, err := e.processEscrowReserveTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processEscrowReserveTx(ctx context.Context, req EscrowReserveRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the wallet — same window as bet/win.
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 2. Pure domain math — SC_REDEEMABLE only.
	res, err := wallet.AllocateEscrowReserve(req.Amount)
	if err != nil {
		return TxResult{}, err
	}
	post := wallet.ApplyEscrowReserve(res)

	// 3. UPDATE wallet to the post-state (funds now locked).
	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	// 4. INSERT the PENDING ESCROW_RESERVE header (idempotency anchor).
	ledgerTxID, err := insertEscrowReserveTx(ctx, tx, req.OperatorCode, req.OperatorTransactionID, req.PlayerID, req.Metadata)
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "ESCROW_RESERVE", domain.FamilySC, req.Amount, req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert escrow reserve tx: %w", err)
	}

	// 5. Double-entry: player DEBIT SC_REDEEMABLE / HOUSE_ESCROW_POOL CREDIT.
	balanceAfter := post.BalanceFor(domain.CurrencySCRedeemable)
	if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, domain.CurrencySCRedeemable, "DEBIT", req.Amount, balanceAfter); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_ESCROW_POOL", domain.CurrencySCRedeemable, "CREDIT", req.Amount); err != nil {
		return TxResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TxResult{}, fmt.Errorf("commit: %w", err)
	}

	return TxResult{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		LedgerTransactionID:   ledgerTxID, // == escrow_transaction_id
		PlayerID:              req.PlayerID,
		TransactionType:       "ESCROW_RESERVE",
		Family:                domain.FamilySC.String(),
		Amount:                req.Amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// ProcessEscrowCommit — finalises an approved withdrawal (burns the escrow).
func (e *engine) ProcessEscrowCommit(ctx context.Context, req EscrowCommitRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	result, err := e.processEscrowCommitTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processEscrowCommitTx(ctx context.Context, req EscrowCommitRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the wallet FIRST (consistent lock order with release/rollback;
	//    a commit does not mutate the wallet but holding the lock serializes a
	//    concurrent release of the same player and prevents lock-order deadlock).
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 2. Lock + validate the reserve. FOR UPDATE here is what serializes commit
	//    vs release: only one can observe PENDING and flip it.
	amount, err := lockEscrowReserve(ctx, tx, req.EscrowTransactionID, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 3. Flip the reserve PENDING→COMPLETED. The WHERE-clause guard makes a lost
	//    race a no-op; we hold FOR UPDATE so exactly 1 row must change.
	tag, err := tx.Exec(ctx, sqlMarkEscrowCommitted, req.EscrowTransactionID)
	if err != nil {
		return TxResult{}, fmt.Errorf("mark escrow committed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return TxResult{}, fmt.Errorf("%w: commit affected %d rows", errs.ErrEscrowConflict, tag.RowsAffected())
	}

	// 4. INSERT the ESCROW_COMMIT header (references the reserve).
	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "ESCROW_COMMIT",
		Reference:             req.EscrowTransactionID,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "ESCROW_COMMIT", domain.FamilySC, amount, req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert escrow commit tx: %w", err)
	}

	// 5. Burn the escrow: HOUSE_ESCROW_POOL DEBIT / HOUSE_WITHDRAWAL_POOL CREDIT.
	//    No player entry — the player's balance changed at reserve time.
	if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_ESCROW_POOL", domain.CurrencySCRedeemable, "DEBIT", amount); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_WITHDRAWAL_POOL", domain.CurrencySCRedeemable, "CREDIT", amount); err != nil {
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
		TransactionType:       "ESCROW_COMMIT",
		Family:                domain.FamilySC.String(),
		Amount:                amount,
		PostBalances:          balanceSummaryOf(wallet), // unchanged by a commit
		Status:                StatusProcessed,
	}, nil
}

// ProcessEscrowRelease — reverses a rejected withdrawal (returns the funds).
func (e *engine) ProcessEscrowRelease(ctx context.Context, req EscrowReleaseRequest) (TxResult, error) {
	if err := req.validate(); err != nil {
		return TxResult{}, err
	}
	idemKey := idempotencyKey(req.OperatorCode, req.OperatorTransactionID)

	if replay, handled, err := e.acquireBarrier(ctx, idemKey, req.RequestHash); handled {
		return replay, err
	}

	result, err := e.processEscrowReleaseTx(ctx, req)
	if err != nil {
		e.releaseQuietly(ctx, idemKey)
		return TxResult{}, err
	}

	e.cacheResultQuietly(ctx, idemKey, result, req.RequestHash)
	return result, nil
}

func (e *engine) processEscrowReleaseTx(ctx context.Context, req EscrowReleaseRequest) (TxResult, error) {
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the wallet (we DO mutate it: the funds come back).
	wallet, err := selectWalletForUpdate(ctx, tx, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 2. Lock + validate the reserve (serializes against a concurrent commit).
	amount, err := lockEscrowReserve(ctx, tx, req.EscrowTransactionID, req.PlayerID)
	if err != nil {
		return TxResult{}, err
	}

	// 3. Return the funds to SC_REDEEMABLE.
	post := wallet.ApplyEscrowRelease(amount)
	if err := updateWalletBalances(ctx, tx, req.PlayerID, post); err != nil {
		return TxResult{}, err
	}

	// 4. Flip the reserve PENDING→ROLLED_BACK.
	tag, err := tx.Exec(ctx, sqlMarkEscrowReleased, req.EscrowTransactionID)
	if err != nil {
		return TxResult{}, fmt.Errorf("mark escrow released: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return TxResult{}, fmt.Errorf("%w: release affected %d rows", errs.ErrEscrowConflict, tag.RowsAffected())
	}

	// 5. INSERT the ESCROW_RELEASE header (references the reserve).
	ledgerTxID, err := insertLedgerTx(ctx, tx, ledgerTxParams{
		OperatorCode:          req.OperatorCode,
		OperatorTransactionID: req.OperatorTransactionID,
		PlayerID:              req.PlayerID,
		Type:                  "ESCROW_RELEASE",
		Reference:             req.EscrowTransactionID,
		Metadata:              req.Metadata,
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return e.recoverGhostSpin(ctx, req.OperatorCode, req.OperatorTransactionID,
				req.PlayerID, "ESCROW_RELEASE", domain.FamilySC, amount, req.RequestHash)
		}
		return TxResult{}, fmt.Errorf("insert escrow release tx: %w", err)
	}

	// 6. Reverse the reserve's lines: player CREDIT / HOUSE_ESCROW_POOL DEBIT.
	balanceAfter := post.BalanceFor(domain.CurrencySCRedeemable)
	if err := insertPlayerWalletEntry(ctx, tx, ledgerTxID, req.PlayerID, domain.CurrencySCRedeemable, "CREDIT", amount, balanceAfter); err != nil {
		return TxResult{}, err
	}
	if err := insertHouseEntry(ctx, tx, ledgerTxID, "HOUSE_ESCROW_POOL", domain.CurrencySCRedeemable, "DEBIT", amount); err != nil {
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
		TransactionType:       "ESCROW_RELEASE",
		Family:                domain.FamilySC.String(),
		Amount:                amount,
		PostBalances:          balanceSummaryOf(post),
		Status:                StatusProcessed,
	}, nil
}

// lockEscrowReserve locks the reserve ledger_transactions row FOR UPDATE and
// validates it is a PENDING ESCROW_RESERVE owned by playerID. It returns the
// single reserved amount, read from the reserve's PLAYER_WALLET debit entry.
//
// This FOR UPDATE is the crux of the double-spend guard: a reserve can be
// observed PENDING by at most one of {commit, release}; the loser sees a
// non-PENDING status and is rejected with ErrEscrowConflict.
func lockEscrowReserve(ctx context.Context, tx pgx.Tx, escrowTxID, playerID uuid.UUID) (domain.Money, error) {
	var (
		origPlayerID uuid.UUID
		origType     string
		origStatus   string
	)
	err := tx.QueryRow(ctx, sqlSelectLedgerTxByIDForUpdate, escrowTxID).
		Scan(&origPlayerID, &origType, &origStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ZeroMoney(), errs.ErrEscrowNotFound
	}
	if err != nil {
		return domain.ZeroMoney(), fmt.Errorf("lookup escrow reserve: %w", err)
	}
	if origType != "ESCROW_RESERVE" {
		return domain.ZeroMoney(), fmt.Errorf("%w: tx %s is %s, not ESCROW_RESERVE",
			errs.ErrEscrowConflict, escrowTxID, origType)
	}
	if origPlayerID != playerID {
		return domain.ZeroMoney(), fmt.Errorf("%w: escrow %s player mismatch",
			errs.ErrTransactionConflict, escrowTxID)
	}
	if origStatus != "PENDING" {
		// Already committed (COMPLETED) or released (ROLLED_BACK): the funds are
		// spoken for. Refusing here is what stops a double payout / double refund.
		return domain.ZeroMoney(), fmt.Errorf("%w: escrow %s status=%s",
			errs.ErrEscrowConflict, escrowTxID, origStatus)
	}

	entries, err := fetchPlayerEntries(ctx, tx, escrowTxID)
	if err != nil {
		return domain.ZeroMoney(), err
	}
	if len(entries) != 1 || entries[0].Currency != domain.CurrencySCRedeemable || entries[0].Direction != "DEBIT" {
		return domain.ZeroMoney(), fmt.Errorf("%w: escrow %s has malformed reserve entries",
			errs.ErrEscrowConflict, escrowTxID)
	}
	return entries[0].Amount, nil
}

// ----------------------------------------------------------------------------
// Transaction history — read straight from the ledger (single source of truth).
// ----------------------------------------------------------------------------

// maxTransactionsLimit caps the history page size so a misbehaving caller can't
// request an unbounded scan across the partitioned ledger.
const maxTransactionsLimit = 100

func (e *engine) ListTransactions(ctx context.Context, req ListTransactionsRequest) ([]TransactionSummary, error) {
	if req.PlayerID == uuid.Nil {
		return nil, fmt.Errorf("%w: nil player id", errs.ErrPlayerNotFound)
	}
	limit := req.Limit
	if limit <= 0 || limit > maxTransactionsLimit {
		limit = maxTransactionsLimit
	}

	rows, err := e.db.Query(ctx, sqlSelectTransactionsByPlayer,
		req.PlayerID, req.TransactionType, req.Status, limit)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()

	out := make([]TransactionSummary, 0, limit)
	for rows.Next() {
		var (
			id          uuid.UUID
			opTxID      string
			txType      string
			status      string
			gameID      sql.NullString
			roundID     sql.NullString
			createdAt   time.Time
			completedAt sql.NullTime
			amount      decimal.NullDecimal
			currency    sql.NullString
			direction   sql.NullString
		)
		if err := rows.Scan(&id, &opTxID, &txType, &status, &gameID, &roundID,
			&createdAt, &completedAt, &amount, &currency, &direction); err != nil {
			return nil, fmt.Errorf("scan transaction: %w", err)
		}

		s := TransactionSummary{
			LedgerTransactionID:   id,
			OperatorTransactionID: opTxID,
			TransactionType:       txType,
			Status:                status,
			GameID:                gameID.String,
			RoundID:               roundID.String,
			Currency:              currency.String,
			Direction:             direction.String,
			CreatedAt:             createdAt,
		}
		if amount.Valid {
			m, err := domain.NewMoney(amount.Decimal)
			if err != nil {
				return nil, fmt.Errorf("scan transaction amount: %w", err)
			}
			s.Amount = &m
		}
		if completedAt.Valid {
			t := completedAt.Time
			s.CompletedAt = &t
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("transaction rows: %w", err)
	}
	return out, nil
}

// insertEscrowReserveTx writes the PENDING ESCROW_RESERVE header and returns its
// id (the escrow_transaction_id). Returns the raw error so the caller can
// distinguish 23505 (Ghost-Spin) from other failures.
func insertEscrowReserveTx(ctx context.Context, tx pgx.Tx, operatorCode, opTxID string, playerID uuid.UUID, metadata json.RawMessage) (uuid.UUID, error) {
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, sqlInsertEscrowReserveTx, operatorCode, opTxID, playerID, metadata).Scan(&id)
	return id, err
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
	reqHash string,
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
	e.cacheResultQuietly(ctx, idempotencyKey(operatorCode, opTxID), result, reqHash)
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

// cacheResultQuietly tries to write the success payload, tagged with the same
// request-body hash that claimed the key. Failure here is the "Ghost Spin"
// origin condition: a retry will catch 23505 and recover.
func (e *engine) cacheResultQuietly(ctx context.Context, idemKey string, result TxResult, reqHash string) {
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
	if err := e.idem.Store(ctx, idemKey, string(b), reqHash); err != nil {
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

func (r EscrowReserveRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if !r.Amount.IsPositive() {
		return fmt.Errorf("%w: escrow amount must be > 0", errs.ErrInvalidAmount)
	}
	return nil
}

func (r EscrowCommitRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if r.EscrowTransactionID == uuid.Nil {
		return fmt.Errorf("%w: nil escrow_transaction_id", errs.ErrEscrowNotFound)
	}
	return nil
}

func (r EscrowReleaseRequest) validate() error {
	if r.OperatorCode == "" {
		return fmt.Errorf("%w: empty operator_code", errs.ErrInvalidAmount)
	}
	if r.OperatorTransactionID == "" {
		return fmt.Errorf("%w: empty operator_transaction_id", errs.ErrInvalidAmount)
	}
	if r.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: nil player_id", errs.ErrPlayerNotFound)
	}
	if r.EscrowTransactionID == uuid.Nil {
		return fmt.Errorf("%w: nil escrow_transaction_id", errs.ErrEscrowNotFound)
	}
	return nil
}
