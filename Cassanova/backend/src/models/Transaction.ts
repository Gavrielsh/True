import mongoose, { Schema, Document } from 'mongoose';

/**
 * Pillar 2 — Single Source of Truth.
 *
 * MongoDB is NO LONGER a copy of financial history. Completed deposits, bets,
 * wins and escrow movements live exclusively in the True Engine ledger. This
 * collection now persists only the OPS-WORKFLOW STATE of *pending manual
 * withdrawals*: a row is created when funds are escrow-reserved in the engine,
 * and it carries `trueEscrowTransactionId` so ops can later commit (pay out) or
 * release (refund) the matching engine reservation.
 *
 * Balance columns are gone — balances are the engine's domain.
 */
export interface ITransaction extends Document {
  userId: mongoose.Types.ObjectId;
  type: 'deposit' | 'withdrawal' | 'bet' | 'win' | 'bonus';
  amount: number; // integer cents
  status: 'pending' | 'completed' | 'failed' | 'cancelled';
  paymentMethod?: string;
  /** The True Engine escrow ledger_transaction_id this request reserved. */
  trueEscrowTransactionId?: string;
  description?: string;
  createdAt: Date;
  updatedAt: Date;
}

const TransactionSchema: Schema = new Schema(
  {
    userId: { type: Schema.Types.ObjectId, ref: 'User', required: true },
    type: {
      type: String,
      required: true,
      enum: ['deposit', 'withdrawal', 'bet', 'win', 'bonus'],
    },
    amount: { type: Number, required: true }, // integer cents
    status: {
      type: String,
      required: true,
      enum: ['pending', 'completed', 'failed', 'cancelled'],
      default: 'pending',
    },
    paymentMethod: { type: String },
    // Links the pending withdrawal to its engine escrow reservation so ops can
    // commit/release it. Unique (sparse) so we never double-record a reserve.
    trueEscrowTransactionId: { type: String, index: true, unique: true, sparse: true },
    description: { type: String },
  },
  {
    timestamps: true,
  }
);

export default mongoose.model<ITransaction>('Transaction', TransactionSchema);
