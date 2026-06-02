/**
 * gameplay.controller.ts
 *
 * Directives enforced:
 *  1 — No MongoDB fallback. True Engine failure → 503.
 *  2 — Win credits dispatched via BullMQ (win.queue) with exponential-backoff
 *      retries. A win is NEVER silently dropped.
 *  3 — roundId / betTxId / winTxId extracted from req.body (client-generated).
 *      Server rejects missing or malformed UUIDs with 400.
 *  4 — All money values in integer CENTS. Float multiplier math produces a
 *      single Math.round() result — no floating-point accumulation.
 */

import { Response } from 'express';
import { AuthRequest } from '../middleware/auth.middleware';
import Game from '../models/Game';
import User from '../models/User';
import { trueWallet, TrueEngineError } from '../services/trueWallet.service';
import { winQueue } from '../queues/win.queue';

// ────────────────────────────────────────────────────────────────────────────
// Directive 3 — UUID validation
//
// IDs MUST originate from the client so that server-side retries (network
// timeouts, worker restarts) reuse the same idempotency keys against the
// True Engine's cache. A server-generated UUID on each request would allow
// double-debit / double-credit on any retry.
// ────────────────────────────────────────────────────────────────────────────

const UUID_V4_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

function requireUUID(value: unknown, fieldName: string): string {
  if (typeof value !== 'string' || !UUID_V4_RE.test(value)) {
    const err: any = new Error(`${fieldName} is required and must be a valid v4 UUID`);
    err.statusCode = 400;
    throw err;
  }
  return value;
}

// ────────────────────────────────────────────────────────────────────────────
// Directive 4 — Integer-cents boundary conversion
//
// betAmount arrives from the client in currency units (e.g. 1.50 for $1.50).
// We convert once at the boundary to integer cents (150). All subsequent
// arithmetic is integer-only, with a single Math.round() for the final win
// amount. No .toFixed(), no +string coercion, no accumulated IEEE-754 error.
// ────────────────────────────────────────────────────────────────────────────

function parseToCents(raw: unknown, fieldName: string): number {
  const n = Number(raw);
  if (!isFinite(n) || n <= 0) {
    const err: any = new Error(`${fieldName} must be a positive number`);
    err.statusCode = 400;
    throw err;
  }
  // Math.round eliminates the representation error in e.g. 1.005 * 100.
  return Math.round(n * 100);
}

// ────────────────────────────────────────────────────────────────────────────
// Game result simulator
//
// Parameters and return values are all in integer CENTS.
// Multiplier algebra uses floats (unavoidable), but the final winAmountCents
// is produced by a single Math.round() — no chained float additions.
// ────────────────────────────────────────────────────────────────────────────

const WIN_PROB: Record<string, number> = {
  low: 0.40,
  medium: 0.28,
  high: 0.15,
};

interface SimulateResult {
  isWin: boolean;
  isJackpot: boolean;
  winAmountCents: number;
}

function simulate(
  rtp: number,
  volatility: string,
  betAmountCents: number,
  hasJackpot: boolean,
  jackpotAmountCents?: number,
): SimulateResult {
  // Tiny jackpot chance (0.1 %)
  if (hasJackpot && jackpotAmountCents && Math.random() < 0.001) {
    return { isWin: true, isJackpot: true, winAmountCents: jackpotAmountCents };
  }

  const winProb = WIN_PROB[volatility] ?? 0.28;
  if (Math.random() >= winProb) {
    return { isWin: false, isJackpot: false, winAmountCents: 0 };
  }

  // Float space: compute the multiplier from RTP algebra.
  const avgMultiplier = rtp / (100 * winProb);
  const varianceFactor = 0.6 + Math.random() * 0.8;
  const multiplier = Math.max(1.0, avgMultiplier * varianceFactor);

  // Single Math.round() converts back to integer cents.
  // Math.max ensures minimum payout equals the bet (1× floor).
  const winAmountCents = Math.max(betAmountCents, Math.round(betAmountCents * multiplier));

  return { isWin: true, isJackpot: false, winAmountCents };
}

// ────────────────────────────────────────────────────────────────────────────
// Controller
// ────────────────────────────────────────────────────────────────────────────

