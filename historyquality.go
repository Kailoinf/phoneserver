// historyquality.go — makes every stored number interpretable.
//
// The samples table records the phone's metrics once per tick. On its own a
// row answers "what was the temperature", and nothing else: it cannot say
// whether the reading was actually taken, whether the clock it is stamped with
// can be trusted, whether the gap to the neighbouring row is the configured
// cadence, or whether a value was thrown away because it was impossible. Every
// question below is answered by data this file writes and reads back:
//
//	1. credibility metadata  tick_gap_ms / mono_ms / tz_offset_min / flags /
//	                         measured_cols / absent_cols / rejected_cols /
//	                         source_version (schema v3, see store.go)
//	2. value validation      a -1000 °C battery or a 250 % CPU reading is not
//	                         stored: the column is NULLed and counted as
//	                         rejected, so "impossible" never reads as "measured"
//	3. counter regressions   a counter that goes backwards inside one boot is a
//	                         glitch, not a measurement — except where the device
//	                         proves otherwise (see chargeCounter handling)
//	4. ts uniqueness         enforced by a UNIQUE index; duplicates are merged
//	                         at migration time keeping the fuller row
//	5. sampling holes        recorded as data (flags/gaps endpoints) so "no
//	                         data" is distinguishable from "the value was 0"
//	6. long-range queries    bucketed aggregation in SQL, plus a permanent
//	                         hourly rollup that outlives the raw samples
//
// Design rule for all of it: NULL means "not recorded", never "zero". A zero
// that nobody measured is the most expensive kind of wrong value, because it
// looks exactly like a real one.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- flag bits ----------

// FLAG_* describe how a row came to be. They are stored in samples.flags and
// counted by GET /api/v1/history/quality.
const (
	// FLAG_CHEAP: the row comes from a cheap tick, which reads sysfs and /proc
	// only. The columns in cheapAbsentColumns (Wi-Fi, package list, process
	// count, remaining charge times) are NULL here. Set by the writer; also
	// inferable from `full = 0`, kept as a bit so a reader needs only one
	// column to classify a row.
	FLAG_CHEAP = 1
	// FLAG_GAP: this row closed a sampling hole. Inferred from tick_gap_ms
	// exceeding 1.5x the configured interval — the first tick after the daemon
	// was restarted or the device slept. Readers that do not trust the bit can
	// recompute it from tick_gap_ms and interval_seconds, which is exactly why
	// the bit is redundant rather than the only source.
	FLAG_GAP = 2
	// FLAG_BOOT_RESET: uptime went backwards relative to the previous row, so
	// the device rebooted between them. Every since-boot counter (network
	// totals, charge counter) restarted with it, so their baselines are reset
	// instead of their values being rejected.
	FLAG_BOOT_RESET = 4
	// FLAG_CARRY: reserved for the read path. Raw rows never set it; it marks
	// values the query layer carried forward from an earlier full tick over a
	// cheap tick's NULL (reported as carried_columns). The bit exists so a
	// client that caches rows can be told which of them were synthesised.
	FLAG_CARRY = 8
	// FLAG_CLOCK_JUMP: the wall clock moved by more than the monotonic clock
	// did between two rows — NTP stepped it, or the date/timezone was changed.
	// The ts column is wall time, so a jump makes the row's ts disagree with
	// how long the phone actually ran. mono_ms carries the trustworthy axis.
	FLAG_CLOCK_JUMP = 16
)

// sampleQualityColumns are the columns schema v3 added to samples. Existing
// rows keep NULL in all of them: unknown, not zero. They are listed in the
// order they are appended to sampleColumns in store.go.
var sampleQualityColumns = []string{
	"tick_gap_ms", "mono_ms", "tz_offset_min", "measured_cols",
	"absent_cols", "rejected_cols", "flags", "source_version",
}

var sampleQualityColumnSet = func() map[string]bool {
	m := make(map[string]bool, len(sampleQualityColumns))
	for _, c := range sampleQualityColumns {
		m[c] = true
	}
	return m
}()

// flagNames maps a bit to its API name, so the report and the documentation
// cannot drift apart.
var flagNames = []struct {
	bit  int64
	name string
}{
	{FLAG_CHEAP, "cheap_tick"},
	{FLAG_GAP, "sampling_gap"},
	{FLAG_BOOT_RESET, "boot_reset"},
	{FLAG_CARRY, "carried_forward"},
	{FLAG_CLOCK_JUMP, "clock_jump"},
}

// ---------- value validation ----------

