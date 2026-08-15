// Package metrics holds the engine's Prometheus instruments. Instruments are
// registered on the default registry at init via promauto, so wiring is just
// "import and call" — the /metrics endpoint (promhttp.Handler in the router)
// serves them with zero extra plumbing.
//
// All instruments are concurrency-safe; recording is lock-free atomics on the
// hot path, so calls are safe inside the FOR UPDATE window without extending it
// measurably.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Operation label values for DBLockDuration. Bounded set — never put
// unbounded values (player ids, tx ids) in a label.
const (
	OpBet      = "bet"
	OpWin      = "win"
	OpRollback = "rollback"
	// OpSpin is the server-authoritative game round: bet + win settled in a
	// single transaction under one wallet lock.
	OpSpin = "spin"
)

var (
	// GhostSpinsRecovered counts 23505 unique-violations on the ledger insert
	// that were successfully replayed as Ghost-Spin recoveries (architecture
	// §6.A). A non-zero rate is normal under operator retries; a SPIKE means
	// the Redis idempotency layer is dropping writes and every duplicate is
	// falling through to the DB anchor.
	GhostSpinsRecovered = promauto.NewCounter(prometheus.CounterOpts{
		Name: "engine_ghost_spins_recovered_total",
		Help: "Total ledger 23505 conflicts successfully replayed via Ghost-Spin recovery.",
	})

	// SpinsSettled counts completed server-authoritative rounds by game and
	// line result. Both labels are bounded (registered game ids; the three
	// LineResult values), so the cardinality stays fixed.
	//
	// OPERATIONAL USE: the ratio of line results is the live check that the
	// RNG is behaving. If THREE_OF_A_KIND drifts from its theoretical rate,
	// either the entropy source or the paytable has been tampered with.
	SpinsSettled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_spins_settled_total",
		Help: "Server-authoritative spins settled, by game id and line result.",
	}, []string{"game_id", "line"})

	// SpinWagerTotal and SpinPayoutTotal track realised turnover and payout
	// per game, in whole currency units. Their ratio is the REALISED RTP,
	// which alerting compares against the paytable's declared figure — the
	// operational counterpart to the compile-time RTP assertion in
	// internal/game.
	SpinWagerTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_spin_wager_total",
		Help: "Total amount wagered on server-authoritative spins, by game id and currency family.",
	}, []string{"game_id", "family"})

	SpinPayoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_spin_payout_total",
		Help: "Total amount paid out on server-authoritative spins, by game id and currency family.",
	}, []string{"game_id", "family"})

	// WinCeilingRejections counts third-party WIN credits refused for
	// exceeding the operator's configured ceiling.
	//
	// ANY increment is a page-the-humans event: a legitimate provider does
	// not pay above its own declared max win, so a rejection means either a
	// provider defect or a compromised webhook secret being used to mint
	// redeemable currency.
	WinCeilingRejections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "engine_win_ceiling_rejections_total",
		Help: "Third-party WIN credits rejected for exceeding the configured absolute ceiling.",
	})

	// LegacySignatureAccepted counts requests admitted via the TRANSITIONAL
	// body-only signature fallback, by operator.
	//
	// OPERATIONAL USE: this is the migration burndown. While it is non-zero
	// for an operator, that operator is still signing the old way and replay
	// protection is not in effect for them. Drive it to zero, then disable
	// HMAC_ACCEPT_LEGACY_SIGNATURE and redeploy.
	LegacySignatureAccepted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_legacy_signature_accepted_total",
		Help: "Requests accepted via the transitional body-only HMAC fallback, by operator.",
	}, []string{"operator"})

	// IdempotencyFingerprintMismatch counts idempotency keys presented with a
	// DIFFERENT request than the one that created them.
	//
	// A healthy integration never trips this: a retry replays the identical
	// body. A non-zero rate means an integrator is reusing keys across
	// distinct transactions — or someone is probing for cached results
	// belonging to another player.
	IdempotencyFingerprintMismatch = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_idempotency_fingerprint_mismatch_total",
		Help: "Idempotency keys reused with a materially different request, by operator.",
	}, []string{"operator"})

	// GeoBlockedRequests counts requests rejected by the jurisdiction fence,
	// per ISO-3166-2 region. Region codes come from the operator-configured
	// blocklist — a small bounded set, safe as a label.
	GeoBlockedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_geo_blocked_requests_total",
		Help: "Requests rejected by the geo-fence, by blocked ISO-3166-2 region.",
	}, []string{"region"})

	// ReconciliationMismatches counts (player, currency) pairs whose wallets
	// cache diverged from the ledger-derived balance. ANY increment is a
	// page-the-humans event — the ledger is the source of truth and the
	// reconciler never auto-corrects.
	ReconciliationMismatches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_reconciliation_mismatches_total",
		Help: "Wallet-vs-ledger balance mismatches found by the reconciliation worker, by currency.",
	}, []string{"currency"})

	// RateLimitedRequests counts 429 rejections by scope (operator|player) —
	// a bounded two-value label.
	RateLimitedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "engine_rate_limited_requests_total",
		Help: "Requests rejected by the sliding-window rate limiter, by scope.",
	}, []string{"scope"})

	// DBLockDuration measures one full DB execution window — BEGIN through
	// COMMIT/ROLLBACK — for the money-moving paths. Because the wallet row is
	// locked FOR UPDATE for almost this entire window, this histogram is the
	// per-player lock-hold-time distribution; its upper tail is the direct
	// head-of-line blocking bound at 10k TPS.
	DBLockDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "engine_db_lock_duration_seconds",
		Help: "Duration of DB execution window from SELECT FOR UPDATE row lock acquisition until COMMIT or ROLLBACK.",
		// Healthy transactions sit in single-digit milliseconds; the 5s ceiling
		// matches the handlers' defaultTxTimeout, past which pgx cancels.
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"operation"})
)

// ObserveDBLockDuration records one DB lock window's elapsed time. Designed
// for a one-line defer at the top of the tx function:
//
//	defer metrics.ObserveDBLockDuration(metrics.OpBet, time.Now())
func ObserveDBLockDuration(operation string, start time.Time) {
	DBLockDuration.WithLabelValues(operation).Observe(time.Since(start).Seconds())
}
