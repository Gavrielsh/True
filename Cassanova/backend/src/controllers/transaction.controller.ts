/**
 * transaction.controller.ts
 *
 * Pillar 1 — Escrow. createWithdrawal LOCKS the funds in the True Engine
 * (escrowReserve) BEFORE persisting any pending request. A player can no longer
 * request a withdrawal and then gamble the same SC: the engine has already
 * moved it out of SC_REDEEMABLE into HOUSE_ESCROW_POOL.
 *
 * Pillar 2 — Single Source of Truth. Completed financial history (deposits,
 * bets, wins, escrow) lives ONLY in the engine ledger. MongoDB keeps user
 * profiles/KYC and the ops-state of *pending* manual withdrawals — never a
 * second copy of financial history. createDeposit no longer dual-writes to
 * Mongo, and getUserTransactions PROXIES the engine.
 *
 * Pillar 4 — Boundary hardening. Every financial input is parsed with a strict
 * Zod schema (positive, finite, bounded amount, <= 2 decimals) BEFORE any
 * centsToWire conversion — closing integer-overflow / NaN / negative vectors
 * that bare Number() casting left open.
 */

import { Response } from 'express';
import { randomUUID } from 'crypto';
import { z } from 'zod';
import { AuthRequest } from '../middleware/auth.middleware';
import Transaction from '../models/Transaction';
import User from '../models/User';
import { trueWallet, TrueEngineError } from '../services/trueWallet.service';

// ────────────────────────────────────────────────────────────────────────────
// Pillar 4 — strict financial input schemas
// ────────────────────────────────────────────────────────────────────────────

// Hard per-transaction ceiling. Far below the point where amount*100 could
// approach Number.MAX_SAFE_INTEGER, so the integer-cents conversion downstream
// can never overflow or lose precision.
const MAX_TXN_AMOUNT = 1_000_000; // $1,000,000.00

// A positive, finite, bounded money value with at most 2 decimal places.
// Accepts JSON numbers or numeric strings; rejects NaN / Infinity / negatives /
// overflow / sub-cent precision before the value reaches centsToWire.
const amountSchema = z.coerce
  .number({ invalid_type_error: 'amount must be a number' })
  .finite('amount must be a finite number') // rejects NaN / Infinity
  .gt(0, { message: 'amount must be greater than 0' })
  .max(MAX_TXN_AMOUNT, { message: `amount must not exceed ${MAX_TXN_AMOUNT}` })
  .refine((n: number) => Math.abs(n * 100 - Math.round(n * 100)) < 1e-6, {
    message: 'amount supports at most 2 decimal places',
  });

const paymentMethodSchema = z
  .string({ invalid_type_error: 'paymentMethod must be a string' })
  .trim()
  .min(1, 'paymentMethod is required')
  .max(64, 'paymentMethod is too long');

const depositSchema = z
  .object({ amount: amountSchema, paymentMethod: paymentMethodSchema })
  .strict();

const withdrawalSchema = z
  .object({ amount: amountSchema, paymentMethod: paymentMethodSchema })
  .strict();

function firstZodError(err: z.ZodError): string {
  return err.issues[0]?.message ?? 'Invalid request';
}

// ────────────────────────────────────────────────────────────────────────────
// Pillar 2 — engine→client transaction shape mapping
// ────────────────────────────────────────────────────────────────────────────

const TYPE_MAP: Record<string, string> = {
  BET: 'bet',
  WIN: 'win',
  DEPOSIT: 'deposit',
  ESCROW_RESERVE: 'withdrawal',
  ESCROW_COMMIT: 'withdrawal',
  ESCROW_RELEASE: 'withdrawal',
  ROLLBACK: 'rollback',
};

const STATUS_MAP: Record<string, string> = {
  PENDING: 'pending',
  COMPLETED: 'completed',
  ROLLED_BACK: 'cancelled',
  FAILED: 'failed',
};

// The engine reports amounts as 4-decimal currency strings (e.g. "50.0000").
// Convert to the integer-cents convention the rest of the app uses.
function engineAmountToCents(raw: unknown): number {
  if (raw == null) return 0;
  const n = Number(raw);
  return Number.isFinite(n) ? Math.round(n * 100) : 0;
}