// sampleBounds is the plausible range of each metric, in the unit the column
// stores. A value outside it is a collector glitch (a file that held garbage,
// a sensor read that failed): storing it would put a -1000 °C battery into the
// long-term data where nothing downstream can tell it from a cold morning.
//
// Only ranges that are physically certain are listed. cpu_load1 is deliberately
// absent: a load average above the core count is legitimate, and rejecting it
// would delete real data. Values are inclusive.
var sampleBounds = map[string]struct{ min, max float64 }{
	"battery_capacity_pct": {0, 100},
	"battery_health_pct":   {0, 100},
	"battery_temp_c":       {-40, 100},
	"max_temp_c":           {-40, 150},
	"gpu_temp_c":           {-40, 150},
	"cpu_usage_pct":        {0, 100},
	"mem_used_pct":         {0, 100},
	"data_used_pct":        {0, 100},
	"wifi_signal_pct":      {0, 100},
	"gpu_busy_pct":         {0, 100},
	"wifi_rssi_dbm":        {-120, 0},
	"uptime_s":             {0, 1e9},   // ~31 years of uptime is the ceiling
	"brightness":           {0, 1 << 20},
	"brightness_max":       {0, 1 << 20},
	"battery_voltage_v":    {0, 30},
	"usb_voltage_v":        {0, 60},
	"mem_total_kb":         {0, 1 << 40},
	"mem_available_kb":     {0, 1 << 40},
	"swap_free_kb":         {0, 1 << 40},
	"data_free_bytes":      {0, 1 << 50},
	"data_total_bytes":     {0, 1 << 50},
	"root_free_bytes":      {0, 1 << 50},
	"net_rx_bytes":         {0, 1 << 50},
	"net_tx_bytes":         {0, 1 << 50},
	"cpu_cores":            {0, 4096},
	"proc_count":           {0, 100000},
	"pkg_total":            {0, 100000},
	"pkg_user":             {0, 100000},
	"pkg_system":           {0, 100000},
	"pkg_disabled":         {0, 100000},
	"thermal_zone_count":   {0, 4096},
	"collect_ms":           {0, 3600000},
}

// monotonicCounters are cumulative since boot. Inside one boot the only
// possible movement is up; a decrease means the kernel reset the counter (an
// interface went down and came back — measured: net_rx_bytes really does drop
// to 0 on this device when the Wi-Fi link re-establishes). A negative delta
// would otherwise be reported as a negative rate, so the row is rejected
// instead.
var monotonicCounters = []string{"net_rx_bytes", "net_tx_bytes"}

// chargeCounterColumn is the charge counter, which is *not* monotonic here:
// across 504 stored rows it decreased in 200 of them because it is the
// remaining charge, not a lifetime total. A regression check on it would throw
// away 40% of the real readings. What is rejected instead is a discontinuity no
// battery can produce in one tick.
const chargeCounterColumn = "battery_charge_mah"

// clockJumpMS is how far the wall clock may drift from the monotonic clock
// between two rows before the row is flagged. ts is stored in whole seconds,
// so each end can be off by up to a second; 2 s leaves that truncation out
// while still catching an NTP step or a date change.
const clockJumpMS = 2000

// warnIntervalS rate-limits the warning log per subject. A device with a
// broken thermal zone reports the same rejection every tick; 10 seconds of
// logging would drown out everything else in the file.
const warnIntervalS = 60

// QualityState is the per-loop baseline the checks compare against. It is
// carried in memory between ticks and seeded from the database at startup, so
// the comparisons do not lose their meaning when the daemon restarts.
type QualityState struct {
	HasPrev    bool
	PrevTs     int64
	PrevMonoMS int64 // monotonic ms since process start; 0 = unknown (after a restart)
	PrevUptime float64
	// PrevCounters holds the last accepted value of every counter column.
	PrevCounters map[string]float64
	warnLast     map[string]int64
}

// EvalResult is one tick's verdict.
type EvalResult struct {
	Flags     int64
	Rejected  []string
	Warnings  []string
	TickGapMS int64
	// Next is the baseline to adopt if the row is stored. A failed insert must
	// not advance it, or the next tick would compare against a value that was
	// never persisted and could report a phantom regression.
	Next QualityState
}

