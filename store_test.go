package main

import (
	"path/filepath"
	"testing"
	"time"
)

// The bug this guards against actually shipped: Prune already holds s.mu and
// called rollupHours, which took s.mu again on the same goroutine. The store
// deadlocked on the first prune on device, taking the history loop and the log
// flusher with it, and nothing in the API could say why. A timeout turns that
// silent hang into a failing test.
func TestPruneWithRollupDoesNotDeadlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := openStore(path, nil)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()

	// Samples in *complete* past hours: the rollup deliberately skips the hour in
	// progress, so nothing in the current hour would exercise its SQL at all.
	now := time.Now()
	for _, back := range []time.Duration{3 * time.Hour, 2 * time.Hour, 2*time.Hour + time.Minute} {
		if err := st.InsertSample(sampleFromSnapshot(Snapshot{}, 1, now.Add(-back).Unix())); err != nil {
			t.Fatalf("InsertSample: %v", err)
		}
	}

	h := defaultHistoryConfig()
	h.Enabled = true
	h.RollupEnabled = true

	done := make(chan PruneResult, 1)
	go func() {
		res, _ := st.Prune(now, h)
		done <- res
	}()
	var res PruneResult
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Prune did not return within 15s — the store is deadlocked (check for a re-entrant s.mu lock)")
	}
	// The rollup runs inside Prune and reports its own failure instead of
	// aborting the prune, so a broken statement stayed invisible in the log
	// while never writing a single aggregate row.
	if res.RollupError != "" {
		t.Fatalf("rollup failed: %s", res.RollupError)
	}
	if res.RollupRows == 0 {
		t.Fatal("rollup wrote no hourly aggregates for samples in complete past hours")
	}
	roll, err := st.QueryRollup([]string{"battery_capacity_pct"}, now.Add(-6*time.Hour).Unix(), now.Unix(), 100)
	if err != nil {
		t.Fatalf("QueryRollup: %v", err)
	}
	if len(roll.Rows) == 0 || roll.Count == 0 {
		t.Fatalf("QueryRollup returned no rows after a successful rollup (%+v)", roll)
	}
}

// sampleColumns and Sample.values() are two hand-maintained lists that must stay
// in step; a mismatch writes misaligned rows instead of failing.
func TestSampleColumnsAndValuesStayInLockstep(t *testing.T) {
	sm := sampleFromSnapshot(Snapshot{}, 1, 1700000000)
	vals := sm.values()
	if len(vals) != len(sampleColumns) {
		t.Fatalf("Sample.values() returned %d values for %d columns", len(vals), len(sampleColumns))
	}
	if got := sampleFromSnapshot(Snapshot{}, 0, 1700000000).values(); len(got) != len(sampleColumns) {
		t.Fatalf("cheap sample: %d values for %d columns", len(got), len(sampleColumns))
	}
}

func TestInsertQueryAndBucketRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := openStore(path, nil)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()

	base := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	for i := 0; i < 5; i++ {
		sm := sampleFromSnapshot(Snapshot{}, 1, base.Unix()+int64(i*60))
		if err := st.InsertSampleAndSnapshot(sm, []byte(`{"probe":true}`)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	res, err := st.QuerySamples([]string{"ts", "battery_capacity_pct"}, base.Unix(), base.Unix()+600, 100, true)
	if err != nil {
		t.Fatalf("QuerySamples: %v", err)
	}
	if res.Count != 5 {
		t.Fatalf("QuerySamples returned %d rows, want 5", res.Count)
	}
	buckets, err := st.QuerySamplesBucketed([]string{"ts", "battery_capacity_pct"}, base.Unix(), base.Unix()+600, 300, 100)
	if err != nil {
		t.Fatalf("QuerySamplesBucketed: %v", err)
	}
	if len(buckets.Buckets) == 0 {
		t.Fatal("bucketed query returned no buckets for 5 samples over 5 minutes")
	}
	if _, _, err := st.SnapshotDoc(base.Unix()); err != nil {
		t.Fatalf("SnapshotDoc: %v", err)
	}
}

// Close() used to close a channel that openStore never created, so the process
// panicked on SIGTERM instead of shutting down.
func TestStoreCloseIsSafeAndRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := openStore(path, nil)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