function mapEngineTransaction(t: any, userId: string) {
  const ledgerId: string = t?.ledger_transaction_id ?? '';
  return {
    _id: ledgerId,
    id: ledgerId,
    userId,
    type: TYPE_MAP[t?.transaction_type] ?? String(t?.transaction_type ?? '').toLowerCase(),
    amount: engineAmountToCents(t?.amount),
    currency: t?.currency ?? undefined,
    status: STATUS_MAP[t?.status] ?? String(t?.status ?? '').toLowerCase(),
    description: [t?.transaction_type, t?.currency].filter(Boolean).join(' '),
    createdAt: t?.created_at ?? null,
    completedAt: t?.completed_at ?? null,
    operatorTransactionId: t?.operator_transaction_id ?? undefined,
    ledgerTransactionId: ledgerId,
    source: 'true-engine' as const,
  };
}

// ────────────────────────────────────────────────────────────────────────────
// GET /transactions — Pillar 2: read straight from the engine ledger.
// ────────────────────────────────────────────────────────────────────────────

export const getUserTransactions = async (req: AuthRequest, res: Response) => {
  try {
    const { type, status, limit } = req.query;

    const user = await User.findById(req.userId).select('truePlayerId');
    if (!user) return res.status(404).json({ message: 'User not found' });

    // No engine wallet yet → the player has no financial history to show.
    if (!user.truePlayerId) return res.json([]);

    let engineResult: any;
    try {
      engineResult = await trueWallet.getTransactions(user.truePlayerId, {
        limit: typeof limit === 'string' ? Number(limit) : undefined,
      });
    } catch (err) {
      if (err instanceof TrueEngineError) {
        return res.status(err.httpStatus).json({ message: err.message, code: err.trueCode });
      }
      return res.status(503).json({
        message: 'Financial service temporarily unavailable.',
        code: 'ENGINE_UNAVAILABLE',
      });
    }

    const engineTxns: any[] = Array.isArray(engineResult?.transactions)
      ? engineResult.transactions
      : [];

    let transactions = engineTxns.map((t) => mapEngineTransaction(t, String(req.userId)));

    // The client uses friendly vocab (deposit/withdrawal/bet/win) which spans
    // several engine types (e.g. withdrawal ← ESCROW_*), so we filter after the
    // mapping rather than pushing an ambiguous exact filter into the engine.
    if (typeof type === 'string' && type) {
      transactions = transactions.filter((t) => t.type === type);
    }
    if (typeof status === 'string' && status) {
      transactions = transactions.filter((t) => t.status === status);
    }

    return res.json(transactions);
  } catch (error) {
    console.error('Get transactions error:', error);
    res.status(500).json({ message: 'Failed to fetch transactions' });
  }
};

// ────────────────────────────────────────────────────────────────────────────
// POST /transactions/deposit — Pillar 2 + 4: engine is the only record.
// ────────────────────────────────────────────────────────────────────────────

export const createDeposit = async (req: AuthRequest, res: Response) => {
  try {
    const parsed = depositSchema.safeParse(req.body);
    if (!parsed.success) {
      return res.status(400).json({ message: firstZodError(parsed.error), code: 'INVALID_INPUT' });
    }
    const { amount, paymentMethod } = parsed.data;

    // Pillar 4: validated, bounded amount → safe integer-cents conversion.
    const amountCents = Math.round(amount * 100);

    const user = await User.findById(req.userId).select('truePlayerId kycStatus');
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (!user.truePlayerId) {
      return res.status(400).json({
        message: 'Wallet not provisioned. Please contact support.',
        code: 'WALLET_NOT_PROVISIONED',
      });
    }

    // Pillar 2: the engine ledger is the SOLE record of this deposit. No Mongo
    // dual-write — there is no orphan-on-Mongo-crash window anymore.
    let trueResult: any;
    try {
      trueResult = await trueWallet.deposit(
        `dep-${req.userId}-${randomUUID()}`,
        user.truePlayerId,
        'GC',
        amountCents,
      );
    } catch (err) {
      if (err instanceof TrueEngineError) {
        return res.status(err.httpStatus).json({ message: err.message, code: err.trueCode });
      }
      return res.status(503).json({
        message: 'Financial service temporarily unavailable. Deposit was not processed.',
        code: 'ENGINE_UNAVAILABLE',
      });
    }

    return res.status(201).json({
      message: 'Deposit successful',
      transaction: trueResult?.result ?? null,
      balances: trueResult?.result?.post_balances ?? null,
    });
  } catch (error) {
    console.error('Deposit error:', error);
    res.status(500).json({ message: 'Deposit failed' });
  }
};