// Evaluate decides what a row is allowed to claim. It mutates sm (NULLing what
// it rejects and filling the quality columns) and returns the verdict plus the
// next baseline; it is deterministic given (sm, state, cfg, monoMS), which is
// what makes it testable without a phone.
func (st QualityState) Evaluate(sm *Sample, cfg HistoryConfig, monoMS int64) EvalResult {
	res := EvalResult{}
	next := st
	next.warnLast = copyWarnMap(st.warnLast)
	if next.PrevCounters == nil {
		next.PrevCounters = map[string]float64{}
	}
	now := time.Now().Unix()
	warn := func(key, format string, args ...any) {
		if next.warnLast == nil {
			next.warnLast = map[string]int64{}
		}
		// Same subject within a minute: already logged.
		if last, ok := next.warnLast[key]; ok && last > now-warnIntervalS {
			return
		}
		next.warnLast[key] = now
		res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...))
	}

	// --- 1. how long since the previous tick, on a clock that cannot be
	// stepped ------------------------------------------------------------
	if st.HasPrev {
		known := false
		gapMS := int64(0)
		switch {
		case st.PrevMonoMS > 0 && monoMS > 0:
			gapMS, known = monoMS-st.PrevMonoMS, true
		case sm.Ts > 0 && st.PrevTs > 0:
			// Only wall time is available (first tick after a restart): use it,
			// but the row's flags will show a clock jump if it was stepped.
			gapMS, known = (sm.Ts-st.PrevTs)*1000, true
		}
		if known {
			if gapMS < 0 {
				gapMS = 0
			}
			sm.TickGapMS = gapMS
			res.TickGapMS = gapMS
			tol := int64(cfg.IntervalSeconds) * 1500
			if tol <= 0 {
				tol = 15000
			}
			if gapMS > tol {
				res.Flags |= FLAG_GAP
				warn("gap", "sampling gap of %d ms at ts=%d (configured cadence %ds): rows are missing in between",
					gapMS, sm.Ts, cfg.IntervalSeconds)
			}
		} else {
			// First tick ever for this loop and nothing in the database: the
			// interval to the previous row is genuinely unknown. NULL.
			sm.markAbsent("tick_gap_ms")
		}
		if st.PrevMonoMS > 0 && monoMS > 0 {
			wall := (sm.Ts - st.PrevTs) * 1000
			mono := monoMS - st.PrevMonoMS
			if d := wall - mono; d > clockJumpMS || d < -clockJumpMS {
				res.Flags |= FLAG_CLOCK_JUMP
				warn("clock_jump", "wall clock moved %.1fs relative to the monotonic clock between ts=%d and ts=%d (NTP step or date change); ts is wall time, mono_ms is the trustworthy axis",
					float64(d)/1000, st.PrevTs, sm.Ts)
			}
		}
	} else {
		sm.markAbsent("tick_gap_ms")
	}

	// --- 2. boot detection ----------------------------------------------
	bootReset := false
	if st.HasPrev && st.PrevUptime > 0 && sm.UptimeS > 0 && sm.UptimeS < st.PrevUptime-1 {
		bootReset = true
		res.Flags |= FLAG_BOOT_RESET
		// The reboot itself is the explanation for the gap, the counter reset
		// and the discontinuity — one flag covers all three, which is why the
		// counters are re-based rather than rejected below.
		warn("boot_reset", "uptime went backwards (%.0fs -> %.0fs): the device rebooted between rows; since-boot counters are re-based",
			st.PrevUptime, sm.UptimeS)
	}

	// --- 3. range validation --------------------------------------------
	rejected := []string{}
	for col, b := range sampleBounds {
		v, ok := sampleValue(sm, col)
		if !ok {
			continue // string column, or not measured this tick
		}
		if _, absent := sm.absent[col]; absent {
			continue
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < b.min || v > b.max {
			sm.markAbsent(col)
			rejected = append(rejected, col)
			warn("range:"+col, "%s = %v is outside the plausible range [%v, %v] (ts=%d): stored as NULL and counted as rejected",
				col, v, b.min, b.max, sm.Ts)
		}
	}

	// --- 4. counters ------------------------------------------------------
	for _, col := range monotonicCounters {
		if _, absent := sm.absent[col]; absent {
			continue
		}
		v, ok := sampleValue(sm, col)
		if !ok {
			continue
		}
		prev, hadPrev := st.PrevCounters[col]
		if !bootReset && st.HasPrev && hadPrev && prev > 0 && v < prev {
			sm.markAbsent(col)
			rejected = append(rejected, col)
			warn("counter:"+col, "%s went backwards (%v -> %v) without a reboot (ts=%d): the kernel counter reset, stored as NULL rather than as a negative delta",
				col, prev, v, sm.Ts)
			continue
		}
		next.PrevCounters[col] = v
	}

	// The charge counter: a discontinuity, not a regression.
	if _, absent := sm.absent[chargeCounterColumn]; !absent {
		if v, ok := sampleValue(sm, chargeCounterColumn); ok {
			full, haveFull := sampleValue(sm, "battery_full_mah")
			prev, hadPrev := st.PrevCounters[chargeCounterColumn]
			switch {
			case v < 0:
				sm.markAbsent(chargeCounterColumn)
				rejected = append(rejected, chargeCounterColumn)
				warn("charge:", "%s = %v is negative (ts=%d): rejected", chargeCounterColumn, v, sm.Ts)
			case !bootReset && hadPrev && haveFull && full > 0 && math.Abs(v-prev) > full*0.5:
				// Half the battery's capacity in one tick is not a charge or
				// discharge, it is a re-read of a counter that lost its state.
				sm.markAbsent(chargeCounterColumn)
				rejected = append(rejected, chargeCounterColumn)
				warn("charge:", "%s jumped by %v mAh in one tick with a %.0f mAh battery (ts=%d): rejected as a discontinuity",
					chargeCounterColumn, math.Abs(v-prev), full, sm.Ts)
			default:
				next.PrevCounters[chargeCounterColumn] = v
			}
		}
	}

	// --- 5. describe the row ---------------------------------------------
	sort.Strings(rejected)
	res.Rejected = rejected
	if len(rejected) == 0 {
		sm.RejectedCols = "[]"
	} else if b, err := json.Marshal(rejected); err == nil {
		sm.RejectedCols = string(b)
	}
	absent := make([]string, 0, len(sm.absent))
	for c := range sm.absent {
		absent = append(absent, c)
	}
	sort.Strings(absent)
	if len(absent) == 0 {
		sm.AbsentCols = "[]"
	} else if b, err := json.Marshal(absent); err == nil {
		sm.AbsentCols = string(b)
	}
	// measured_cols counts the metric columns that really carry a reading. It
	// is the one number that says "trust this row" without cross-referencing
	// absent_cols, and the denominator (72-8) is asserted by a unit test so the
	// column list and this count cannot drift apart silently.
	metricCols := metricColumnCount()
	missing := 0
	for c := range sm.absent {
		if !sampleQualityColumnSet[c] {
			missing++
		}
	}
	sm.MeasuredCols = int64(metricCols - missing)
	if sm.Full == 0 {
		res.Flags |= FLAG_CHEAP
	}
	sm.Flags = res.Flags
	sm.MonoMS = monoMS
	// Store the offset the row's own timestamp was interpreted with, so a
	// later reader can reconstruct local time without assuming the phone's
	// current zone.
	if _, off := time.Unix(sm.Ts, 0).Zone(); true {
		sm.TzOffsetMin = int64(off / 60)
	}
	// Which build wrote the row: a schema change or a collector change makes
	// old rows explainable instead of merely old.
	sm.SourceVersion = version + "/s" + strconv.Itoa(schemaVersion)

	next.HasPrev = true
	next.PrevTs = sm.Ts
	next.PrevMonoMS = monoMS
	if sm.UptimeS > 0 {
		next.PrevUptime = sm.UptimeS
	}
	return res
}

