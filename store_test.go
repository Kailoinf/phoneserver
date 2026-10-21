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

	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		if err := st.InsertSample(sampleFromSnapshot(Snapshot{}, 1, now-int64(i*60))); err != nil {
			t.Fatalf("InsertSample: %v", err)
		}
	}

	h := defaultHistoryConfig()
	h.Enabled = true
	h.RollupEnabled = true

	done := make(chan PruneResult, 1)
	go func() {
		res, _ := st.Prune(time.Now(), h)
		done <- res
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Prune did not return within 15s — the store is deadlocked (check for a re-entrant s.mu lock)")
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
