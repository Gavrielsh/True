package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// gatherFamily returns the named metric family from the default registry, or
// nil if it has no series yet.
func gatherFamily(t *testing.T, name string) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		out := make(map[string]float64)
		for _, m := range mf.GetMetric() {
			key := ""
			for _, lp := range m.GetLabel() {
				key += lp.GetName() + "=" + lp.GetValue() + ";"
			}
			switch {
			case m.GetHistogram() != nil:
				out[key] = float64(m.GetHistogram().GetSampleCount())
			case m.GetCounter() != nil:
				out[key] = m.GetCounter().GetValue()
			}
		}
		return out
	}
	return nil
}

// The counter must be registered on the DEFAULT registry — that is what the
// router's promhttp.Handler() serves — and Inc must move it by exactly 1.
func TestGhostSpinsRecovered_RegisteredAndCounts(t *testing.T) {
	if gatherFamily(t, "engine_ghost_spins_recovered_total") == nil {
		t.Fatal("engine_ghost_spins_recovered_total not registered on the default registry")
	}
	before := testutil.ToFloat64(GhostSpinsRecovered)
	GhostSpinsRecovered.Inc()
	if got := testutil.ToFloat64(GhostSpinsRecovered); got != before+1 {
		t.Errorf("counter: got %v want %v", got, before+1)
	}
}

// ObserveDBLockDuration must create the labelled series on the default
// registry and record one sample with a plausible (non-negative, sub-second)
// value.
func TestObserveDBLockDuration_RecordsLabelledSample(t *testing.T) {
	start := time.Now().Add(-5 * time.Millisecond)
	for _, op := range []string{OpBet, OpWin, OpRollback} {
		ObserveDBLockDuration(op, start)
	}

	fam := gatherFamily(t, "engine_db_lock_duration_seconds")
	if fam == nil {
		t.Fatal("engine_db_lock_duration_seconds not registered on the default registry")
	}
	for _, op := range []string{OpBet, OpWin, OpRollback} {
		key := "operation=" + op + ";"
		if fam[key] < 1 {
			t.Errorf("operation=%s: sample count %v, want >= 1", op, fam[key])
		}
	}
}