func copyWarnMap(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sampleFieldIndex maps a column name to the struct field that holds it, via
// the json tag. Driven by the tag rather than a hand-written switch so a new
// metric cannot be added to Sample and silently left unchecked here.
var sampleFieldIndex = func() map[string]int {
	m := map[string]int{}
	t := reflect.TypeOf(Sample{})
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			m[tag] = i
		}
	}
	return m
}()

// sampleValue reads a numeric column from a Sample. The second result is false
// for text columns and for names that are not columns at all.
func sampleValue(sm *Sample, col string) (float64, bool) {
	i, ok := sampleFieldIndex[col]
	if !ok {
		return 0, false
	}
	v := reflect.ValueOf(sm).Elem().Field(i)
	switch v.Kind() {
	case reflect.Float64:
		return v.Float(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), true
	}
	return 0, false
}

// ---------- reading the quality back ----------

type ColumnCoverage struct {
	Metric    string  `json:"metric"`
	Rows      int64   `json:"rows"`
	NullRows  int64   `json:"null_rows"`
	NullRatio float64 `json:"null_ratio"`
	Rejected  int64   `json:"rejected_values"`
}

type GapRec struct {
	FromTS      int64 `json:"from_ts"`
	ToTS        int64 `json:"to_ts"`
	GapMS       int64 `json:"gap_ms"`
	RowsMissing int64 `json:"rows_missing"`
}

type CoverageBlock struct {
	FirstTS         int64   `json:"first_ts"`
	LastTS          int64   `json:"last_ts"`
	Rows            int64   `json:"rows"`
	ExpectedRows    int64   `json:"expected_rows"`
	CoveragePct     float64 `json:"coverage_pct"`
	IntervalSeconds int     `json:"interval_seconds"`
	Note            string  `json:"note"`
}

