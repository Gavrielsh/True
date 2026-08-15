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

	// sqlLockWalletAndStatus acquires the pessimistic wallet lock AND reads the
	// player's lifecycle status in ONE round-trip.
	//
	// Previously these were two separate statements inside the lock window. The
	// join is exactly equivalent — wallets:users is 1:1 on player_id — and the
	// guarantee is unchanged: the status is read inside the same transaction,
	// after the row lock, so a suspension committed mid-flight is observed by
	// every spin queued behind the lock.
	//
	// FOR UPDATE OF w locks ONLY the wallet row. Locking users too would add a
	// second lock to every money operation and invite a deadlock against any
	// flow that touches the two tables in the opposite order.
	sqlLockWalletAndStatus = `
		SELECT w.gc_balance, w.sc_unplayed_balance, w.sc_redeemable_balance, u.status
		FROM wallets w
		JOIN users u ON u.id = w.player_id
		WHERE w.player_id = $1
		FOR UPDATE OF w
	`

	// sqlSettleSpin performs the ENTIRE write side of a settled round in ONE
	// statement: wallet update, both ledger transaction headers, both global
	// dedup anchors, and every double-entry line.
	//
	// ─────────────────────────────────────────────────────────────────────────
	// WHY A CTE
	// ─────────────────────────────────────────────────────────────────────────
	// The old path issued 7–11 sequential statements while holding the wallet's
	// FOR UPDATE lock, so the lock window was bounded by that many network
	// round-trips rather than by the work itself. Collapsing them removes the
	// round-trips from the critical section without changing what is written.
	//
	// ─────────────────────────────────────────────────────────────────────────
	// ORDERING
	// ─────────────────────────────────────────────────────────────────────────
	// Data-modifying CTEs are NOT ordered by their textual position — Postgres
	// runs them all, but sequencing comes only from data dependencies. Each
	// stage therefore selects FROM the previous one (bet_tx FROM locked,
	// bet_dedup FROM bet_tx, win_tx FROM bet_tx), which is what forces the
	// wallet update to happen before the ledger rows that describe it.
	//
	// ─────────────────────────────────────────────────────────────────────────
	// GHOST-SPIN ANCHOR
	// ─────────────────────────────────────────────────────────────────────────
	// The dedup inserts are inside this statement, so a duplicate
	// operator_transaction_id raises 23505 and the WHOLE statement — wallet
	// update included — rolls back atomically. The engine catches that code and
	// switches to Ghost-Spin recovery exactly as before. Verified against a
	// live Postgres: on conflict the wallet balance is untouched.
	//
	// ─────────────────────────────────────────────────────────────────────────
	// ENTRIES
	// ─────────────────────────────────────────────────────────────────────────
	// The variable-length entry set arrives as parallel arrays and is expanded
	// with unnest(), so one INSERT covers 2–6 rows without the statement text
	// changing shape. `leg` routes each row to the bet or win header. Money and
	// ids travel as text and are cast server-side: explicit casts keep the
	// driver's array type-mapping out of the money path.
	//
	// The amounts here are computed by the DOMAIN layer and passed through
	// verbatim. No allocation arithmetic happens in this SQL.
	sqlSettleSpin = `
		WITH
		locked AS (
			UPDATE wallets
			SET gc_balance            = $1::numeric,
			    sc_unplayed_balance   = $2::numeric,
			    sc_redeemable_balance = $3::numeric
			WHERE player_id = $4::uuid
			RETURNING player_id
		),
		bet_tx AS (
			INSERT INTO ledger_transactions (
				operator_code, operator_transaction_id, player_id, transaction_type,
				status, game_id, round_id, request_metadata, completed_at)
			SELECT $5, $6, l.player_id, 'BET', 'COMPLETED', $7, $8, $9::jsonb, now()
			FROM locked l
			RETURNING id
		),
		bet_dedup AS (
			INSERT INTO ledger_transaction_dedup (
				operator_code, operator_transaction_id, ledger_transaction_id)
			SELECT $5, $6, b.id FROM bet_tx b
			RETURNING ledger_transaction_id
		),
		win_tx AS (
			INSERT INTO ledger_transactions (
				operator_code, operator_transaction_id, player_id, transaction_type,
				status, game_id, round_id, reference_transaction_id, request_metadata, completed_at)
			SELECT $5, $10, $4::uuid, 'WIN', 'COMPLETED', $7, $8, b.id, $9::jsonb, now()
			FROM bet_tx b
			WHERE $11::boolean
			RETURNING id
		),
		win_dedup AS (
			INSERT INTO ledger_transaction_dedup (
				operator_code, operator_transaction_id, ledger_transaction_id)
			SELECT $5, $10, w.id FROM win_tx w
			RETURNING ledger_transaction_id
		),
		entries AS (
			INSERT INTO ledger_entries (
				ledger_transaction_id, player_id, account_type, currency,
				direction, amount, balance_after)
			SELECT
				CASE e.leg WHEN 'BET' THEN (SELECT id FROM bet_tx) ELSE (SELECT id FROM win_tx) END,
				e.player_id,
				e.account_type::account_type,
				e.currency::currency_type,
				e.direction::entry_direction,
				e.amount,
				e.balance_after
			FROM unnest(
				$12::text[], $13::uuid[], $14::text[], $15::text[],
				$16::text[], $17::numeric[], $18::numeric[]
			) AS e(leg, player_id, account_type, currency, direction, amount, balance_after)
			RETURNING 1
		)
		SELECT (SELECT id FROM bet_tx) AS bet_id, (SELECT id FROM win_tx) AS win_id
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

	// Ghost-Spin recovery for a server-authoritative round. Returns the
	// committed BET leg plus its request_metadata, which carries the ORIGINAL
	// outcome (reels, line, multiplier). Recovering the outcome from the
	// ledger — rather than trusting the retry's fresh draw — is what stops a
	// player re-rolling a settled spin by replaying the request.
	sqlSelectLedgerTxForGhost = `
		SELECT id, player_id, request_metadata
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
