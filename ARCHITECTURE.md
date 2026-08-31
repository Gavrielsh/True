# High-Frequency Sweepstakes Casino Transaction Engine
**AI Agent Architecture & Implementation Master Guide**

## ⚠️ SYSTEM DIRECTIVES FOR AI AGENTS (READ FIRST) ⚠️
As an AI assisting in writing code for this project, you must strictly adhere to the following absolute constraints. **Do not deviate, optimize away, or simplify these rules under any circumstances.**

1. **NO FLOATING POINT MATH:** Never use `float32` or `float64` for any currency values. You MUST use `github.com/shopspring/decimal` in Go and `NUMERIC(18,4)` in PostgreSQL.
2. **ZERO-TRUST LEDGER:** Never update a wallet balance independently. EVERY balance mutation requires a balanced Double-Entry Ledger insertion (`DEBIT` = `CREDIT`). 
3. **PESSIMISTIC LOCKING ONLY:** Use `SELECT ... FOR UPDATE` for all wallet mutations. Do not use Optimistic Concurrency Control (OCC) for player wallets. High-frequency slot spins will cause retry-storms under OCC.
4. **STRICT IDEMPOTENCY:** Every state-mutating endpoint must enforce distributed caching and locking via Redis before touching the database.
5. **FAIL CLOSED:** If Redis is down, or Postgres is unreachable, reject the transaction (`HTTP 500` or `424 Failed Dependency`). Never proceed in a degraded state when handling real money or sweepstakes tokens.

---

## 1. System Overview & Scale
This system is a high-frequency, Zero-Trust transaction engine designed for a Social Sweepstakes Casino. It must scale **horizontally**, and a single player can issue rapid consecutive spins, so the design assumes heavy concurrent write pressure on one wallet row.

> **No throughput figure is published here.** This document previously claimed a specific
> transactions-per-second range. Nothing in this repository measured it, so the number stated
> a capability the system had never been shown to have. A concrete target belongs here only
> once a reproducible benchmark harness produces it — see `loadtest/` for the k6 baseline this
> would build on.

### Sweepstakes Core Logic
To comply with strict US sweepstakes legal frameworks, tokens must be segregated natively at the database level:
* **Gold Coins (GC):** Play-for-fun tokens. No real-world value.
* **Sweeps Coins Unplayed (SC_UNPLAYED):** Promotional tokens. Cannot be redeemed for cash directly. Must be wagered first.
* **Sweeps Coins Redeemable (SC_REDEEMABLE):** Tokens won from gameplay. Eligible for real-world prize redemption.

**Betting Logic (`/bet`):** Wagers must consume `SC_UNPLAYED` first. If insufficient, consume the remainder from `SC_REDEEMABLE`.
**Winning Logic (`/win`):** All game winnings in SC must be credited entirely to `SC_REDEEMABLE`.

---

## 2. Technology Stack
* **Language:** Go (Golang).
* **Database:** PostgreSQL (v15+) using `pgx/v5` driver for connection pooling and binary serialization.
* **In-Memory Store:** Redis for idempotency barriers, distributed locks, and session caching.
* **Framework/Router:** `gin-gonic/gin` or standard `net/http` (optimized for zero-allocation routing).

---

## 3. Database Architecture (Double-Entry Ledger)

The `wallets` table acts as a materialized view (cache) for read operations. The actual single source of truth is the `ledger_entries` table. The sum of all historical credits minus debits must always perfectly match the wallet balance.

### Schema Constraints & Partitioning
At sustained production write volume the `ledger_entries` table will reach hundreds of millions of rows. **You MUST implement PostgreSQL Table Partitioning by date.**

**Required PostgreSQL Typings & Constraints:**
* Types: `currency_type` ('GC', 'SC_UNPLAYED', 'SC_REDEEMABLE'), `transaction_type` ('BET', 'WIN', 'ROLLBACK'), `entry_direction` ('DEBIT', 'CREDIT').
* Wallets: Must have `CHECK (balance >= 0.0000)` on all currency columns to prevent negative balances at the DB engine level.
* Ledger: Must be strictly append-only. Must enforce `CHECK (amount > 0.0000)`.

---

## 4. Transaction & Concurrency Flow