type FlagBlock struct {
	RowsTotal           int64            `json:"rows"`
	RowsWithoutMetadata int64            `json:"rows_without_quality_metadata"`
	RowsWithAnyFlag     int64            `json:"rows_with_any_flag"`
	Bits                map[string]int64 `json:"bits"`
	BitValues           map[string]int   `json:"bit_values"`
}

type GapsResult struct {
	Since           int64    `json:"since"`
	Until           int64    `json:"until"`
	MinGapMS        int64    `json:"min_gap_ms"`
	IntervalSeconds int      `json:"interval_seconds"`
	Rows            int64    `json:"rows"`
	GapsFound       int64    `json:"gaps_found"`
	Count           int      `json:"count"`
	Gaps            []GapRec `json:"gaps"`
	Truncated       bool     `json:"truncated"`
	Note            string   `json:"note"`
}

type QualityReport struct {
	Since             int64                  `json:"since"`
	Until             int64                  `json:"until"`
	IntervalSeconds   int                    `json:"interval_seconds"`
	SchemaVersion     int                    `json:"schema_version"`
	SchemaExpected    int                    `json:"schema_expected"`
	MetricColumns     int                    `json:"metric_columns"`
	Coverage          CoverageBlock          `json:"coverage"`
	NullRatioTop      []ColumnCoverage       `json:"null_ratio_top"`
	ColumnsWithNulls  int                    `json:"columns_with_nulls"`
	RejectedByColumn  map[string]int64       `json:"rejected_by_column"`
	RejectedRows      int64                  `json:"rejected_rows"`
	Flags             FlagBlock              `json:"flags"`
	LongestGaps       []GapRec               `json:"longest_gaps"`
	GapsFound         int64                  `json:"gaps_found"`
	DuplicateTS       int64                  `json:"duplicate_ts_rows"`
	DedupedAtMigrate  int64                  `json:"deduped_rows_at_migration"`
	CarriedColumns    []string               `json:"carried_columns"`
	DB                map[string]any         `json:"db"`
	Rollup            map[string]any         `json:"rollup"`
	Notes             map[string]string      `json:"notes"`
}

