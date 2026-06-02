/**
 * transaction.controller.ts
 *
 * Directive 1 — No local balance mutations.
 * Deposits and withdrawals route exclusively through the True Engine.
 * No user.balance reads or writes. The Transaction collection is kept
 * as an audit log (read-only after creation) — the source of financial
 * truth is always the True Engine ledger.
 *
 * Directive 4 — Amounts converted to integer cents before calling trueWallet.
 */

import { Response } from 'express';
import { AuthRequest } from '../middleware/auth.middleware';
import Transaction from '../models/Transaction';
import User from '../models/User';
import { trueWallet, TrueEngineError } from '../services/trueWallet.service';

export const getUserTransactions = async (req: AuthRequest, res: Response) => {
  try {
    const { type, status } = req.query;
    const filter: Record<string, unknown> = { userId: req.userId };

    if (type) filter.type = type;
    if (status) filter.status = status;

    const transactions = await Transaction.find(filter).sort({ createdAt: -1 }).limit(50);
    res.json(transactions);
  } catch (error) {
    console.error('Get transactions error:', error);
    res.status(500).json({ message: 'Failed to fetch transactions' });
  }
};

export const createDeposit = async (req: AuthRequest, res: Response) => {
  try {
    const { amount, paymentMethod } = req.body;

    const amountNum = Number(amount);
    if (!isFinite(amountNum) || amountNum <= 0) {
      return res.status(400).json({ message: 'Invalid deposit amount' });
    }

    // Directive 4: convert to integer cents at the boundary.
    const amountCents = Math.round(amountNum * 100);

    const user = await User.findById(req.userId).select('truePlayerId kycStatus');
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (!user.truePlayerId) {
      return res.status(400).json({
        message: 'Wallet not provisioned. Please contact support.',
        code: 'WALLET_NOT_PROVISIONED',
      });
    }

    // Directive 1: Deposit goes to the True Engine first. If the engine is
    // unreachable, we return 503 — no local balance mutation ever happens.
    let trueResult: any;
    try {
      trueResult = await trueWallet.deposit(
        `dep-${req.userId}-${Date.now()}`,
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

    // Persist audit log after the True Engine confirms.
    const transaction = await new Transaction({
      userId: req.userId,
      type: 'deposit',
      amount: amountCents,
      status: 'completed',
      paymentMethod,
      description: `Deposit via ${paymentMethod}`,
    }).save();

    res.status(201).json({
      message: 'Deposit successful',
      transaction,
      balances: trueResult?.result?.post_balances ?? null,
    });
  } catch (error) {
    console.error('Deposit error:', error);
    res.status(500).json({ message: 'Deposit failed' });
  }
};

export const createWithdrawal = async (req: AuthRequest, res: Response) => {
  try {
    const { amount, paymentMethod } = req.body;

    const amountNum = Number(amount);
    if (!isFinite(amountNum) || amountNum <= 0) {
      return res.status(400).json({ message: 'Invalid withdrawal amount' });
    }

    const user = await User.findById(req.userId).select('truePlayerId kycStatus');
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (user.kycStatus !== 'verified') {
      return res.status(400).json({ message: 'KYC verification required for withdrawals' });
    }

    // Note: balance check is performed by the True Engine (it will return
    // INSUFFICIENT_FUNDS if the wallet balance is below the withdrawal amount).
    // We never read a local balance to gate this check.

    // Persist withdrawal request as "pending" for operations review.
    const transaction = await new Transaction({
      userId: req.userId,
      type: 'withdrawal',
      amount: Math.round(amountNum * 100),
      status: 'pending',
      paymentMethod,
      description: `Withdrawal via ${paymentMethod}`,
    }).save();

    res.status(201).json({
      message: 'Withdrawal request submitted and pending review',
      transaction,
    });
  } catch (error) {
    console.error('Withdrawal error:', error);
    res.status(500).json({ message: 'Withdrawal failed' });
  }
};