// ────────────────────────────────────────────────────────────────────────────
// POST /transactions/withdrawal — Pillar 1 + 4: escrow-first.
// ────────────────────────────────────────────────────────────────────────────

export const createWithdrawal = async (req: AuthRequest, res: Response) => {
  try {
    const parsed = withdrawalSchema.safeParse(req.body);
    if (!parsed.success) {
      return res.status(400).json({ message: firstZodError(parsed.error), code: 'INVALID_INPUT' });
    }
    const { amount, paymentMethod } = parsed.data;
    const amountCents = Math.round(amount * 100);

    const user = await User.findById(req.userId).select('truePlayerId kycStatus');
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (user.kycStatus !== 'verified') {
      return res.status(400).json({
        message: 'KYC verification required for withdrawals',
        code: 'KYC_REQUIRED',
      });
    }
    if (!user.truePlayerId) {
      return res.status(400).json({
        message: 'Wallet not provisioned. Please contact support.',
        code: 'WALLET_NOT_PROVISIONED',
      });
    }

    // ── Pillar 1, Step 1: LOCK the funds in the engine FIRST ────────────────
    // escrowReserve deducts SC_REDEEMABLE into HOUSE_ESCROW_POOL and returns the
    // escrow ledger id. If the engine rejects (e.g. INSUFFICIENT_FUNDS) nothing
    // is locked and nothing is persisted. The balance check is the ENGINE's —
    // we never read a local balance to gate this.
    const escrowOpTxId = `wd-reserve-${req.userId}-${randomUUID()}`;
    let reserve: any;
    try {
      reserve = await trueWallet.escrowReserve(escrowOpTxId, user.truePlayerId, amountCents);
    } catch (err) {
      if (err instanceof TrueEngineError) {
        return res.status(err.httpStatus).json({ message: err.message, code: err.trueCode });
      }
      return res.status(503).json({
        message: 'Financial service temporarily unavailable. Withdrawal was not processed.',
        code: 'ENGINE_UNAVAILABLE',
      });
    }

    const escrowLedgerTxId: string | undefined = reserve?.result?.ledger_transaction_id;
    if (!escrowLedgerTxId) {
      // Engine answered 200 without an id — refuse to record a request we could
      // never commit/release.
      return res.status(502).json({
        message: 'Withdrawal could not be reserved. Please try again.',
        code: 'ESCROW_RESERVE_FAILED',
      });
    }

    // ── Pillar 1, Step 2: only NOW persist the pending request ──────────────
    // The funds are safely escrowed. Store the ops-review record carrying the
    // True escrow id so ops can later commit (pay) or release (refund).
    try {
      const transaction = await new Transaction({
        userId: req.userId,
        type: 'withdrawal',
        amount: amountCents,
        status: 'pending',
        paymentMethod,
        trueEscrowTransactionId: escrowLedgerTxId,
        description: `Withdrawal via ${paymentMethod}`,
      }).save();

      return res.status(201).json({
        message: 'Withdrawal request submitted and pending review',
        transaction,
        escrowTransactionId: escrowLedgerTxId,
        balances: reserve?.result?.post_balances ?? null,
      });
    } catch (persistErr) {
      // Mongo failed AFTER the engine locked the funds. Compensate: release the
      // escrow so the player's SC is never stranded. (The engine reservation is
      // the source of truth; if release also fails, ops can reconcile the
      // PENDING ESCROW_RESERVE that has no matching Mongo row.)
      try {
        await trueWallet.escrowRelease(
          `wd-release-${escrowLedgerTxId}`,
          user.truePlayerId,
          escrowLedgerTxId,
        );
      } catch (releaseErr) {
        console.error('Escrow release after persistence failure also failed:', releaseErr);
      }
      console.error('Withdrawal persistence error (escrow released):', persistErr);
      return res.status(500).json({
        message: 'Withdrawal failed and the reservation was rolled back. Please try again.',
        code: 'WITHDRAWAL_PERSIST_FAILED',
      });
    }
  } catch (error) {
    console.error('Withdrawal error:', error);
    res.status(500).json({ message: 'Withdrawal failed' });
  }
};