// QueryGaps returns the holes in the sampling cadence. The comparison is
// between consecutive stored rows, which is the only thing the database can
// actually prove: a gap means rows are missing, not that the phone was asleep.
func (s *Store) QueryGaps(since, until, minGapMS int64, intervalSeconds, limit int) (*GapsResult, error) {
	if until <= 0 {
		until = time.Now().Unix()
	}
	if since <= 0 {
		since = until - 24*3600
	}
	if intervalSeconds <= 0 {
		intervalSeconds = 10
	}
	if minGapMS <= 0 {
		// 2x the cadence: one missing row. Below that, ordinary scheduling
		// jitter would be reported as a hole.
		minGapMS = int64(intervalSeconds) * 2 * 1000
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 2000 {
		limit = 2000
	}
	out := &GapsResult{Since: since, Until: until, MinGapMS: minGapMS, IntervalSeconds: intervalSeconds,
		Gaps: make([]GapRec, 0, 16),
		Note: "rows_missing counts the samples the configured cadence would have produced inside the gap " +
			"(gap_ms/interval - 1, at least 1); it is an estimate from the interval, not a record of what was attempted"}
	var minS int64
	rows, err := s.db.Query(
		`SELECT (ts - prev_ts) FROM (
		   SELECT ts, LAG(ts) OVER (ORDER BY ts) AS prev_ts
		   FROM samples WHERE ts >= ? AND ts <= ?)
		 WHERE prev_ts IS NOT NULL AND (ts - prev_ts) * 1000 >= ?`, since, until, minGapMS)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return nil, err
		}
		_ = g
		minS++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.GapsFound = minS
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE ts >= ? AND ts <= ?`, since, until).
		Scan(&out.Rows); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(
		`SELECT prev_ts, ts, (ts - prev_ts) FROM (
		   SELECT ts, LAG(ts) OVER (ORDER BY ts) AS prev_ts
		   FROM samples WHERE ts >= ? AND ts <= ?)
		 WHERE prev_ts IS NOT NULL AND (ts - prev_ts) * 1000 >= ?
		 ORDER BY (ts - prev_ts) DESC LIMIT ?`, since, until, minGapMS, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var from, to, gapS int64
		if err := rows.Scan(&from, &to, &gapS); err != nil {
			rows.Close()
			return nil, err
		}
		miss := gapS/int64(intervalSeconds) - 1
		if miss < 1 {
			miss = 1
		}
		out.Gaps = append(out.Gaps, GapRec{FromTS: from, ToTS: to, GapMS: gapS * 1000, RowsMissing: miss})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Count = len(out.Gaps)
	out.Truncated = int64(out.Count) < out.GapsFound
	return out, nil
}

// QueryQuality is the credibility report for a window: how much of the
// configured cadence is actually present, which columns are habitually missing,
// what was rejected and why, and whether the long-range trend survived.
func (s *Store) QueryQuality(since, until int64, topN int, h HistoryConfig) (*QualityReport, error) {
	if until <= 0 {
		until = time.Now().Unix()
	}
	if since <= 0 {
		since = until - 24*3600
	}
	if topN <= 0 {
		topN = 10
	}
	if topN > len(sampleColumns) {
		topN = len(sampleColumns)
	}
	rep := &QualityReport{Since: since, Until: until, IntervalSeconds: h.IntervalSeconds,
		MetricColumns: metricColumnCount(),
		SchemaExpected: schemaVersion,
		RejectedByColumn: map[string]int64{}, CarriedColumns: []string{},
		DB: map[string]any{}, Rollup: map[string]any{},
		Notes: map[string]string{}}
	if v, ok := s.metaGet("schema"); ok {
		fmt.Sscanf(v, "%d", &rep.SchemaVersion)
	}
	if v, ok := s.metaGet("samples_deduped_total"); ok {
		fmt.Sscanf(v, "%d", &rep.DedupedAtMigrate)
	}

	// Coverage: what the cadence would have produced versus what is there.
	if err := s.db.QueryRow(`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM samples
		WHERE ts >= ? AND ts <= ?`, since, until).Scan(&rep.Coverage.FirstTS, &rep.Coverage.LastTS, &rep.Coverage.Rows); err != nil {
		return nil, err
	}
	rep.Coverage.IntervalSeconds = h.IntervalSeconds
	if rep.Coverage.Rows > 1 && h.IntervalSeconds > 0 {
		span := rep.Coverage.LastTS - rep.Coverage.FirstTS
		rep.Coverage.ExpectedRows = span/int64(h.IntervalSeconds) + 1
		if rep.Coverage.ExpectedRows > 0 {
			rep.Coverage.CoveragePct = round2(float64(rep.Coverage.Rows) / float64(rep.Coverage.ExpectedRows) * 100)
		}
		rep.Coverage.Note = fmt.Sprintf("expected_rows is inferred from the configured %ds cadence over the covered span, "+
			"so coverage below 100%% means ticks were missed, skipped or longer than the cadence", h.IntervalSeconds)
	} else {
		rep.Coverage.Note = "fewer than two rows in the window: a coverage ratio would be meaningless"
	}

	// NULL ratio per column: one pass with a CASE per column, so the whole
	// report costs one table scan rather than one query per metric.
	sel := make([]string, 0, len(sampleColumns))
	for _, c := range sampleColumns {
		if c == "ts" {
			continue
		}
		sel = append(sel, "SUM(CASE WHEN "+c+" IS NULL THEN 1 ELSE 0 END)")
	}
	q := "SELECT " + strings.Join(sel, ", ") + " FROM samples WHERE ts >= ? AND ts <= ?"
	vals := make([]any, len(sel))
	ptrs := make([]any, len(sel))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := s.db.QueryRow(q, since, until).Scan(ptrs...); err != nil {
		return nil, err
	}
	cov := make([]ColumnCoverage, 0, len(sel))
	i := 0
	for _, c := range sampleColumns {
		if c == "ts" {
			continue
		}
		var n int64
		if f, ok := asFloat(vals[i]); ok {
			n = int64(f)
		}
		i++
		if n > 0 {
			rep.ColumnsWithNulls++
		}
		ratio := 0.0
		if rep.Coverage.Rows > 0 {
			ratio = round4(float64(n) / float64(rep.Coverage.Rows))
		}
		cov = append(cov, ColumnCoverage{Metric: c, Rows: rep.Coverage.Rows, NullRows: n, NullRatio: ratio})
	}
	// Columns carried forward on read are exactly those the cheap tick cannot
	// measure and that really are NULL here: reporting the configured list
	// would claim carrying happened even in a window where it did not.
	for _, c := range cheapAbsentColumns {
		for _, cc := range cov {
			if cc.Metric == c && cc.NullRows > 0 {
				rep.CarriedColumns = append(rep.CarriedColumns, c)
				break
			}
		}
	}
	sort.Strings(rep.CarriedColumns)
	byRatio := append([]ColumnCoverage(nil), cov...)
	sort.Slice(byRatio, func(a, b int) bool {
		if byRatio[a].NullRatio != byRatio[b].NullRatio {
			return byRatio[a].NullRatio > byRatio[b].NullRatio
		}
		return byRatio[a].Metric < byRatio[b].Metric
	})
	if len(byRatio) > topN {
		byRatio = byRatio[:topN]
	}
	rep.NullRatioTop = byRatio

	// Which values were thrown away, by column. This is parsed from the stored
	// rejected_cols text rather than with SQLite's JSON functions: the report
	// must not depend on a compile-time JSON1 feature being present, and the
	// number of rows involved is by definition small.
	rows, err := s.db.Query(`SELECT rejected_cols FROM samples
		WHERE ts >= ? AND ts <= ? AND rejected_cols IS NOT NULL AND rejected_cols <> '[]' LIMIT 20000`, since, until)
	if err != nil {
		return nil, err
	}
	scanned := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		scanned++
		var cols []string
		if json.Unmarshal([]byte(raw), &cols) == nil {
			for _, c := range cols {
				rep.RejectedByColumn[c]++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM samples
		WHERE ts >= ? AND ts <= ? AND rejected_cols IS NOT NULL AND rejected_cols <> '[]'`, since, until).
		Scan(&rep.RejectedRows); err != nil {
		return nil, err
	}
	if scanned > 0 && int64(scanned) < rep.RejectedRows {
		rep.Notes["rejected_scan"] = fmt.Sprintf("rejected_by_column was tallied from the first %d of %d rows with rejections", scanned, rep.RejectedRows)
	}

	// Flags. One pass, one SUM per bit; the bit values are reported next to the
	// counts so a client can decode flags itself.
	fb := &rep.Flags
	fb.Bits = map[string]int64{}
	fb.BitValues = map[string]int{}
	exprs := []string{"COUNT(*)"}
	for _, f := range flagNames {
		fb.BitValues[f.name] = int(f.bit)
		exprs = append(exprs, fmt.Sprintf("SUM(CASE WHEN flags & %d THEN 1 ELSE 0 END)", f.bit))
	}
	exprs = append(exprs,
		"SUM(CASE WHEN flags IS NULL THEN 1 ELSE 0 END)",
		"SUM(CASE WHEN flags IS NOT NULL AND flags <> 0 THEN 1 ELSE 0 END)")
	dest := make([]any, len(exprs))
	ptrs2 := make([]any, len(exprs))
	for i := range dest {
		ptrs2[i] = &dest[i]
	}
	if err := s.db.QueryRow(`SELECT `+strings.Join(exprs, ", ")+
		` FROM samples WHERE ts >= ? AND ts <= ?`, since, until).Scan(ptrs2...); err != nil {
		return nil, err
	}
	if n, ok := asFloat(dest[0]); ok {
		fb.RowsTotal = int64(n)
	}
	for i, f := range flagNames {
		if n, ok := asFloat(dest[i+1]); ok {
			fb.Bits[f.name] = int64(n)
		}
	}
	if n, ok := asFloat(dest[len(flagNames)+1]); ok {
		fb.RowsWithoutMetadata = int64(n)
	}
	if n, ok := asFloat(dest[len(flagNames)+2]); ok {
		fb.RowsWithAnyFlag = int64(n)
	}

	// Holes: the same computation as /api/v1/history/gaps, newest-worst first.
	gaps, err := s.QueryGaps(since, until, 0, h.IntervalSeconds, 10)
	if err != nil {
		return nil, err
	}
	rep.LongestGaps = gaps.Gaps
	rep.GapsFound = gaps.GapsFound
	rep.DuplicateTS = func() int64 {
		var n int64
		s.db.QueryRow(`SELECT COUNT(*) FROM (SELECT ts FROM samples WHERE ts >= ? AND ts <= ?
			GROUP BY ts HAVING COUNT(*) > 1)`, since, until).Scan(&n)
		return n
	}()

	// Database-level facts, so the report answers "can I trust this file" as
	// well as "can I trust these numbers".
	if st, err := s.Stats(h); err == nil {
		rep.DB["bytes"] = st.DBBytes
		rep.DB["mb"] = st.DBMB
		rep.DB["samples"] = st.Samples
		rep.DB["oldest_ts"] = st.OldestTS
		rep.DB["newest_ts"] = st.NewestTS
		rep.DB["bytes_per_day"] = st.BytesPerDay
		rep.DB["growth_note"] = st.GrowthNote
		rep.DB["est_days_to_cap"] = st.EstDaysToCap
		rep.DB["rows_with_rejected_values"] = st.RejectedSamples
		rep.DB["deduped_samples_total"] = st.DedupedSamples
		rep.DB["dropped_log_lines"] = st.DroppedLogLines
		rep.DB["pruned_total"] = st.PrunedTotal
	} else {
		rep.DB["error"] = err.Error()
	}
	rep.Rollup["rows"] = func() int64 {
		var n int64
		s.db.QueryRow(`SELECT COUNT(*) FROM rollup_hourly`).Scan(&n)
		return n
	}()
	var oldest, newest int64
	s.db.QueryRow(`SELECT COALESCE(MIN(ts_hour),0), COALESCE(MAX(ts_hour),0) FROM rollup_hourly`).Scan(&oldest, &newest)
	rep.Rollup["oldest_ts_hour"] = oldest
	rep.Rollup["newest_ts_hour"] = newest
	rep.Rollup["enabled"] = h.RollupEnabled
	rep.Rollup["retention_days"] = h.RollupRetentionDays
	rep.Rollup["note"] = "rollup_hourly keeps one avg/min/max/last per hour and metric after the raw samples are pruned; " +
		"rollup_retention_days=0 keeps it forever"

	rep.Notes["null_ratio_top"] = fmt.Sprintf("the %d columns with the largest share of NULL rows in this window; "+
		"a high ratio on a column in carried_columns is expected (cheap ticks cannot measure it), "+
		"a high ratio elsewhere means the collector failed", topN)
	rep.Notes["rejected_by_column"] = "values that failed the plausibility checks (range, counter regression). They are NULL in the row and counted here; " +
		"the row itself is kept so the hole is visible"
	rep.Notes["coverage_pct"] = "rows / expected_rows, both counted inside the queried window"
	return rep, nil
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

