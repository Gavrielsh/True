import { Router } from 'express';
import { authenticateToken } from '../middleware/auth.middleware';
import { getWalletBalances } from '../controllers/wallet.controller';

const router = Router();

router.use(authenticateToken);
router.get('/balances', getWalletBalances);

export default router;
