import { Response } from 'express';
import { AuthRequest } from '../middleware/auth.middleware';
import User from '../models/User';
import { trueWallet, TrueEngineError } from '../services/trueWallet.service';

/**
 * GET /api/wallet/balances
 *
 * Strict proxy — reads balances ONLY from the True Engine.
 * No MongoDB fallback. If the engine is unreachable, the client receives
 * 503 and must retry. A stale balance shown from a local DB is a financial
 * integrity violation and is never acceptable.
 */
export const getWalletBalances = async (req: AuthRequest, res: Response) => {
  try {
    const user = await User.findById(req.userId).select('truePlayerId');
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (!user.truePlayerId) {
      return res.status(400).json({
        message: 'Wallet not linked to transaction engine. Please contact support.',
        code: 'WALLET_NOT_PROVISIONED',
      });
    }

    try {
      const trueResponse = await trueWallet.getBalances(user.truePlayerId);
      return res.json({ balances: trueResponse.balances, source: 'true-engine' });
    } catch (err) {
      if (err instanceof TrueEngineError) {
        return res.status(err.httpStatus).json({ message: err.message, code: err.trueCode });
      }
      // True engine unreachable — fail-fast, never fall back to stale data.
      return res.status(503).json({
        message: 'Financial service temporarily unavailable. Please try again.',
        code: 'ENGINE_UNAVAILABLE',
      });
    }
  } catch (error) {
    console.error('Get wallet balances error:', error);
    res.status(500).json({ message: 'Failed to fetch wallet balances' });
  }
};