// ---------- HTTP ----------

// handleHistoryQuality: GET /api/v1/history/quality?since=-7d&until=now&limit=10
func (a *App) handleHistoryQuality(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	q := r.URL.Query()
	h := a.config().History.norm()
	until := parseTimeParam(q.Get("until"), time.Now().Unix())
	since := parseTimeParam(q.Get("since"), until-24*3600)
	if since > until {
		since, until = until, since
	}
	topN := 10
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		topN = v
	}
	rep, err := a.store.QueryQuality(since, until, topN, h)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": map[string]any{
			"since": since, "until": until, "top_n": topN,
			"since_human": time.Unix(since, 0).Format("2006-01-02 15:04:05"),
			"until_human": time.Unix(until, 0).Format("2006-01-02 15:04:05"),
		},
		"report": rep,
		"note": "every number here is derived from what is stored: coverage compares stored rows with what the " +
			"configured cadence would have produced, flags say how each row was produced, and rejected_by_column " +
			"says what was refused as impossible rather than stored",
	})
}

// handleHistoryGaps: GET /api/v1/history/gaps?since=-24h&until=now&min_gap_ms=20000&limit=10
func (a *App) handleHistoryGaps(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	q := r.URL.Query()
	h := a.config().History.norm()
	until := parseTimeParam(q.Get("until"), time.Now().Unix())
	since := parseTimeParam(q.Get("since"), until-24*3600)
	if since > until {
		since, until = until, since
	}
	minGap := int64(0)
	if v, err := strconv.ParseInt(q.Get("min_gap_ms"), 10, 64); err == nil && v > 0 {
		minGap = v
	}
	limit := 10
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	res, err := a.store.QueryGaps(since, until, minGap, h.IntervalSeconds, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": map[string]any{
			"since": since, "until": until,
			"min_gap_ms": res.MinGapMS, "limit": limit,
			"since_human": time.Unix(since, 0).Format("2006-01-02 15:04:05"),
			"until_human": time.Unix(until, 0).Format("2006-01-02 15:04:05"),
		},
		"result": res,
	})
}

// handleHistoryRollup serves the hourly trend. It is also reachable as
// GET /api/v1/history?rollup=1 when the dedicated route is not wired.
func (a *App) handleHistoryRollup(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	q := r.URL.Query()
	metrics := splitList(q.Get("metric"))
	if len(metrics) == 0 {
		metrics = splitList(q.Get("metrics"))
	}
	until := parseTimeParam(q.Get("until"), time.Now().Unix())
	// An empty `since` means "everything the rollup holds", not "the last six
	// hours": the point of this endpoint is the long span.
	since := parseTimeParam(q.Get("since"), 0)
	if since > until {
		since, until = until, since
	}
	limit := 720
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	res, err := a.store.QueryRollup(metrics, since, until, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": map[string]any{
			"metrics": res.Metrics, "since": since, "until": until, "limit": limit,
			"since_human": time.Unix(since, 0).Format("2006-01-02 15:04:05"),
			"until_human": time.Unix(until, 0).Format("2006-01-02 15:04:05"),
		},
		"result": res,
	})
}