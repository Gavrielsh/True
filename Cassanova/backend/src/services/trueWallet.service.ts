/**
 * TrueWalletService — HMAC-signed HTTP client for the True transaction engine.
 *
 * AMOUNT CONVENTION (Directive 4 — Integer-Cents Architecture):
 *   All monetary amounts are passed as integer cents (e.g. 150 = $1.50).
 *   centsToWire() converts to the 4-decimal string required by the True Engine
 *   using pure integer arithmetic — no floating-point division that could
 *   produce 1.5099999... instead of "1.5100".
 *
 * Every outbound call includes:
 *   X-Operator-Code  — identifies Cassanova as the operator
 *   X-Signature      — HMAC-SHA256(raw_body, secret) as hex
 *   X-Timestamp      — unix seconds (True rejects if >5 min old)
 *   X-Nonce          — per-request UUID consumed by True's Redis nonce store
 */

import crypto from 'crypto';

const TRUE_ENGINE_URL =
  process.env.TRUE_ENGINE_URL || 'http://localhost:8080';
const TRUE_OPERATOR_CODE =
  process.env.TRUE_OPERATOR_CODE || 'cassanova';
const TRUE_OPERATOR_SECRET =
  process.env.TRUE_OPERATOR_SECRET || 'dev-secret-change-me';

// ────────────────────────────────────────────────────────────────────────────
// Error type — lets callers distinguish True domain errors (e.g. insufficient
// funds) from unexpected network / server failures.
// ────────────────────────────────────────────────────────────────────────────

export class TrueEngineError extends Error {
  readonly trueCode: string;
  readonly httpStatus: number;

  constructor(message: string, trueCode: string, httpStatus: number) {
    super(message);
    this.name = 'TrueEngineError';
    this.trueCode = trueCode;
    this.httpStatus = httpStatus;
  }
}

// ────────────────────────────────────────────────────────────────────────────
// Integer-cents → 4-decimal wire format (Directive 4).
//
// Uses integer arithmetic only — no floating-point division.
//   centsToWire(150)  → "1.5000"
//   centsToWire(151)  → "1.5100"
//   centsToWire(10000) → "100.0000"
// ────────────────────────────────────────────────────────────────────────────

function centsToWire(cents: number): string {
  if (!Number.isInteger(cents) || cents < 0) {
    throw new Error(`Amount must be a non-negative integer (cents), got: ${cents}`);
  }
  const major = Math.floor(cents / 100);
  const minor = cents % 100;
  return `${major}.${minor.toString().padStart(2, '0')}00`;
}

// ────────────────────────────────────────────────────────────────────────────
// Internal call helper
// ────────────────────────────────────────────────────────────────────────────

async function call(
  method: 'GET' | 'POST',
  path: string,
  body?: object,
): Promise<any> {
  const bodyStr = body ? JSON.stringify(body) : '';

  const signature = crypto
    .createHmac('sha256', TRUE_OPERATOR_SECRET)
    .update(bodyStr)
    .digest('hex');

  const timestamp = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomUUID();

  const url = `${TRUE_ENGINE_URL}${path}`;

  const options: RequestInit = {
    method,
    headers: {
      'Content-Type': 'application/json',
      'X-Operator-Code': TRUE_OPERATOR_CODE,
      'X-Signature': signature,
      'X-Timestamp': timestamp,
      'X-Nonce': nonce,
    },
  };

  if (bodyStr && method !== 'GET') {
    options.body = bodyStr;
  }

  let response: Response;
  try {
    response = await fetch(url, options);
  } catch (err: any) {
    throw new Error(`True engine unreachable at ${url}: ${err?.message}`);
  }

  const json = await response.json().catch(() => ({})) as { message?: string; code?: string };

  if (!response.ok) {
    throw new TrueEngineError(
      json?.message || 'True engine error',
      json?.code || 'INTERNAL',
      response.status,
    );
  }

  return json;
}

// ────────────────────────────────────────────────────────────────────────────
// Public API
// ────────────────────────────────────────────────────────────────────────────

export const trueWallet = {
  /** Provision a player row + empty wallet. Idempotent (ON CONFLICT DO NOTHING). */
  createPlayer: (
    playerId: string,
    externalId: string,
    email: string,
    username: string,
  ) =>
    call('POST', '/api/v1/player', {
      player_id: playerId,
      external_id: externalId,
      email,
      username,
    }),

  /** Snapshot read of all three currency balances. */
  getBalances: (playerId: string) =>
    call('GET', `/api/v1/session?player_id=${playerId}`),

  /**
   * Credit the player's GC (real money) or SC_UNPLAYED (promo) wallet.
   * @param amountCents - Integer cents (e.g. 5000 = $50.00)
   */
  deposit: (
    operatorTxId: string,
    playerId: string,
    currency: 'GC' | 'SC',
    amountCents: number,
  ) =>
    call('POST', '/api/v1/deposit', {
      operator_transaction_id: operatorTxId,
      player_id: playerId,
      currency,
      amount: centsToWire(amountCents),
    }),

  /**
   * Debit a bet from GC or SC wallet. Returns the True TxResult.
   * @param amountCents - Integer cents (e.g. 150 = $1.50)
   */
  bet: (
    operatorTxId: string,
    playerId: string,
    currency: 'GC' | 'SC',
    amountCents: number,
    gameId: string,
    roundId: string,
  ) =>
    call('POST', '/api/v1/bet', {
      operator_transaction_id: operatorTxId,
      player_id: playerId,
      currency,
      amount: centsToWire(amountCents),
      game_id: gameId,
      round_id: roundId,
    }),

  /**
   * Credit a win to the GC or SC_REDEEMABLE wallet.
   * @param amountCents - Integer cents (e.g. 375 = $3.75)
   */
  win: (
    operatorTxId: string,
    playerId: string,
    currency: 'GC' | 'SC',
    amountCents: number,
    gameId: string,
    roundId: string,
    referenceTxId: string,
  ) =>
    call('POST', '/api/v1/win', {
      operator_transaction_id: operatorTxId,
      player_id: playerId,
      currency,
      amount: centsToWire(amountCents),
      game_id: gameId,
      round_id: roundId,
      reference_transaction_id: referenceTxId || undefined,
    }),
};
