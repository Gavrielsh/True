import crypto from 'crypto';
import { Response } from 'express';
import { AuthRequest } from '../middleware/auth.middleware';
import Game from '../models/Game';
import Transaction from '../models/Transaction';
import User from '../models/User';
import { trueWallet, TrueEngineError } from '../services/trueWallet.service';

// ────────────────────────────────────────────────────────────────────────────
// Game result simulator
//
// Win probability varies by volatility so the long-run expected payout equals
// the game's stated RTP:
//   expected_win = betAmount * rtp/100
//   avg_multiplier_when_win = rtp / (100 * winProb)
// ────────────────────────────────────────────────────────────────────────────

const WIN_PROB: Record<string, number> = {
  low: 0.40,
  medium: 0.28,
  high: 0.15,
};

function simulate(rtp: number, volatility: string, betAmount: number, hasJackpot: boolean, jackpotAmount?: number) {
  // Tiny jackpot chance (0.1 %)
  if (hasJackpot && jackpotAmount && Math.random() < 0.001) {
    return { isWin: true, isJackpot: true, multiplier: +(jackpotAmount / betAmount).toFixed(2), winAmount: jackpotAmount };
  }

  const winProb = WIN_PROB[volatility] ?? 0.28;
  if (Math.random() >= winProb) {
    return { isWin: false, isJackpot: false, multiplier: 0, winAmount: 0 };
  }

  const avgMultiplier = rtp / (100 * winProb);
  const varianceFactor = 0.6 + Math.random() * 0.8;
  const multiplier = Math.max(1.0, +(avgMultiplier * varianceFactor).toFixed(2));
  const winAmount = +(betAmount * multiplier).toFixed(2);
  return { isWin: true, isJackpot: false, multiplier, winAmount };
}

// ────────────────────────────────────────────────────────────────────────────
// Controller
// ────────────────────────────────────────────────────────────────────────────

export const playGame = async (req: AuthRequest, res: Response) => {
  try {
    const { slug } = req.params;
    const { betAmount, currency = 'GC' } = req.body;

    if (!betAmount || betAmount <= 0) {
      return res.status(400).json({ message: 'Invalid bet amount' });
    }
    if (!['GC', 'SC'].includes(currency)) {
      return res.status(400).json({ message: 'Currency must be GC or SC' });
    }

    const game = await Game.findOne({ slug });
    if (!game) return res.status(404).json({ message: 'Game not found' });

    if (betAmount < game.minBet || betAmount > game.maxBet) {
      return res.status(400).json({
        message: `Bet must be between $${game.minBet} and $${game.maxBet}`,
      });
    }

    const user = await User.findById(req.userId);
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
      });
    }

    // ── Phase 1: debit the bet from True wallet (fall back to MongoDB) ────────
    const roundId = crypto.randomUUID();
    const betTxId = crypto.randomUUID();
    const gameId = String(game._id);

    let postBalances: any = null;
    let usedTrueEngine = false;

    try {
      const betResult = await trueWallet.bet(
        betTxId,
        user.truePlayerId,
        currency as 'GC' | 'SC',
        betAmount,
        gameId,
        roundId,
      );

      usedTrueEngine = true;

      // ── Phase 2: simulate game outcome ──────────────────────────────────────
      const outcome = simulate(game.rtp, game.volatility, betAmount, game.hasJackpot, game.jackpotAmount ?? undefined);

      postBalances = betResult?.result?.post_balances ?? null;
      const betLedgerTxId: string = betResult?.result?.ledger_transaction_id ?? '';

      // ── Phase 3: credit win to True wallet if player won ────────────────────
      if (outcome.isWin && outcome.winAmount > 0) {
        const winTxId = crypto.randomUUID();
        try {
          const winResult = await trueWallet.win(
            winTxId,
            user.truePlayerId,
            currency as 'GC' | 'SC',
            outcome.winAmount,
            gameId,
            roundId,
            betLedgerTxId,
          );
          postBalances = winResult?.result?.post_balances ?? postBalances;
        } catch (err) {
          console.error('True wallet win credit failed:', err);
        }
      }

      return res.json({
        isWin: outcome.isWin,
        isJackpot: outcome.isJackpot,
        multiplier: outcome.multiplier,
        betAmount,
        winAmount: outcome.winAmount,
        currency,
        balances: postBalances,
        engine: 'true',
      });
    } catch (err) {
      if (err instanceof TrueEngineError) {
        if (err.trueCode === 'INSUFFICIENT_FUNDS') {
          return res.status(400).json({ message: 'Insufficient balance' });
        }
        return res.status(err.httpStatus).json({ message: err.message });
      }
      // True engine unreachable — fall through to MongoDB mode
      if (usedTrueEngine) throw err; // unexpected error after successful bet, re-throw
    }

    // ── MongoDB fallback (True engine offline) ────────────────────────────────
    if (currency !== 'GC') {
      return res.status(400).json({ message: 'SC currency requires True engine to be online' });
    }
    if (user.balance < betAmount) {
      return res.status(400).json({ message: 'Insufficient balance' });
    }

    const outcome = simulate(game.rtp, game.volatility, betAmount, game.hasJackpot, game.jackpotAmount ?? undefined);

    const balanceBefore = user.balance;
    const newBalance = Math.round((balanceBefore - betAmount + outcome.winAmount) * 100) / 100;
    user.balance = newBalance;
    await user.save();

    await new Transaction({
      userId: req.userId,
      type: outcome.isWin ? 'win' : 'bet',
      amount: outcome.isWin ? outcome.winAmount : betAmount,
      status: 'completed',
      paymentMethod: 'game',
      balanceBefore,
      balanceAfter: newBalance,
      description: `${game.title} — ${outcome.isWin ? `win ×${outcome.multiplier}` : 'loss'}`,
    }).save();

    postBalances = {
      gc: newBalance.toFixed(4),
      sc_unplayed: '0.0000',
      sc_redeemable: '0.0000',
    };

    // ── Return result ─────────────────────────────────────────────────────────
    res.json({
      isWin: outcome.isWin,
      isJackpot: outcome.isJackpot,
      multiplier: outcome.multiplier,
      betAmount,
      winAmount: outcome.winAmount,
      currency,
      balances: postBalances,
      engine: 'mongodb-fallback',
    });
  } catch (error) {
    console.error('Play game error:', error);
    res.status(500).json({ message: 'Game play failed', error });
  }
};
