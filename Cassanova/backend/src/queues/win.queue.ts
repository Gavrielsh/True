/**
 * Win Credit Queue (Directive 2 — Guaranteed Orphaned Wins)
 *
 * All win-credit events are pushed here instead of being sent inline in the
 * HTTP request handler. A separate worker (workers/win.worker.ts) consumes
 * the queue and retries with exponential backoff until the True Engine
 * acknowledges a 200 OK. The player is NEVER orphaned.
 *
 * Idempotency: each job's BullMQ jobId is the client-supplied winTxId (a v4
 * UUID). If the client retries the /play request (network timeout), the
 * second enqueue with the same jobId is a no-op — BullMQ deduplicates it.
 * The True Engine's own idempotency cache provides a second layer of
 * protection against double-credit.
 *
 * Connection: BullMQ bundles its own ioredis internally. We pass plain
 * RedisOptions ({ host, port }) to avoid version conflicts with any
 * separately installed ioredis package.
 */

import { Queue } from 'bullmq';

export interface WinJobPayload {
  /** Client-generated v4 UUID — idempotency key for the True Engine /win call. */
  winTxId: string;
  playerId: string;
  currency: 'GC' | 'SC';
  /** Win amount expressed in integer CENTS (e.g. 375 = $3.75). */
  winAmountCents: number;
  gameId: string;
  roundId: string;
  /** Ledger transaction ID returned by the True Engine for the corresponding bet. */
  betLedgerTxId: string;
}

// Parse REDIS_URL (redis://host:port) into host/port, or fall back to defaults.
// Using plain RedisOptions avoids bundled-vs-standalone ioredis type conflicts.
function redisConnection() {
  const url = process.env.REDIS_URL || '';
  const match = url.match(/redis:\/\/(?:[^@]+@)?([^:]+):(\d+)/);
  return {
    host: match?.[1] || process.env.REDIS_HOST || 'localhost',
    port: parseInt(match?.[2] || process.env.REDIS_PORT || '6379', 10),
    maxRetriesPerRequest: null as null, // required by BullMQ
  };
}

// NameType is explicitly typed as `string` so queue.add('credit-win', ...) compiles
// without narrowing the literal to a union type.
export const winQueue = new Queue<WinJobPayload, void, string>('win-credits', {
  connection: redisConnection(),
  defaultJobOptions: {
    attempts: 10,
    backoff: {
      type: 'exponential',
      delay: 1_000, // 1 s → 2 s → 4 s → … → ~512 s on attempt 10
    },
    removeOnComplete: { count: 1_000 },
    removeOnFail: false, // Retain failed jobs for audit / manual replay
  },
});