export const playGame = async (req: AuthRequest, res: Response) => {
  // ── Directive 3: extract and validate client-generated UUIDs ────────────
  // The client MUST generate all three before the request so that a network
  // retry reuses the same IDs and the True Engine's idempotency cache
  // absorbs duplicates without double-debiting or double-crediting.
  let roundId: string, betTxId: string, winTxId: string;
  try {
    roundId = requireUUID(req.body.roundId, 'roundId');
    betTxId = requireUUID(req.body.betTxId, 'betTxId');
    winTxId = requireUUID(req.body.winTxId, 'winTxId');
  } catch (err: any) {
    return res.status(err.statusCode ?? 400).json({ message: err.message });
  }

  try {
    const { slug } = req.params;
    const { currency = 'GC' } = req.body;

    if (!['GC', 'SC'].includes(currency)) {
      return res.status(400).json({ message: 'currency must be "GC" or "SC"' });
    }

    // ── Directive 4: convert to integer cents at the boundary ───────────────
    let betAmountCents: number;
    try {
      betAmountCents = parseToCents(req.body.betAmount, 'betAmount');
    } catch (err: any) {
      return res.status(400).json({ message: err.message });
    }

    const game = await Game.findOne({ slug });
    if (!game) return res.status(404).json({ message: 'Game not found' });

    const minBetCents = Math.round(game.minBet * 100);
    const maxBetCents = Math.round(game.maxBet * 100);

    if (betAmountCents < minBetCents || betAmountCents > maxBetCents) {
      return res.status(400).json({
        message: `Bet must be between ${game.minBet} and ${game.maxBet}`,
      });
    }

    const user = await User.findById(req.userId).select('truePlayerId responsibleGaming');
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (
      user.responsibleGaming?.selfExclusionUntil &&
      user.responsibleGaming.selfExclusionUntil > new Date()
    ) {
      return res.status(403).json({ message: 'Account is currently self-excluded' });
    }

    if (!user.truePlayerId) {
      return res.status(400).json({
        message: 'Wallet not linked to transaction engine. Please re-register or contact support.',
        code: 'WALLET_NOT_PROVISIONED',
      });
    }

    // ── Phase 1: debit the bet from the True Engine ─────────────────────────
    // Directive 1: No MongoDB fallback. Fail-fast with 503 if the engine is
    // unreachable. The player's account is never touched locally.
    let betResult: any;
    try {
      betResult = await trueWallet.bet(
        betTxId,
        user.truePlayerId,
        currency as 'GC' | 'SC',
        betAmountCents,
        String(game._id),
        roundId,
      );
    } catch (err) {
      if (err instanceof TrueEngineError) {
        if (err.trueCode === 'INSUFFICIENT_FUNDS') {
          return res.status(400).json({ message: 'Insufficient balance', code: 'INSUFFICIENT_FUNDS' });
        }
        return res.status(err.httpStatus).json({ message: err.message, code: err.trueCode });
      }
      // Network timeout / engine unreachable — fail-fast.
      return res.status(503).json({
        message: 'Financial service temporarily unavailable. Your account has not been charged.',
        code: 'ENGINE_UNAVAILABLE',
      });
    }

    // ── Phase 2: simulate game outcome ──────────────────────────────────────
    const jackpotAmountCents = game.jackpotAmount
      ? Math.round(game.jackpotAmount * 100)
      : undefined;

    const outcome = simulate(
      game.rtp,
      game.volatility,
      betAmountCents,
      game.hasJackpot,
      jackpotAmountCents,
    );

    const postBalances = betResult?.result?.post_balances ?? null;
    const betLedgerTxId: string = betResult?.result?.ledger_transaction_id ?? '';

    // ── Phase 3: enqueue win credit via BullMQ ──────────────────────────────
    // Directive 2: Replace the fragile try/catch with a durable message queue.
    // The worker retries with exponential backoff until the True Engine returns
    // 200 OK. jobId = winTxId ensures a client-retry enqueue is a no-op (BullMQ
    // deduplicates by jobId); the True Engine's cache absorbs worker retries.
    if (outcome.isWin && outcome.winAmountCents > 0) {
      await winQueue.add(
        'credit-win',
        {
          winTxId,
          playerId: user.truePlayerId,
          currency: currency as 'GC' | 'SC',
          winAmountCents: outcome.winAmountCents,
          gameId: String(game._id),
          roundId,
          betLedgerTxId,
        },
        { jobId: winTxId },
      );
    }

    return res.json({
      isWin: outcome.isWin,
      isJackpot: outcome.isJackpot,
      betAmountCents,
      winAmountCents: outcome.winAmountCents,
      currency,
      balances: postBalances,
      engine: 'true',
    });
  } catch (error) {
    console.error('Play game error:', error);
    res.status(500).json({ message: 'Game play failed' });
  }
};