When implementing state-mutating endpoints (e.g., `/bet` or `/win`), you must script the flow exactly in this order:

1. **Idempotency Check (Redis):** Attempt `SET api:idempotency:{operator_tx_id} "PROCESSING" NX EX 10`. 
   * If blocked, check if value is `"PROCESSING"` (return `HTTP 409 Conflict`) or a JSON payload (return `HTTP 200` with the cached payload).
2. **Begin DB Transaction:** Use isolation level `READ COMMITTED`.
3. **Acquire Pessimistic Lock:** Execute `SELECT ... FROM wallets WHERE player_id = $1 FOR UPDATE;`. This forces concurrent spins from the same user to queue sequentially.
4. **Calculate Deductions:** Use Go's `decimal` library.
5. **Mutate Wallet Cache:** `UPDATE wallets SET ...`
6. **Append Ledger:** Insert into `ledger_transactions` and `ledger_entries` (Debit and Credit).
7. **Commit Transaction:** `COMMIT;`
8. **Cache Response (Redis):** Set the successful JSON response to the idempotency key with a 24-hour TTL (`SET XX EX 86400`).

---

## 5. Security & Webhook Cryptography

Integrators (e.g., Pragmatic Play, Hacksaw) will call our endpoints via webhooks. The system must verify the `HMAC-SHA256` signature to prevent spoofing.

**AI Implementation Rules for Security Middleware:**
* **Payload Preservation:** Read `c.Request.Body` into a byte array, then restore it `c.Request.Body = io.NopCloser(bytes.NewBuffer(body))`. **NEVER unmarshal the JSON body before hashing.** Unmarshalling alters JSON spacing and breaks signature validation. Hash the raw bytes.
* **Constant Time Compare:** Use `subtle.ConstantTimeCompare` to compare the computed HMAC and the `X-Signature` header to prevent Timing Attacks.
* **Replay Attacks:** Validate the `X-Timestamp` header. Reject requests older than 300 seconds (5 minutes).
* **Nonce Validation:** Cache the `X-Nonce` in Redis (`SET NX EX 600`). If it exists, reject the request as a replay attack.

---

## 6. Extreme Edge Cases & Fail-Safes

You must build robust error handling for the following scenarios:

### A. The "Ghost Spin" (Redis Timeout / Postgres Commit Success)
* **Scenario:** The Postgres transaction commits successfully, but the network drops before Go can update the Redis Idempotency key from `"PROCESSING"` to the final JSON payload. The game provider retries the webhook.
* **Handling:** Before executing the DB `INSERT` for the ledger transaction, you must catch the `unique_violation` (Postgres Error Code `23505`) on the `operator_transaction_id`. If triggered, it means the DB already committed this transaction. Reconstruct the success payload from the current wallet state, update the Redis cache, and return `200 OK` **without re-deducting funds.**

### B. Connection Pool Exhaustion
* **Scenario:** Opening too many Postgres connections causes Context Switching collapse.
* **Handling:** Configure `pgxpool` dynamically. Aim for `MaxConns = (2 * CPU Cores) + 1` for the writer nodes. 

### C. Platform Revenue Hotspots
* **Scenario:** If every transaction credits a single "Platform Master Wallet", that single row will lock the entire database.
* **Handling:** Do not route real-time platform rake/fees to a single wallet row. Platform revenue must be derived asynchronously by aggregating the `ledger_entries` or using domain-driven sharding.

---

## 7. Recommended Folder Structure (Clean Architecture)
```text
├── cmd
│   └── engine          # Main application entry point
├── internal
│   ├── api             # HTTP handlers, Gin/Fiber routing, HMAC middlewares
│   ├── config          # Environment vars, Postgres/Redis connection setups
│   ├── domain          # Core business logic, Decimal math, Sweepstakes allocations
│   ├── repository      # PostgreSQL raw SQL (pgx), locking logic, Ledger appends
│   └── cache           # Redis idempotency wrapper, distributed locks
├── pkg
│   ├── crypto          # HMAC signature generation/validation
│   └── errors          # Domain-specific error codes (e.g., INSUFFICIENT_FUNDS)
└── migrations          # Up/Down SQL schema files (incl. Partitioning logic)