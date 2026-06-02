/**
 * Win Credit Worker (Directive 2 — Guaranteed Orphaned Wins)
 *
 * Consumes 'win-credits' queue jobs and calls the True Engine's /win endpoint.
 * BullMQ retries failed jobs with exponential backoff (configured on the queue).
 *
 * Key design decisions:
 *  - Transient failures (5xx, network): re-throw → BullMQ backoff + retry.
 *  - Permanent failures (4xx business errors): throw UnrecoverableError so
 *    BullMQ moves the job straight to "failed" without wasting retry budget.
 *  - winTxId is a client-generated UUID → the True Engine's idempotency cache
 *    guarantees no double-credit on any retry attempt.
 *
 * Production note: run this worker as an independent process (separate
 * Kubernetes pod / PM2 cluster worker) so it survives API server restarts.
 * For development, startWinWorker() is called from server.ts.
 */

import { Worker, Job, UnrecoverableError } from 'bullmq';
import { trueWallet, TrueEngineError } from '../services/trueWallet.service';
import { WinJobPayload } from '../queues/win.queue';

function redisConnection() {
  const url = process.env.REDIS_URL || '';
  const match = url.match(/redis:\/\/(?:[^@]+@)?([^:]+):(\d+)/);
  return {
    host: match?.[1] || process.env.REDIS_HOST || 'localhost',
    port: parseInt(match?.[2] || process.env.REDIS_PORT || '6379', 10),
    maxRetriesPerRequest: null as null,
  };
}

async function processWinJob(job: Job<WinJobPayload>): Promise<void> {
  const { winTxId, playerId, currency, winAmountCents, gameId, roundId, betLedgerTxId } = job.data;

  try {
    await trueWallet.win(
      winTxId,
      playerId,
      currency,
      winAmountCents,
      gameId,
      roundId,
      betLedgerTxId,
    );
  } catch (err) {
    if (err instanceof TrueEngineError && err.httpStatus >= 400 && err.httpStatus < 500) {
      // 4xx = business-rule violation; retrying will never succeed.
      // Move immediately to failed so the retry budget is not exhausted on
      // a permanent error (e.g. PLAYER_NOT_FOUND, ROLLBACK_ALREADY).
      throw new UnrecoverableError(
        `Win credit permanently rejected by True Engine [${err.trueCode}] ` +
        `winTxId=${winTxId}: ${err.message}`,
      );
    }
    // 5xx / network timeout — re-throw to trigger exponential backoff.
    throw err;
  }
}

export function startWinWorker(): Worker<WinJobPayload> {
  const worker = new Worker<WinJobPayload>('win-credits', processWinJob, {
    connection: redisConnection(),
    concurrency: 5,
  });

  worker.on('completed', (job) => {
    console.info(
      `[WinWorker] credited winTxId=${job.data.winTxId} ` +
      `player=${job.data.playerId} amount_cents=${job.data.winAmountCents}`,
    );
  });

  worker.on('failed', (job, err) => {
    console.error(
      `[WinWorker] FAILED jobId=${job?.id} winTxId=${job?.data.winTxId} ` +
      `attempt=${job?.attemptsMade}/${job?.opts.attempts}: ${err.message}`,
    );
  });

  worker.on('error', (err) => {
    console.error('[WinWorker] Worker-level error (Redis?):', err.message);
  });

  return worker;
}
