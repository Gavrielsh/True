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

	// Rollback: locks the original ledger_transactions row so two concurrent
	// rollback requests for the same reference serialize.
	sqlSelectLedgerTxByIDForUpdate = `
		SELECT player_id, transaction_type, status
		FROM ledger_transactions
		WHERE id = $1
		FOR UPDATE
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

	// Rollback: flip the original tx's status. The status_was_completed CHECK
	// in the WHERE clause makes the UPDATE a no-op (RowsAffected==0) on
	// concurrent retries — caller then knows the rollback already landed.
	sqlMarkTxRolledBack = `
		UPDATE ledger_transactions
		SET status = 'ROLLED_BACK',
		    completed_at = now()
		WHERE id = $1
		  AND status = 'COMPLETED'
	`

	// Player provisioning (called once at Cassanova registration).
	// ON CONFLICT DO NOTHING makes it safe to retry on network hiccups.
	sqlCreateUser = `
		INSERT INTO users (id, external_id, email, username, status, country_code)
		VALUES ($1, $2, $3, $4, 'ACTIVE', 'US')
		ON CONFLICT (id) DO NOTHING
	`

	// Wallet row for a new player. All balances default to 0.
	sqlCreateWallet = `
		INSERT INTO wallets (player_id)
		VALUES ($1)
		ON CONFLICT (player_id) DO NOTHING
	`
)
