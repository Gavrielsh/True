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

// Operation label values for DBTransactionDuration. Bounded set — never put
// unbounded values (player ids, tx ids) in a label.
const (
	OpBet      = "bet"
	OpWin      = "win"
	OpRollback = "rollback"
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

	// DBTransactionDuration measures one full DB transaction — BEGIN through
	// COMMIT/ROLLBACK — for the money-moving paths. Because the wallet row is
	// locked FOR UPDATE for almost this entire window, this histogram is the
	// per-player lock-hold-time distribution; its upper tail is the direct
	// head-of-line blocking bound at 10k TPS.
	DBTransactionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "engine_db_transaction_duration_seconds",
		Help: "Duration of the core DB transaction (wallet FOR UPDATE lock window) per operation.",
		// Healthy transactions sit in single-digit milliseconds; the 5s ceiling
		// matches the handlers' defaultTxTimeout, past which pgx cancels.
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"operation"})
)

// ObserveDBTransaction records one DB transaction's elapsed time. Designed for
// a one-line defer at the top of the tx function:
//
//	defer metrics.ObserveDBTransaction(metrics.OpBet, time.Now())
func ObserveDBTransaction(operation string, start time.Time) {
	DBTransactionDuration.WithLabelValues(operation).Observe(time.Since(start).Seconds())
}
