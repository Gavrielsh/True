import { Response } from 'express';
import { AuthRequest } from '../middleware/auth.middleware';
import User from '../models/User';
import { trueWallet } from '../services/trueWallet.service';

export const getWalletBalances = async (req: AuthRequest, res: Response) => {
  try {
    const user = await User.findById(req.userId);
    if (!user) return res.status(404).json({ message: 'User not found' });

    if (!user.truePlayerId) {
      // User pre-dates True integration — return MongoDB balance as GC
      return res.json({
        balances: {
          gc: user.balance.toFixed(4),
          sc_unplayed: '0.0000',
          sc_redeemable: '0.0000',
        },
        source: 'mongodb',
      });
    }

    try {
      const trueResponse = await trueWallet.getBalances(user.truePlayerId);
      return res.json({
        balances: trueResponse.balances,
        source: 'true-engine',
      });
    } catch {
      // True engine unavailable — fall back to MongoDB balance
      return res.json({
        balances: {
          gc: user.balance.toFixed(4),
          sc_unplayed: '0.0000',
          sc_redeemable: '0.0000',
        },
        source: 'mongodb-fallback',
      });
    }
  } catch (error) {
    console.error('Get wallet balances error:', error);
    res.status(500).json({ message: 'Failed to fetch wallet balances', error });
  }
};
