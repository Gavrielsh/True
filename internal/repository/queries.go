package repository

// All SQL is raw and parameterised. No string interpolation of user input.
// Statements deliberately use $N positional args so pgx can prepare-and-cache
// them; renaming a placeholder forces a re-prepare and a connection-pool
// stall under high load.

const (
	// SELECT ... FOR UPDATE: blocks any concurrent spin for this player.
	// Held until COMMIT/ROLLBACK. Returns ErrNoRows if the wallet does not
	// exist (player unknown / not yet provisioned).
	sqlSelectWalletForUpdate = `
		SELECT gc_balance, sc_unplayed_balance, sc_redeemable_balance
		FROM wallets
		WHERE player_id = $1
		FOR UPDATE
	`

	// Non-locking snapshot read for /session.
	sqlSelectWalletBalances = `
		SELECT gc_balance, sc_unplayed_balance, sc_redeemable_balance
		FROM wallets
		WHERE player_id = $1
	`

	// KYC/compliance guard: the player's lifecycle status, read INSIDE the
	// open transaction right after the wallets FOR UPDATE. READ COMMITTED sees
	// the latest committed status, and the wallet lock serializes the check
	// against any concurrent money movement for the same player — a suspension
	// committed mid-flight is observed by every queued spin behind the lock.
	sqlSelectPlayerStatus = `SELECT status FROM users WHERE id = $1`

	// UPDATE wallets: precise per-column assignment of the post-state values
	// the domain layer just computed. updated_at is bumped by the trigger.
	sqlUpdateWallet = `
		UPDATE wallets
		SET gc_balance            = $1,
		    sc_unplayed_balance   = $2,
		    sc_redeemable_balance = $3
		WHERE player_id = $4
	`

	// INSERT into ledger_transactions: this is the idempotency anchor.
	// The (operator_code, operator_transaction_id) UNIQUE constraint raises
	// SQLSTATE 23505 on duplicate webhook delivery; the engine intercepts
	// that error code for Ghost-Spin recovery.
	//
	// status is COMMITTED inline — for /bet and /win the entire flow is
	// atomic, so the row only materialises if the surrounding tx commits.
	sqlInsertLedgerTx = `
		INSERT INTO ledger_transactions (
			operator_code,
			operator_transaction_id,
			player_id,
			transaction_type,
			status,
			game_id,
			round_id,
			reference_transaction_id,
			request_metadata,
			completed_at
		)
		VALUES ($1, $2, $3, $4, 'COMPLETED', $5, $6, $7, $8, now())
		RETURNING id
	`

	// INSERT into ledger_transaction_dedup: the GLOBAL, cross-time idempotency
	// anchor (migration 000005). ledger_transactions is RANGE-partitioned by
	// created_at, so its UNIQUE(operator_code, operator_transaction_id,
	// created_at) is only PARTITION-LOCAL — a webhook replayed on a later day
	// lands in a different partition and would NOT raise 23505. This
	// non-partitioned table's PK (operator_code, operator_transaction_id) is
	// what makes a duplicate operator transaction collide regardless of when it
	// arrives. Written in the SAME tx as the ledger_transactions row; the 23505
	// it raises is what the engine catches for Ghost-Spin recovery (§6.A).
	sqlInsertLedgerTxDedup = `
		INSERT INTO ledger_transaction_dedup (
			operator_code,
			operator_transaction_id,
			ledger_transaction_id
		)
		VALUES ($1, $2, $3)
	`

	// INSERT into ledger_entries: one row per debit or credit line.
	// balance_after is NULL for HOUSE_* rows (per CHECK constraint).
	sqlInsertLedgerEntry = `
		INSERT INTO ledger_entries (
			ledger_transaction_id,
			player_id,
			account_type,
			currency,
			direction,
			amount,
			balance_after
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	// Lookup used by Ghost-Spin recovery to fetch the committed counterpart.
	sqlSelectLedgerTxByOperator = `
		SELECT id, player_id, transaction_type
		FROM ledger_transactions
		WHERE operator_code = $1
		  AND operator_transaction_id = $2
	`

	// Rollback: read the original ledger_transactions header (immutable — the
	// ledger is strictly append-only, so NO lock is taken here). Serialization
	// of concurrent rollbacks is provided by the wallets FOR UPDATE lock taken
	// first in the same flow: every rollback of this BET targets the same
	// player, so they queue on that one row. `status` is intentionally NOT
	// selected — rollback state is now derived from the append-only audit
	// trail (sqlRollbackExistsForReference), never from a mutable flag.
	sqlSelectLedgerTxByID = `
		SELECT player_id, transaction_type
		FROM ledger_transactions
		WHERE id = $1
	`

	// Rollback: fetch the original tx's PLAYER_WALLET entries so we know
	// which currencies and amounts to reverse. Order is deterministic for
	// stable test assertions.
	sqlSelectPlayerEntriesByTx = `
		SELECT currency, direction, amount
		FROM ledger_entries
		WHERE ledger_transaction_id = $1
		  AND account_type = 'PLAYER_WALLET'
		ORDER BY currency
	`

	// Rollback (APPEND-ONLY double-rollback guard): a BET is "already rolled
	// back" iff a ROLLBACK ledger_transactions row already references it. We
	// derive that from the immutable audit trail instead of mutating a status
	// flag on the original row. The NOT(...) clause excludes THIS rollback's
	// own anchor so a genuine retry of the same operator_transaction_id falls
	// through to the dedup INSERT and is handled by Ghost-Spin recovery (replay
	// success) rather than being misreported as ErrRollbackAlready. Served by
	// ledger_tx_reference_idx; correctness is guaranteed because the caller
	// holds the player's wallet row lock while running this check.
	sqlRollbackExistsForReference = `
		SELECT EXISTS (
			SELECT 1
			FROM ledger_transactions
			WHERE reference_transaction_id = $1
			  AND transaction_type = 'ROLLBACK'
			  AND NOT (operator_code = $2 AND operator_transaction_id = $3)
		)
	`
)
