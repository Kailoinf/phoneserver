// store.go — SQLite-backed history for phoneserver.
//
// Three things are persisted locally:
//
//	samples    one row per tick, the scalar metrics flattened into columns
//	           (queryable: "battery temp over the last 6 hours")
//	snapshots  the complete /api/v1/all document, gzipped (nothing is lost)
//	logs       the log lines, live-mirrored and backfilled from the existing
//	           log files on first start
//
// The driver is modernc.org/sqlite: SQLite compiled to pure Go. That matters
// because the binary must stay CGO-free and statically linked (a PIE or
// dynamically linked build cannot be exec'd on Android — see build.sh), so
// mattn/go-sqlite3 is not an option. Verified on Android 16/arm64: WAL on f2fs,
// cross-process durability, real "SQLite format 3" file.
//
// Retention: rows are pruned by age every few minutes, and a hard size cap
// evicts the oldest snapshots first, so the database can never fill the device.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

// ---------- flattened sample row ----------

// Sample is one tick of scalar metrics. Every field becomes a column, so the
// set is deliberately "the numbers you would graph". Anything not here is
// still recoverable from the gzipped snapshots table.
type Sample struct {
	Ts int64 `json:"ts"`

	// absent names the columns this tick could not measure. They are stored as
	// NULL rather than a zero, which would read as a real reading: the cheap
	// tick cannot see Wi-Fi, the package list or the remaining charge times,
	// and recording 0 for those would invent data instead of omitting it.
	absent map[string]bool

	BatteryCapacityPct float64 `json:"battery_capacity_pct"`
	BatteryStatus      string  `json:"battery_status"`
	BatteryHealth      string  `json:"battery_health"`
	BatteryTempC       float64 `json:"battery_temp_c"`
	BatteryVoltageV    float64 `json:"battery_voltage_v"`
	BatteryCurrentA    float64 `json:"battery_current_a"`
	BatteryChargeMAh   float64 `json:"battery_charge_mah"`
	BatteryFullMAh     float64 `json:"battery_full_mah"`
	BatteryHealthPct   float64 `json:"battery_health_pct"`
	BatteryCycles      int64   `json:"battery_cycles"`
	BatteryTimeFullS   int64   `json:"battery_time_to_full_s"`
	BatteryTimeEmptyS  int64   `json:"battery_time_to_empty_s"`
	UsbOnline          int     `json:"usb_online"`
	UsbPresent         int     `json:"usb_present"`
	UsbVoltageV        float64 `json:"usb_voltage_v"`
	UsbCurrentA        float64 `json:"usb_current_a"`
	WirelessOnline     int     `json:"wireless_online"`
	Wakefulness        string  `json:"wakefulness"`
	ScreenOn           int     `json:"screen_on"`
	Brightness         int64   `json:"brightness"`
	BrightnessMax      int64   `json:"brightness_max"`
	CPUUsagePct        float64 `json:"cpu_usage_pct"`
	CPUCores           int     `json:"cpu_cores"`
	CPULoad1           float64 `json:"cpu_load1"`
	CPULoad5           float64 `json:"cpu_load5"`
	CPULoad15          float64 `json:"cpu_load15"`
	CPUFreqKHz         string  `json:"cpu_freqs_khz"` // JSON array, one per core
	CPUSampleMS        int64   `json:"cpu_sample_ms"`
	GPUBusyPct         int64   `json:"gpu_busy_pct"`
	GPUHz              int64   `json:"gpu_hz"`
	GPUTempC           float64 `json:"gpu_temp_c"`
	MemTotalKB         int64   `json:"mem_total_kb"`
	MemAvailableKB     int64   `json:"mem_available_kb"`
	MemUsedPct         float64 `json:"mem_used_pct"`
	SwapFreeKB         int64   `json:"swap_free_kb"`
	DataFreeBytes      uint64  `json:"data_free_bytes"`
	DataTotalBytes     uint64  `json:"data_total_bytes"`
	DataUsedPct        float64 `json:"data_used_pct"`
	RootFreeBytes      uint64  `json:"root_free_bytes"`
	MaxTempC           float64 `json:"max_temp_c"`
	HottestZone        string  `json:"hottest_zone"`
	ZoneCount          int     `json:"thermal_zone_count"`
	TempZoneCount      int     `json:"thermal_temperature_zone_count"`
	NetRxBps           float64 `json:"net_rx_bps"`
	NetTxBps           float64 `json:"net_tx_bps"`
	NetRxBytes         uint64  `json:"net_rx_bytes"`
	NetTxBytes         uint64  `json:"net_tx_bytes"`
	NetSampleMS        int64   `json:"net_sample_ms"`
	WifiConnected      int     `json:"wifi_connected"`
	WifiSSID           string  `json:"wifi_ssid"`
	WifiRSSIDbm        int     `json:"wifi_rssi_dbm"`
	WifiSignalPct      int     `json:"wifi_signal_pct"`
	WifiLinkMbps       int     `json:"wifi_link_mbps"`
	WifiFreqMHz        int     `json:"wifi_freq_mhz"`
	WifiChannel        int     `json:"wifi_channel"`
	UptimeS            float64 `json:"uptime_s"`
	ProcCount          int     `json:"proc_count"`
	SelinuxEnforcing   int     `json:"selinux_enforcing"`
	PkgTotal           int     `json:"pkg_total"`
	PkgUser            int     `json:"pkg_user"`
	PkgSystem          int     `json:"pkg_system"`
	PkgDisabled        int     `json:"pkg_disabled"`
	Full               int     `json:"full"` // 1 when built from the full snapshot

	CollectMS int64 `json:"collect_ms"` // wall time the collection cost

	// ---- schema v3: metadata that makes a row interpretable on its own ----
	//
	// These describe *how* the row was produced, not what was measured, so a
	// reader can tell a real plateau from a hole and a real reading from a
	// value that failed validation. Rows written before v3 hold NULL here,
	// which means "unknown" — never 0, which would read as a measurement.
	//
	// TickGapMS is the interval since the previous tick measured on the
	// monotonic clock (wall time can be stepped by NTP). MonoMS is that
	// monotonic clock itself, TzOffsetMin the local UTC offset in minutes,
	// MeasuredCols how many metric columns this row really carries,
	// AbsentCols/RejectedCols the column names stored NULL and rejected (JSON
	// arrays), Flags the bitmask documented in historyquality.go and
	// SourceVersion the build that wrote the row.
	TickGapMS     int64  `json:"tick_gap_ms"`
	MonoMS        int64  `json:"mono_ms"`
	TzOffsetMin   int64  `json:"tz_offset_min"`
	MeasuredCols  int64  `json:"measured_cols"`
	AbsentCols    string `json:"absent_cols"`
	RejectedCols  string `json:"rejected_cols"`
	Flags         int64  `json:"flags"`
	SourceVersion string `json:"source_version"`
}

// sampleColumns mirrors the Sample struct field order exactly.
var sampleColumns = []string{
	"ts",
	"battery_capacity_pct", "battery_status", "battery_health", "battery_temp_c",
	"battery_voltage_v", "battery_current_a", "battery_charge_mah", "battery_full_mah",
	"battery_health_pct", "battery_cycles", "battery_time_to_full_s", "battery_time_to_empty_s",
	"usb_online", "usb_present", "usb_voltage_v", "usb_current_a", "wireless_online",
	"wakefulness", "screen_on", "brightness", "brightness_max",
	"cpu_usage_pct", "cpu_cores", "cpu_load1", "cpu_load5", "cpu_load15",
	"cpu_freqs_khz", "cpu_sample_ms",
	"gpu_busy_pct", "gpu_hz", "gpu_temp_c",
	"mem_total_kb", "mem_available_kb", "mem_used_pct", "swap_free_kb",
	"data_free_bytes", "data_total_bytes", "data_used_pct", "root_free_bytes",
	"max_temp_c", "hottest_zone", "thermal_zone_count", "thermal_temperature_zone_count",
	"net_rx_bps", "net_tx_bps", "net_rx_bytes", "net_tx_bytes", "net_sample_ms",
	"wifi_connected", "wifi_ssid", "wifi_rssi_dbm", "wifi_signal_pct", "wifi_link_mbps",
	"wifi_freq_mhz", "wifi_channel",
	"uptime_s", "proc_count", "selinux_enforcing",
	"pkg_total", "pkg_user", "pkg_system", "pkg_disabled",
	"full", "collect_ms",
	"tick_gap_ms", "mono_ms", "tz_offset_min", "measured_cols",
	"absent_cols", "rejected_cols", "flags", "source_version",
}

// values mirrors sampleColumns exactly — keep the two in lockstep.
// markAbsent flags columns this tick did not measure. ts, full and the schema
// v3 quality columns never go missing: they describe the row itself, so NULLing
// them would delete the very metadata that explains the NULLs.
func (s *Sample) markAbsent(cols ...string) {
	if s.absent == nil {
		s.absent = make(map[string]bool, len(cols))
	}
	for _, c := range cols {
		if c == "ts" || c == "full" || sampleQualityColumnSet[c] {
			continue
		}
		s.absent[c] = true
	}
}

// cheapAbsentColumns are the columns only a full tick can measure. They stay
// NULL in between and the read path carries the last measured value forward.
var cheapAbsentColumns = []string{
	"wifi_ssid", "wifi_connected", "wifi_rssi_dbm", "wifi_signal_pct",
	"wifi_link_mbps", "wifi_freq_mhz", "wifi_channel",
	"pkg_total", "pkg_user", "pkg_system", "pkg_disabled",
	"proc_count", "battery_time_to_full_s", "battery_time_to_empty_s",
}

func (s Sample) values() []any {
	v := []any{
		s.Ts,
		s.BatteryCapacityPct, s.BatteryStatus, s.BatteryHealth, s.BatteryTempC,
		s.BatteryVoltageV, s.BatteryCurrentA, s.BatteryChargeMAh, s.BatteryFullMAh,
		s.BatteryHealthPct, s.BatteryCycles, s.BatteryTimeFullS, s.BatteryTimeEmptyS,
		s.UsbOnline, s.UsbPresent, s.UsbVoltageV, s.UsbCurrentA, s.WirelessOnline,
		s.Wakefulness, s.ScreenOn, s.Brightness, s.BrightnessMax,
		s.CPUUsagePct, s.CPUCores, s.CPULoad1, s.CPULoad5, s.CPULoad15,
		s.CPUFreqKHz, s.CPUSampleMS,
		s.GPUBusyPct, s.GPUHz, s.GPUTempC,
		s.MemTotalKB, s.MemAvailableKB, s.MemUsedPct, s.SwapFreeKB,
		s.DataFreeBytes, s.DataTotalBytes, s.DataUsedPct, s.RootFreeBytes,
		s.MaxTempC, s.HottestZone, s.ZoneCount, s.TempZoneCount,
		s.NetRxBps, s.NetTxBps, s.NetRxBytes, s.NetTxBytes, s.NetSampleMS,
		s.WifiConnected, s.WifiSSID, s.WifiRSSIDbm, s.WifiSignalPct, s.WifiLinkMbps,
		s.WifiFreqMHz, s.WifiChannel,
		s.UptimeS, s.ProcCount, s.SelinuxEnforcing,
		s.PkgTotal, s.PkgUser, s.PkgSystem, s.PkgDisabled,
		s.Full, s.CollectMS,
		s.TickGapMS, s.MonoMS, s.TzOffsetMin, s.MeasuredCols,
		s.AbsentCols, s.RejectedCols, s.Flags, s.SourceVersion,
	}
	if len(s.absent) > 0 {
		for i, c := range sampleColumns {
			if s.absent[c] {
				v[i] = nil
			}
		}
	}
	return v
}

// ---------- log rows ----------

type logRow struct {
	TsMS   int64
	Level  string
	Source string // "live" or the file it came from
	Line   string
}

// logSink is an io.Writer the logger can tee into. It never blocks: if the
// database cannot keep up, lines are dropped and counted rather than stalling
// an HTTP handler that is trying to log its own request.
type logSink struct {
	ch    chan logRow
	drops atomic.Int64
	prev  []byte
	mu    sync.Mutex
}

// ---------- store ----------

type Store struct {
	db   *sql.DB
	path string

	// warnf is the migrator's logger: openStore fills it in before migrate()
	// runs so a one-off repair (dropped duplicates) is reported to the operator
	// instead of happening silently.
	warnf func(string, ...any)

	mu      sync.Mutex // serialises writers; SQLite wants one writer
	sink    *logSink
	stopCh  chan struct{}
	stopped sync.Once

	lastPrune atomic.Int64
	pruned    atomic.Int64 // total rows removed
}

// sampleColumnType maps a sample column to its SQLite type. Kept in one place
// so the CREATE TABLE for a new database and the ALTER TABLE for an existing
// one can never disagree about a column's type.
func sampleColumnType(c string) string {
	switch {
	case c == "battery_status", c == "battery_health", c == "wakefulness",
		c == "hottest_zone", c == "wifi_ssid", c == "cpu_freqs_khz",
		c == "absent_cols", c == "rejected_cols", c == "source_version":
		return "TEXT"
	case c == "tick_gap_ms", c == "mono_ms", c == "tz_offset_min",
		c == "measured_cols", c == "flags":
		return "INTEGER"
	case strings.HasSuffix(c, "_bytes"):
		return "INTEGER"
	default:
		return "REAL"
	}
}

func (s *Store) LogSink() io.Writer { return s.sink }

func logLevel(line string) string {
	switch {
	case strings.Contains(line, "FATAL"):
		return "fatal"
	case strings.Contains(line, "watchdog"):
		return "watchdog"
	case strings.Contains(line, "access "):
		return "access"
	case strings.Contains(line, " warning"), strings.Contains(line, "warn:"):
		return "warn"
	case strings.Contains(line, "error"), strings.Contains(line, "FAIL"):
		return "error"
	default:
		return "info"
	}
}

// parseLogTime pulls the "2006/01/02 15:04:05.000000" prefix the standard
// logger writes. It must be parsed in time.Local: the logger prints local wall
// time, and this device has a +08:00 offset, so parsing as UTC would file every
// line eight hours in the future and make "last 10 minutes" queries empty.
// Falls back to now when the line carries no timestamp.
func parseLogTime(line string, fallback time.Time) int64 {
	if len(line) >= 26 {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05.000000", line[:26], time.Local); err == nil {
			return t.UnixMilli()
		}
	}
	if len(line) >= 19 {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); err == nil {
			return t.UnixMilli()
		}
	}
	return fallback.UnixMilli()
}

// Write splits on newlines so a single log line becomes a single row.
func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prev = append(l.prev, p...)
	now := time.Now()
	for {
		i := bytes.IndexByte(l.prev, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.prev[:i]), "\r")
		l.prev = l.prev[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		row := logRow{TsMS: parseLogTime(line, now), Level: logLevel(line), Source: "live", Line: line}
		select {
		case l.ch <- row:
		default:
			l.drops.Add(1)
		}
	}
	// A partial line with no newline would otherwise grow unbounded.
	if len(l.prev) > 64*1024 {
		l.prev = nil
	}
	return len(p), nil
}

func openStore(path string, warnf func(string, ...any)) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	dsn := path + "?_pragma=busy_timeout(8000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=foreign_keys(OFF)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: no SQLITE_BUSY dance, writes are tiny, reads are short.
	db.SetMaxOpenConns(1)
	// Keep the write-ahead log small: the default checkpoint threshold is 1000
	// pages, so the WAL alone sat at ~2.5 MB between prune checkpoints.
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=256`); err != nil {
		return nil, fmt.Errorf("set wal_autocheckpoint: %w", err)
	}
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path, sink: &logSink{ch: make(chan logRow, 8192)}, warnf: warnf,
		// stopCh must exist before Close() (which closes it) or shutdown panics
		// with "close of nil channel" — caught by killing the process on device.
		stopCh: make(chan struct{})}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// The db dir is 0700 already, but keep the files themselves private.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(f, 0o600)
	}
	if warnf != nil {
		warnf("history db ready: %s (schema v%d)", path, schemaVersion)
	}
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)`,
		`CREATE TABLE IF NOT EXISTS samples (` + func() string {
			cols := make([]string, 0, len(sampleColumns))
			for _, c := range sampleColumns {
				if c == "ts" {
					cols = append(cols, "ts INTEGER PRIMARY KEY")
					continue
				}
				cols = append(cols, c+" "+sampleColumnType(c))
			}
			return strings.Join(cols, ", ")
		}() + `)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			ts INTEGER PRIMARY KEY,
			doc BLOB NOT NULL,
			raw_bytes INTEGER NOT NULL,
			gz_bytes INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts_ms INTEGER NOT NULL,
			level TEXT NOT NULL,
			source TEXT NOT NULL,
			line TEXT NOT NULL,
			line_hash INTEGER NOT NULL DEFAULT 0
		)`,
		// rollup_hourly keeps the long-range trend alive after the raw samples
		// age out. One row per hour and metric; WITHOUT ROWID because the
		// composite key *is* the row, which halves the index overhead.
		`CREATE TABLE IF NOT EXISTS rollup_hourly (
			ts_hour INTEGER NOT NULL,
			metric TEXT NOT NULL,
			n INTEGER NOT NULL,
			avg REAL, min REAL, max REAL, last REAL,
			PRIMARY KEY (ts_hour, metric)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_rollup_metric ON rollup_hourly(metric, ts_hour)`,
		// Deduplicate on a 64-bit fingerprint, not on the text itself: indexing
		// (ts_ms, line) stores a second copy of every line inside the index
		// (measured: 241 KB of index for 163 KB of text).
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_logs_unique ON logs(ts_ms, line_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_ts ON snapshots(ts)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w (statement: %s)", err, firstLine(q))
		}
	}
	_ = schemaVersion // keep the version visible next to the migration list

	// --- v2 -> v3: the quality columns -------------------------------------
	// Added with ALTER TABLE, never by rebuilding the table: a rebuild would
	// double the file size on the phone's flash and could lose rows if it were
	// interrupted. Existing rows keep NULL in every new column, which is the
	// honest value ("this row predates the metadata") and must not be backfilled
	// with 0, because 0 reads as a measurement: measured_cols=0 would claim the
	// row has no data at all, and flags=0 would claim no anomaly was seen.
	// The error for an already-present column is expected on the second and
	// every later start, which is what makes this idempotent.
	for _, c := range sampleQualityColumns {
		if _, err := s.db.Exec(`ALTER TABLE samples ADD COLUMN ` + c + " " + sampleColumnType(c)); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("migrate: add samples.%s: %w", c, err)
			}
		}
	}
	// Duplicates must be gone before the UNIQUE index can be created, and the
	// migration has to survive a database written by a build that did not
	// enforce it.
	if n, err := s.dedupeSamples(); err != nil {
		return err
	} else if n > 0 {
		total := int64(0)
		if v, ok := s.metaGet("samples_deduped_total"); ok {
			fmt.Sscanf(v, "%d", &total)
		}
		total += n
		_ = s.metaSet("samples_deduped_total", fmt.Sprint(total))
		if s.warnf != nil {
			s.warnf("history: migrate dropped %d duplicate sample row(s) at the same ts (kept the most complete row); %d dropped in total", n, total)
		}
	}
	// ts uniqueness, stated in the schema instead of trusted to the writer.
	// When ts is INTEGER PRIMARY KEY SQLite already enforces it (a second insert
	// replaces the first), but a table written by an older or foreign build may
	// have had ts as a plain column, where a restart could leave two rows in the
	// same second. The index also stops ON CONFLICT/REPLACE from silently
	// creating that state again. It indexes one integer, so it costs ~1% of the
	// samples table.
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_samples_ts_unique ON samples(ts)`); err != nil {
		return fmt.Errorf("migrate: unique index on samples.ts: %w", err)
	}

	// Migrations that SQL cannot express, run every start and cheap when done.
	// 1. logs.line_hash. Adding the column twice fails harmlessly.
	s.db.Exec(`ALTER TABLE logs ADD COLUMN line_hash INTEGER NOT NULL DEFAULT 0`)
	if err := s.backfillLogHashes(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_logs_unique`); err != nil {
		return fmt.Errorf("migrate: drop idx_logs_unique: %w", err)
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_logs_unique ON logs(ts_ms, line_hash)`); err != nil {
		return fmt.Errorf("migrate: recreate idx_logs_unique: %w", err)
	}
	// 2. Incremental auto-vacuum. Without it a DELETE only marks pages free, so
	//    the file keeps its high-water mark forever and the reported size stops
	//    describing the content. Switching requires one VACUUM.
	var av int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err == nil && av != 2 {
		if _, err := s.db.Exec(`PRAGMA auto_vacuum=2`); err == nil {
			if _, err := s.db.Exec(`VACUUM`); err != nil {
				return fmt.Errorf("migrate: vacuum for auto_vacuum: %w", err)
			}
		}
	}

	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES('schema',?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, fmt.Sprint(schemaVersion))
	return err
}

// lineHash is a stable 64-bit fingerprint of a log line. The deduplication
// index keys on it so the index does not hold a second copy of the text.
func lineHash(s string) int64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return int64(h.Sum64())
}

// backfillLogHashes fills line_hash for rows written before the column existed.
// It is a no-op once every row has one. The rows are collected before the
// updates start: with a single pooled connection, writing while a cursor is
// still open would wait on itself.
func (s *Store) backfillLogHashes() error {
	var pending int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM logs WHERE line_hash IS NULL OR line_hash = 0`).Scan(&pending); err != nil {
		return fmt.Errorf("migrate: count rows needing line_hash: %w", err)
	}
	if pending == 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT id, line FROM logs WHERE line_hash IS NULL OR line_hash = 0`)
	if err != nil {
		return err
	}
	var ids []int64
	var hashes []int64
	for rows.Next() {
		var id int64
		var line string
		if err := rows.Scan(&id, &line); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		hashes = append(hashes, lineHash(line))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	st, err := tx.Prepare(`UPDATE logs SET line_hash = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for i, id := range ids {
		if _, err := st.Exec(hashes[i], id); err != nil {
			st.Close()
			tx.Rollback()
			return err
		}
	}
	st.Close()
	return tx.Commit()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// completenessScore is a SQL expression ranking how much of a row is real:
// each non-NULL metric counts one. It is used to decide which row survives when
// two rows claim the same second — keeping the fuller one is keeping the truth.
func completenessScore() string {
	parts := make([]string, 0, len(sampleColumns))
	for _, c := range sampleColumns {
		if c == "ts" {
			continue
		}
		parts = append(parts, "("+c+" IS NOT NULL)")
	}
	return strings.Join(parts, " + ")
}

// dedupeSamples removes rows that share a ts, keeping the most complete one.
// SQLite's INTEGER PRIMARY KEY already makes that impossible in a table this
// build created, but a database written by another build may not have had the
// constraint — and a UNIQUE index cannot be created while duplicates exist.
// Rows are collected before any DELETE runs: with SetMaxOpenConns(1) a write
// issued while a cursor is open would wait on itself forever.
func (s *Store) dedupeSamples() (int64, error) {
	var groups int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM (SELECT ts FROM samples GROUP BY ts HAVING COUNT(*) > 1)`).Scan(&groups); err != nil {
		return 0, fmt.Errorf("migrate: count duplicate ts: %w", err)
	}
	if groups == 0 {
		return 0, nil
	}
	rows, err := s.db.Query(
		`SELECT rowid, ts FROM samples WHERE ts IN
			(SELECT ts FROM samples GROUP BY ts HAVING COUNT(*) > 1)
		 ORDER BY ts ASC, (` + completenessScore() + `) DESC, COALESCE(full,0) DESC, COALESCE(collect_ms,0) DESC`)
	if err != nil {
		return 0, fmt.Errorf("migrate: list duplicate rows: %w", err)
	}
	var losers []int64
	lastTs := int64(-1 << 62)
	it := int64(0)
	for rows.Next() {
		var rowid, ts int64
		if err := rows.Scan(&rowid, &ts); err != nil {
			rows.Close()
			return 0, err
		}
		// Rows arrive grouped by ts and best-first inside the group, so the
		// first row of each group is kept and everything after it is a loser.
		if ts != lastTs {
			lastTs = ts
			it = 0
		}
		it++
		if it > 1 {
			losers = append(losers, rowid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var dropped int64
	for i := 0; i < len(losers); i += 200 {
		end := i + 200
		if end > len(losers) {
			end = len(losers)
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", end-i), ",")
		args := make([]any, 0, end-i)
		for _, id := range losers[i:end] {
			args = append(args, id)
		}
		res, err := s.db.Exec(`DELETE FROM samples WHERE rowid IN (`+ph+`)`, args...)
		if err != nil {
			return dropped, fmt.Errorf("migrate: delete duplicate rows: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			dropped += n
		}
	}
	return dropped, nil
}

func (s *Store) Close() error {
	s.stopped.Do(func() { close(s.stopCh) })
	return s.db.Close()
}

// ---------- writes ----------

func (s *Store) InsertSample(sm Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := "INSERT OR REPLACE INTO samples (" + strings.Join(sampleColumns, ",") + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?,", len(sampleColumns)), ",") + ")"
	_, err := s.db.Exec(q, sm.values()...)
	return err
}

// InsertSampleAndSnapshot writes the sample row and (when doc is not empty)
// the gzipped document in a single transaction. Two separate Execs mean two
// COMMITs, and every COMMIT is an fsync on the phone's flash.
func (s *Store) InsertSampleAndSnapshot(sm Sample, doc []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	q := "INSERT OR REPLACE INTO samples (" + strings.Join(sampleColumns, ",") + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?,", len(sampleColumns)), ",") + ")"
	if _, err := tx.Exec(q, sm.values()...); err != nil {
		tx.Rollback()
		return err
	}
	if len(doc) > 0 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(doc); err == nil {
			if err := zw.Close(); err == nil {
				if _, err := tx.Exec(
					`INSERT OR REPLACE INTO snapshots (ts, doc, raw_bytes, gz_bytes) VALUES (?,?,?,?)`,
					sm.Ts, buf.Bytes(), len(doc), buf.Len()); err != nil {
					tx.Rollback()
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func (s *Store) InsertSnapshot(ts int64, doc []byte) (int, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(doc); err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO snapshots (ts, doc, raw_bytes, gz_bytes) VALUES (?,?,?,?)`,
		ts, buf.Bytes(), len(doc), buf.Len())
	return buf.Len(), err
}

// insertLogRows is the batched writer used by the flusher.
func (s *Store) insertLogRows(rows []logRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	st, err := tx.Prepare(`INSERT OR IGNORE INTO logs (ts_ms, level, source, line, line_hash) VALUES (?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	n := 0
	for _, r := range rows {
		res, err := st.Exec(r.TsMS, r.Level, r.Source, r.Line, lineHash(r.Line))
		if err != nil {
			continue
		}
		if c, err := res.RowsAffected(); err == nil {
			n += int(c)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// StartLogFlusher drains the sink in batches so a busy access log costs a few
// transactions per second rather than one per line.
func (s *Store) StartLogFlusher(warnf func(string, ...any)) {
	go func() {
		// Each COMMIT is an fsync on the phone's flash. Flushing on every 400 ms
		// tick with two or three rows means constant fsync churn, which competes
		// with the sampling loop for the write lock; batching wider is cheaper.
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		batch := make([]logRow, 0, 256)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if _, err := s.insertLogRows(batch); err != nil && warnf != nil {
				warnf("history: log insert failed: %v", err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case <-s.stopCh:
				flush()
				return
			case r := <-s.sink.ch:
				batch = append(batch, r)
				if len(batch) >= 64 {
					flush()
				}
			case <-tick.C:
				flush()
			}
		}
	}()
}

// BackfillLogs imports the pre-existing log files so the history is not
// "since the upgrade" only. Safe to re-run: (ts_ms, line) is unique, so
// already-imported lines are ignored.
func (s *Store) BackfillLogs(paths []string) (inserted int, files int, err error) {
	var rows []logRow
	for _, p := range paths {
		f, oerr := os.Open(p)
		if oerr != nil {
			continue
		}
		base := filepath.Base(p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			rows = append(rows, logRow{
				TsMS:   parseLogTime(line, time.Now()),
				Level:  logLevel(line),
				Source: "backfill:" + base,
				Line:   line,
			})
			if len(rows) >= 500 {
				n, _ := s.insertLogRows(rows)
				inserted += n
				rows = rows[:0]
			}
		}
		f.Close()
		files++
	}
	if len(rows) > 0 {
		n, _ := s.insertLogRows(rows)
		inserted += n
	}
	return inserted, files, nil
}

// ---------- retention ----------

type PruneResult struct {
	SamplesDeleted   int64  `json:"samples_deleted"`
	SnapshotsDeleted int64  `json:"snapshots_deleted"`
	LogsDeleted      int64  `json:"logs_deleted"`
	RollupRows       int64  `json:"rollup_rows_upserted"`
	RollupDeleted    int64  `json:"rollup_hours_deleted"`
	SizeCapEvicted   int64  `json:"size_cap_evicted"`
	DBBytesBefore    int64  `json:"db_bytes_before"`
	DBBytesAfter     int64  `json:"db_bytes_after"`
	RollupError      string `json:"rollup_error,omitempty"`
}

func (s *Store) DBFileBytes() int64 {
	var total int64
	for _, f := range []string{s.path, s.path + "-wal"} {
		if st, err := os.Stat(f); err == nil {
			total += st.Size()
		}
	}
	return total
}

// Prune drops rows past the configured ages, then, if the file is still bigger
// than the hard cap, evicts the oldest snapshots first and then logs.
func (s *Store) Prune(now time.Time, h HistoryConfig) (PruneResult, error) {
	res := PruneResult{DBBytesBefore: s.DBFileBytes()}
	s.mu.Lock()
	defer s.mu.Unlock()

	cut := func(days int) int64 {
		if days <= 0 {
			return 0
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	}
	exec := func(q string, args ...any) int64 {
		r, err := s.db.Exec(q, args...)
		if err != nil {
			return 0
		}
		n, _ := r.RowsAffected()
		return n
	}
	// Roll up *before* deleting raw samples. Retention deletes samples at the
	// end of this function; if the hourly aggregate for an hour does not exist
	// yet, that hour's trend is destroyed forever. Doing it first makes the
	// order irrelevant to correctness even though the two run in one call.
	//
	// rollupHoursLocked, not rollupHours: Prune already holds s.mu, and the
	// locking version would re-acquire it on the same goroutine and hang the
	// store forever (the log flusher queues behind it and the sink channel then
	// fills up). This deadlock was found on device with a goroutine dump.
	if h.RollupEnabled {
		n, err := s.rollupHoursLocked(now, h)
		res.RollupRows = n
		if err != nil {
			res.RollupError = err.Error()
			if s.warnf != nil {
				s.warnf("history: hourly rollup failed: %v", err)
			}
		}
	}
	if c := cut(h.RetentionDays); c > 0 {
		res.SamplesDeleted = exec(`DELETE FROM samples WHERE ts < ?`, c)
	}
	if c := cut(h.RollupRetentionDays); c > 0 {
		// 0 means keep the hourly trend forever: above the raw retention it is
		// the only remaining evidence, and it is ~1/3600th of the raw volume.
		res.RollupDeleted = exec(`DELETE FROM rollup_hourly WHERE ts_hour < ?`, c)
	}
	if c := cut(h.SnapshotRetentionDays); c > 0 {
		res.SnapshotsDeleted = exec(`DELETE FROM snapshots WHERE ts < ?`, c)
	}
	if c := cut(h.LogRetentionDays); c > 0 {
		res.LogsDeleted = exec(`DELETE FROM logs WHERE ts_ms < ?`, c*1000)
	}

	// Hard cap: the device must never fill up because of telemetry.
	if h.MaxDBMB > 0 {
		limit := int64(h.MaxDBMB) << 20
		for i := 0; i < 50 && s.DBFileBytes() > limit; i++ {
			// Oldest snapshots are the cheapest history to lose (they are
			// recoverable in aggregate from samples) so they go first.
			n := exec(`DELETE FROM snapshots WHERE ts IN
				(SELECT ts FROM snapshots ORDER BY ts ASC LIMIT 200)`)
			if n == 0 {
				n = exec(`DELETE FROM logs WHERE id IN
					(SELECT id FROM logs ORDER BY ts_ms ASC LIMIT 2000)`)
			}
			if n == 0 {
				n = exec(`DELETE FROM samples WHERE ts IN
					(SELECT ts FROM samples ORDER BY ts ASC LIMIT 2000)`)
			}
			res.SizeCapEvicted += n
			if n == 0 {
				break
			}
			// Reclaiming space in WAL mode needs a checkpoint.
			s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		}
	}
	// With incremental auto-vacuum this is what actually gives space back to
	// the filesystem; without it the deletions would only mark pages free.
	s.db.Exec(`PRAGMA incremental_vacuum`)
	s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	// Stat only after the checkpoint, otherwise the reported size ignores the
	// WAL that is about to be folded back into the main file.
	res.DBBytesAfter = s.DBFileBytes()
	s.pruned.Add(res.SamplesDeleted + res.SnapshotsDeleted + res.LogsDeleted + res.SizeCapEvicted)
	s.lastPrune.Store(now.Unix())
	return res, nil
}

// ---------- reads ----------

// metricWhitelist guards the column names that may reach SQL.
var metricWhitelist = func() map[string]bool {
	m := map[string]bool{"ts": true}
	for _, c := range sampleColumns {
		m[c] = true
	}
	return m
}()

type SeriesResult struct {
	Count int `json:"count"`
	// Carried lists the columns whose values came from an earlier full tick:
	// they were not measured in every row of this series.
	Carried   []string  `json:"carried_columns,omitempty"`
	Columns   []string  `json:"columns"`
	Points    [][]any   `json:"points"` // [[ts, v1, v2, ...], ...] oldest first
	Since     int64     `json:"since"`
	Until     int64     `json:"until"`
	Truncated bool      `json:"truncated"`
	Stats     *AggStats `json:"stats,omitempty"`
}

type AggStats struct {
	Min  map[string]float64 `json:"min"`
	Max  map[string]float64 `json:"max"`
	Avg  map[string]float64 `json:"avg"`
	Last map[string]float64 `json:"last"`
}

// QuerySamples returns the requested metric columns between two unix times.
func (s *Store) QuerySamples(metrics []string, since, until int64, limit int, withStats bool) (*SeriesResult, error) {
	cols := []string{"ts"}
	seen := map[string]bool{"ts": true}
	if len(metrics) == 0 {
		metrics = []string{"battery_capacity_pct", "battery_temp_c", "cpu_usage_pct", "mem_used_pct", "max_temp_c"}
	}
	for _, m := range metrics {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !metricWhitelist[m] {
			return nil, fmt.Errorf("unknown metric %q (see GET /api/v1/history/metrics)", m)
		}
		seen[m] = true
		cols = append(cols, m)
	}
	if limit <= 0 {
		limit = 720
	}
	if limit > 20000 {
		limit = 20000
	}
	if until <= 0 {
		until = time.Now().Unix()
	}
	if since <= 0 {
		since = until - 6*3600
	}

	// Seed every column with its last measured value before the window, so the
	// first points are not blank merely because the window starts between two
	// full ticks. This runs before the main query: with a single pooled
	// connection, a query issued while the cursor is open waits on itself.
	seed := make([]any, len(cols))
	for i := 1; i < len(cols); i++ {
		var v any
		if err := s.db.QueryRow("SELECT "+cols[i]+" FROM samples WHERE ts < ? AND "+cols[i]+
			" IS NOT NULL ORDER BY ts DESC LIMIT 1", since).Scan(&v); err == nil {
			seed[i] = normalize(v)
		}
	}

	q := "SELECT " + strings.Join(cols, ",") + " FROM samples WHERE ts >= ? AND ts <= ? ORDER BY ts DESC LIMIT ?"
	rows, err := s.db.Query(q, since, until, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := make([][]any, 0, 128)
	colVals := make([][]float64, len(cols))
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		pt := make([]any, len(cols))
		for i, v := range dest {
			pt[i] = normalize(v)
			if f, ok := asFloat(v); ok && i > 0 {
				colVals[i] = append(colVals[i], f)
			}
		}
		points = append(points, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	truncated := len(points) >= limit
	// Oldest first reads better for charts.
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}

	// Carry the last measured value forward over the NULLs left by cheap ticks,
	// and report which columns needed it: a client should not have to guess
	// whether a flat stretch is a real plateau or a carried value.
	carried := []string{}
	for i := 1; i < len(cols); i++ {
		last := seed[i]
		filled := false
		for _, pt := range points {
			if pt[i] == nil {
				if last != nil {
					pt[i] = last
					filled = true
				}
				continue
			}
			last = pt[i]
		}
		if filled {
			carried = append(carried, cols[i])
		}
	}
	sort.Strings(carried)

	out := &SeriesResult{
		Count: len(points), Columns: cols, Points: points,
		Since: since, Until: until, Truncated: truncated, Carried: carried,
	}
	if withStats && len(points) > 0 {
		st := &AggStats{Min: map[string]float64{}, Max: map[string]float64{}, Avg: map[string]float64{}, Last: map[string]float64{}}
		for i, c := range cols {
			if i == 0 || len(colVals[i]) == 0 {
				continue
			}
			vals := colVals[i]
			min, max := vals[0], vals[0]
			var sum float64
			for _, v := range vals {
				if v < min {
					min = v
				}
				if v > max {
					max = v
				}
				sum += v
			}
			st.Min[c], st.Max[c] = round2(min), round2(max)
			st.Avg[c] = round2(sum / float64(len(vals)))
			// Rows are scanned newest first, so vals[0] is the most recent
			// measured value; vals[len-1] would report the oldest point in the
			// window as "last".
			st.Last[c] = round2(vals[0])
		}
		out.Stats = st
	}
	return out, nil
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case []byte:
		f, err := strconv.ParseFloat(string(t), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

func normalize(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case int64:
		return t
	case float64:
		return round2(t)
	}
	return v
}

type LogResult struct {
	Count     int      `json:"count"`
	Lines     []LogRec `json:"lines"`
	SinceMS   int64    `json:"since_ms"`
	UntilMS   int64    `json:"until_ms"`
	Truncated bool     `json:"truncated"`
	Levels    []string `json:"levels"`
}

type LogRec struct {
	TsMS   int64  `json:"ts_ms"`
	Time   string `json:"time"`
	Level  string `json:"level"`
	Source string `json:"source"`
	Line   string `json:"line"`
}

// QueryLogs returns log rows newest-first, optionally filtered by substring.
// The level histogram is collected *before* the main query: with
// SetMaxOpenConns(1) a second query issued while the first rows are still open
// would wait for a connection that cannot be released yet.
func (s *Store) QueryLogs(sinceMS, untilMS int64, limit int, grep, level string) (*LogResult, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	if untilMS <= 0 {
		untilMS = time.Now().UnixMilli()
	}
	levels, _ := s.logLevels()
	q := `SELECT ts_ms, level, source, line FROM logs WHERE ts_ms >= ? AND ts_ms <= ?`
	args := []any{sinceMS, untilMS}
	if level != "" {
		q += ` AND level = ?`
		args = append(args, level)
	}
	if grep != "" {
		q += ` AND line LIKE ?`
		args = append(args, "%"+grep+"%")
	}
	q += ` ORDER BY ts_ms DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &LogResult{SinceMS: sinceMS, UntilMS: untilMS, Lines: make([]LogRec, 0, 64)}
	for rows.Next() {
		var r LogRec
		var tsMS int64
		if err := rows.Scan(&tsMS, &r.Level, &r.Source, &r.Line); err != nil {
			return nil, err
		}
		r.TsMS = tsMS
		r.Time = time.UnixMilli(tsMS).Format("2006-01-02 15:04:05.000")
		out.Lines = append(out.Lines, r)
	}
	out.Count = len(out.Lines)
	out.Truncated = out.Count >= limit
	out.Levels = levels
	return out, rows.Err()
}

func (s *Store) logLevels() ([]string, error) {
	rows, err := s.db.Query(`SELECT level, COUNT(*) FROM logs GROUP BY level ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var lv string
		var n int64
		if err := rows.Scan(&lv, &n); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s:%d", lv, n))
	}
	return out, nil
}

// SnapshotDoc returns the stored full document closest to ts (decompressed).
func (s *Store) SnapshotDoc(ts int64) (map[string]any, int64, error) {
	var (
		rowTs int64
		blob  []byte
		raw   int64
		err   error
	)
	if ts <= 0 {
		err = s.db.QueryRow(`SELECT ts, doc, raw_bytes FROM snapshots ORDER BY ts DESC LIMIT 1`).
			Scan(&rowTs, &blob, &raw)
	} else {
		err = s.db.QueryRow(`SELECT ts, doc, raw_bytes FROM snapshots ORDER BY ABS(ts - ?) LIMIT 1`, ts).
			Scan(&rowTs, &blob, &raw)
	}
	if err == sql.ErrNoRows {
		return nil, 0, errors.New("no snapshots stored yet")
	}
	if err != nil {
		return nil, 0, err
	}
	zr, gerr := gzip.NewReader(bytes.NewReader(blob))
	if gerr != nil {
		return nil, 0, gerr
	}
	defer zr.Close()
	body, rerr := io.ReadAll(zr)
	if rerr != nil {
		return nil, 0, rerr
	}
	var doc map[string]any
	if uerr := json.Unmarshal(body, &doc); uerr != nil {
		return nil, 0, uerr
	}
	return doc, rowTs, nil
}

// ---------- bucketed aggregation ----------

// defaultMetrics is what /api/v1/history shows when no metric is named.
var defaultMetrics = []string{"battery_capacity_pct", "battery_temp_c", "cpu_usage_pct", "mem_used_pct", "max_temp_c"}

// resolveMetrics validates a metric list against the whitelist and returns the
// SELECT column list (with ts first). Shared by the raw and the bucketed query
// so both reject the same unknown names.
func resolveMetrics(metrics []string) ([]string, error) {
	cols := []string{"ts"}
	seen := map[string]bool{"ts": true}
	if len(metrics) == 0 {
		metrics = defaultMetrics
	}
	for _, m := range metrics {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !metricWhitelist[m] {
			return nil, fmt.Errorf("unknown metric %q (see GET /api/v1/history/metrics)", m)
		}
		seen[m] = true
		cols = append(cols, m)
	}
	return cols, nil
}

// BucketStat is one metric inside one bucket. The pointers keep "no reading in
// this bucket" distinguishable from a reading of zero — the whole point of the
// NULL discipline in the samples table.
type BucketStat struct {
	N    int64    `json:"n"`
	Avg  *float64 `json:"avg"`
	Min  *float64 `json:"min"`
	Max  *float64 `json:"max"`
	Last *float64 `json:"last"`
}

type BucketSeries struct {
	Since         int64            `json:"since"`
	Until         int64            `json:"until"`
	BucketSeconds int              `json:"bucket_seconds"`
	Aggregation   string           `json:"aggregation"`
	SourceRows    int64            `json:"source_rows"`
	Columns       []string         `json:"columns"`
	Count         int              `json:"count"`
	Buckets       []map[string]any `json:"buckets"`
	Truncated     bool             `json:"truncated"`
	Note          string           `json:"note"`
}

// QuerySamplesBucketed aggregates inside SQLite instead of shipping the raw
// rows to the client. A 30-day window at a 10 s cadence is 259 200 rows: the
// raw answer would be truncated by `limit` and the client would silently chart
// the oldest slice of it. Bucketing answers the question that was actually
// asked ("what did the last month look like") with one row per 10m/1h/1d.
//
// Every bucket carries, per metric, the count of real readings, the average,
// the extremes and the most recent value. `last` is the value nearest the end
// of the bucket, so a chart's final point is the real last reading rather than
// an average the phone never measured.
func (s *Store) QuerySamplesBucketed(metrics []string, since, until int64, bucketSec, limit int) (*BucketSeries, error) {
	cols, err := resolveMetrics(metrics)
	if err != nil {
		return nil, err
	}
	if bucketSec < 60 {
		bucketSec = 60
	}
	if bucketSec > 7*86400 {
		bucketSec = 7 * 86400
	}
	if limit <= 0 {
		limit = 720
	}
	if limit > 20000 {
		limit = 20000
	}
	if until <= 0 {
		until = time.Now().Unix()
	}
	if since <= 0 {
		since = until - 30*24*3600
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var sourceRows int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE ts >= ? AND ts <= ?`, since, until).
		Scan(&sourceRows); err != nil {
		return nil, err
	}

	// Buckets are aligned to the epoch, not to `since`: two queries over
	// overlapping windows then return the same buckets for the same clock
	// times, which is what makes an aggregated series comparable with another.
	type statRow struct {
		bucket int64
		st     BucketStat
	}
	perMetric := make(map[string][]statRow, len(cols)-1)
	bucketSet := map[int64]bool{}
	for _, c := range cols[1:] {
		q := `WITH g AS (
			SELECT (ts / ?) * ? AS b, COUNT(` + c + `) AS n,
			       AVG(` + c + `) AS a, MIN(` + c + `) AS mn, MAX(` + c + `) AS mx
			FROM samples WHERE ts >= ? AND ts <= ? AND ` + c + ` IS NOT NULL
			GROUP BY b)
			SELECT g.b, g.n, g.a, g.mn, g.mx,
			       (SELECT s2.` + c + ` FROM samples s2
			         WHERE s2.ts >= g.b AND s2.ts < g.b + ? AND s2.` + c + ` IS NOT NULL
			         ORDER BY s2.ts DESC LIMIT 1)
			FROM g ORDER BY g.b DESC LIMIT ?`
		rows, err := s.db.Query(q, bucketSec, bucketSec, since, until, bucketSec, limit)
		if err != nil {
			return nil, fmt.Errorf("bucket query for %s: %w", c, err)
		}
		for rows.Next() {
			var b int64
			var st BucketStat
			var avg, mn, mx, last sql.NullFloat64
			if err := rows.Scan(&b, &st.N, &avg, &mn, &mx, &last); err != nil {
				rows.Close()
				return nil, err
			}
			st.Avg, st.Min, st.Max, st.Last = fp(avg), fp(mn), fp(mx), fp(last)
			perMetric[c] = append(perMetric[c], statRow{bucket: b, st: st})
			bucketSet[b] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	buckets := make([]int64, 0, len(bucketSet))
	for b := range bucketSet {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })

	out := &BucketSeries{
		Since: since, Until: until, BucketSeconds: bucketSec,
		Aggregation: "per bucket and metric: n (readings), avg, min, max, last (most recent reading in the bucket); " +
			"n=0 with nulls means nothing was measured in that bucket, which is not the same as a measured zero",
		SourceRows: sourceRows, Columns: cols[1:],
		Count:     len(buckets),
		Truncated: len(buckets) >= limit,
		Note: "buckets are left-aligned to the epoch and sorted oldest first; " +
			"points are [ts, {n,avg,min,max,last}] per metric",
	}
	for _, b := range buckets {
		row := map[string]any{"ts": b}
		for _, c := range cols[1:] {
			for _, sr := range perMetric[c] {
				if sr.bucket == b {
					row[c] = sr.st
					break
				}
			}
			if _, ok := row[c]; !ok {
				row[c] = BucketStat{}
			}
		}
		out.Buckets = append(out.Buckets, row)
	}
	return out, nil
}

func fp(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := round2(v.Float64)
	return &f
}

// ---------- hourly rollup ----------

// rollupMetricColumns are the metrics worth keeping for years. Quality
// metadata (flags, gaps, measured counts) is deliberately excluded: those
// describe the *rows*, are fully reported by /api/v1/history/quality while the
// raw samples exist, and averaging them per hour says nothing useful.
var rollupMetricColumns = func() []string {
	out := make([]string, 0, len(sampleColumns))
	for _, c := range SampleColumns() {
		if c == "full" || sampleQualityColumnSet[c] {
			continue
		}
		out = append(out, c)
	}
	return out
}()

var rollupMetricSet = func() map[string]bool {
	m := make(map[string]bool, len(rollupMetricColumns))
	for _, c := range rollupMetricColumns {
		m[c] = true
	}
	return m
}()

// rollupHours folds raw samples into rollup_hourly, one grouped statement per
// metric (never one row at a time) and returns how many aggregate rows were
// written.
//
// Only complete hours are folded. The hour in progress is still growing, so an
// average over it would be wrong in a way that nothing later corrects — it
// would sit in the database forever as a plausible-looking low number.
func (s *Store) rollupHours(now time.Time, h HistoryConfig) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rollupHoursLocked(now, h)
}

// rollupHoursLocked is the body of rollupHours with the caller owning s.mu,
// so Prune can fold aggregates without deadlocking on its own lock.
func (s *Store) rollupHoursLocked(now time.Time, h HistoryConfig) (int64, error) {
	end := now.Truncate(time.Hour).Unix()
	var start int64
	if v, ok := s.metaGet("rollup_hour"); ok {
		fmt.Sscanf(v, "%d", &start)
	}
	if start <= 0 {
		var oldest int64
		if err := s.db.QueryRow(`SELECT COALESCE(MIN(ts), 0) FROM samples`).Scan(&oldest); err != nil {
			return 0, err
		}
		if oldest <= 0 {
			return 0, nil
		}
		start = oldest - oldest%3600
	}
	if start >= end {
		return 0, nil
	}
	// A fresh database (or one upgraded from a build without rollups) can hold
	// weeks of samples: folding all of it in one call would hold the writer for
	// seconds and stall the sampling loop behind it. Bounding the work per pass
	// lets it catch up over successive prune passes instead.
	const maxHoursPerPass = 48
	if end-start > maxHoursPerPass*3600 {
		end = start + maxHoursPerPass*3600
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	var written int64
	for _, c := range rollupMetricColumns {
		// `last` is the reading closest to the end of the hour, which is what a
		// chart labels that hour with. The subquery is written as a ts range
		// rather than `(ts/3600)*3600 = g.h` on purpose: the range form uses the
		// primary-key index on ts and stops after one row, while the expression
		// form would force a full scan of the hour per bucket.
		st, err := tx.Prepare(`WITH g AS (
			SELECT (ts / 3600) * 3600 AS h, COUNT(` + c + `) AS n,
			       AVG(` + c + `) AS avg, MIN(` + c + `) AS mn, MAX(` + c + `) AS mx
			FROM samples WHERE ts >= ? AND ts < ? AND ` + c + ` IS NOT NULL
			GROUP BY h)
			INSERT INTO rollup_hourly (ts_hour, metric, n, avg, min, max, last)
			SELECT g.h, ?, g.n, g.avg, g.mn, g.mx,
			       (SELECT s2.` + c + ` FROM samples s2
			         WHERE s2.ts >= g.h AND s2.ts < g.h + 3600 AND s2.` + c + ` IS NOT NULL
			         ORDER BY s2.ts DESC LIMIT 1)
			FROM g
			ON CONFLICT(ts_hour, metric) DO UPDATE SET
				n = excluded.n, avg = excluded.avg, min = excluded.min, max = excluded.max, last = excluded.last`)
		if err != nil {
			tx.Rollback()
			return written, err
		}
		res, err := st.Exec(start, end, c)
		st.Close()
		if err != nil {
			tx.Rollback()
			return written, fmt.Errorf("rollup %s: %w", c, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			written += n
		}
	}
	if err := tx.Commit(); err != nil {
		return written, err
	}
	if err := s.metaSet("rollup_hour", fmt.Sprint(end)); err != nil {
		return written, err
	}
	return written, nil
}

type RollupPoint struct {
	Metric string   `json:"metric"`
	TsHour int64    `json:"ts_hour"`
	N      int64    `json:"n"`
	Avg    *float64 `json:"avg"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Last   *float64 `json:"last"`
}

type RollupResult struct {
	Since        int64         `json:"since"`
	Until        int64         `json:"until"`
	Metrics      []string      `json:"metrics"`
	Count        int           `json:"count"`
	Rows         []RollupPoint `json:"rows"`
	Truncated    bool          `json:"truncated"`
	OldestTSHour int64         `json:"oldest_ts_hour"`
	NewestTSHour int64         `json:"newest_ts_hour"`
	RollupRows   int64         `json:"rollup_rows_total"`
	Source       string        `json:"source"`
	Note         string        `json:"note"`
}

// QueryRollup reads the hourly trend. It is the only path that still answers
// "what did last month look like" after the raw samples were pruned, so it
// reports its own coverage (oldest/newest hour) and never pretends an empty
// stretch is a zero.
func (s *Store) QueryRollup(metrics []string, since, until int64, limit int) (*RollupResult, error) {
	if len(metrics) == 0 {
		metrics = []string{"battery_capacity_pct", "battery_temp_c", "cpu_usage_pct", "mem_used_pct", "max_temp_c"}
	}
	names := make([]string, 0, len(metrics))
	seen := map[string]bool{}
	for _, m := range metrics {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !rollupMetricSet[m] {
			return nil, fmt.Errorf("unknown rollup metric %q (see GET /api/v1/history/quality for the rolled-up set)", m)
		}
		seen[m] = true
		names = append(names, m)
	}
	if limit <= 0 {
		limit = 720
	}
	if limit > 20000 {
		limit = 20000
	}
	if until <= 0 {
		until = time.Now().Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := &RollupResult{Since: since, Until: until, Metrics: names, Rows: make([]RollupPoint, 0, 64),
		Source: "rollup_hourly",
		Note: "one row per hour and metric, built from the raw samples before they age out; " +
			"n is how many real readings that hour had (0 would mean none, and then avg/min/max/last are null)"}
	if err := s.db.QueryRow(`SELECT COALESCE(MIN(ts_hour),0), COALESCE(MAX(ts_hour),0), COUNT(*) FROM rollup_hourly`).
		Scan(&out.OldestTSHour, &out.NewestTSHour, &out.RollupRows); err != nil {
		return nil, err
	}
	for _, c := range names {
		q := `SELECT ts_hour, n, avg, min, max, last FROM rollup_hourly
		      WHERE metric = ? AND ts_hour >= ? AND ts_hour <= ? ORDER BY ts_hour DESC LIMIT ?`
		rows, err := s.db.Query(q, c, since, until, limit)
		if err != nil {
			return nil, err
		}
		n := 0
		for rows.Next() {
			p := RollupPoint{Metric: c}
			var avg, mn, mx, last sql.NullFloat64
			if err := rows.Scan(&p.TsHour, &p.N, &avg, &mn, &mx, &last); err != nil {
				rows.Close()
				return nil, err
			}
			p.Avg, p.Min, p.Max, p.Last = fp(avg), fp(mn), fp(mx), fp(last)
			out.Rows = append(out.Rows, p)
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if n >= limit {
			out.Truncated = true
		}
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].TsHour != out.Rows[j].TsHour {
			return out.Rows[i].TsHour < out.Rows[j].TsHour
		}
		return out.Rows[i].Metric < out.Rows[j].Metric
	})
	out.Count = len(out.Rows)
	return out, nil
}

// SeedQualityState loads the newest stored row's counter baselines so the
// regression checks work across a daemon restart, not only within one run. The
// monotonic clock is left unknown (0): it does not survive a restart, and
// pretending it does would flag a clock jump on every restart.
func (s *Store) SeedQualityState() QualityState {
	var st QualityState
	var ts int64
	var up, chg sql.NullFloat64
	var rx, tx sql.NullInt64
	err := s.db.QueryRow(
		`SELECT ts, uptime_s, net_rx_bytes, net_tx_bytes, battery_charge_mah
		 FROM samples ORDER BY ts DESC LIMIT 1`).Scan(&ts, &up, &rx, &tx, &chg)
	if err != nil {
		return st
	}
	st.HasPrev = true
	st.PrevTs = ts
	st.PrevMonoMS = 0
	// The baselines live in PrevCounters (that is what Evaluate compares
	// against); PrevUptime is kept in step because the reboot check reads it.
	st.PrevCounters = map[string]float64{}
	if up.Valid {
		st.PrevUptime = up.Float64
		st.PrevCounters["uptime_s"] = up.Float64
	}
	if chg.Valid {
		st.PrevCounters[chargeCounterColumn] = chg.Float64
	}
	if rx.Valid && rx.Int64 > 0 {
		st.PrevCounters["net_rx_bytes"] = float64(rx.Int64)
	}
	if tx.Valid && tx.Int64 > 0 {
		st.PrevCounters["net_tx_bytes"] = float64(tx.Int64)
	}
	return st
}

// ---------- stats ----------

type StoreStats struct {
	Path             string         `json:"path"`
	DBBytes          int64          `json:"db_bytes"`
	DBMB             float64        `json:"db_mb"`
	MainBytes        int64          `json:"main_db_bytes"`
	Samples          int64          `json:"samples"`
	Snapshots        int64          `json:"snapshots"`
	Logs             int64          `json:"logs"`
	OldestTS         int64          `json:"oldest_ts"`
	NewestTS         int64          `json:"newest_ts"`
	SpanHours        float64        `json:"span_hours"`
	BytesPerDay      float64        `json:"bytes_per_day"`
	SamplesPerDay    float64        `json:"samples_per_day"`
	SnapshotsPerDay  float64        `json:"snapshots_per_day"`
	SnapshotBytesDay float64        `json:"snapshot_bytes_per_day"`
	GrowthNote       string         `json:"growth_note,omitempty"`
	EstDaysToCap     float64        `json:"est_days_to_cap"`
	SnapshotRawBytes int64          `json:"snapshot_raw_bytes"`
	SnapshotGzBytes  int64          `json:"snapshot_gz_bytes"`
	CompressionRatio float64        `json:"compression_ratio"`
	RetentionDays    int            `json:"retention_days"`
	SnapRetentionDay int            `json:"snapshot_retention_days"`
	LogRetentionDays int            `json:"log_retention_days"`
	MaxDBMB          int            `json:"max_db_mb"`
	PrunedTotal      int64          `json:"pruned_total"`
	DroppedLogLines  int64          `json:"dropped_log_lines"`
	LogSource        map[string]int `json:"logs_by_source"`

	// --- data-quality counters (schema v3) ---
	SchemaVersion   int   `json:"schema_version"`
	SchemaExpected  int   `json:"schema_expected"`
	RollupRows      int64 `json:"rollup_hourly_rows"`
	RollupOldest    int64 `json:"rollup_oldest_ts_hour"`
	RollupNewest    int64 `json:"rollup_newest_ts_hour"`
	RollupWatermark int64 `json:"rollup_watermark_hour"`
	RollupEnabled   bool  `json:"rollup_enabled"`
	RollupRetention int   `json:"rollup_retention_days"`
	DedupedSamples  int64 `json:"deduped_samples_total"`
	RejectedSamples int64 `json:"rows_with_rejected_values"`
	RejectedValues  int64 `json:"rejected_value_total"`
	QualityRows     int64 `json:"rows_with_quality_metadata"`
	SampleColumnsN  int   `json:"metric_columns"`
}

// metaSet/metaGet store small bookkeeping values (schema version, growth
// baseline) in the meta table.
func (s *Store) metaSet(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) metaGet(k string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// mainFileBytes is the database file without the WAL. The WAL grows and is
// checkpointed back in bursts, so including it makes any short-window growth
// estimate wildly wrong (measured: 158 MB/day reported vs 66 MB/day real).
func (s *Store) mainFileBytes() int64 {
	if st, err := os.Stat(s.path); err == nil {
		return st.Size()
	}
	return 0
}

func (s *Store) Stats(h HistoryConfig) (*StoreStats, error) {
	st := &StoreStats{Path: s.path, LogSource: map[string]int{}}
	st.DBBytes = s.DBFileBytes()
	st.MainBytes = s.mainFileBytes()
	st.DBMB = round2(float64(st.DBBytes) / (1 << 20))
	count := func(q string) int64 {
		var n int64
		s.db.QueryRow(q).Scan(&n)
		return n
	}
	st.Samples = count(`SELECT COUNT(*) FROM samples`)
	st.Snapshots = count(`SELECT COUNT(*) FROM snapshots`)
	st.Logs = count(`SELECT COUNT(*) FROM logs`)
	s.db.QueryRow(`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM samples`).Scan(&st.OldestTS, &st.NewestTS)
	if h.IntervalSeconds > 0 {
		st.SamplesPerDay = round2(86400 / float64(h.IntervalSeconds))
	}
	if h.FullSeconds > 0 {
		st.SnapshotsPerDay = round2(86400 / float64(h.FullSeconds))
	}

	// The snapshot share is exact: average gzipped size × how often we take one.
	if st.Snapshots > 0 && h.FullSeconds > 0 {
		var avgGz, avgRaw float64
		s.db.QueryRow(`SELECT COALESCE(AVG(gz_bytes),0), COALESCE(AVG(raw_bytes),0) FROM snapshots`).Scan(&avgGz, &avgRaw)
		st.SnapshotBytesDay = round2(avgGz * 86400 / float64(h.FullSeconds))
		if avgRaw > 0 && avgGz > 0 {
			st.CompressionRatio = round2(avgRaw / avgGz)
		}
	}

	// Real growth is measured against a stored baseline instead of assuming the
	// database was empty at its oldest row (which backfilled logs would inflate).
	now := time.Now().Unix()
	if base, ok := s.metaGet("size_baseline_bytes"); ok {
		if bts, ok2 := s.metaGet("size_baseline_ts"); ok2 {
			var bb, bt int64
			fmt.Sscanf(base, "%d", &bb)
			fmt.Sscanf(bts, "%d", &bt)
			elapsed := now - bt
			switch {
			case elapsed >= 1800 && st.MainBytes > bb:
				st.BytesPerDay = round2(float64(st.MainBytes-bb) / float64(elapsed) * 86400)
				if h.MaxDBMB > 0 && st.BytesPerDay > 0 {
					room := float64(int64(h.MaxDBMB)<<20) - float64(st.MainBytes)
					if room < 0 {
						room = 0
					}
					st.EstDaysToCap = round2(room / st.BytesPerDay)
				}
			case elapsed < 1800:
				st.GrowthNote = fmt.Sprintf("增长速率需要至少 30 分钟基线（已积累 %d 分钟）；快照部分是精确值", elapsed/60)
			default:
				st.GrowthNote = "基线期内数据库没有增长"
			}
		}
	} else {
		s.metaSet("size_baseline_bytes", fmt.Sprint(st.MainBytes))
		s.metaSet("size_baseline_ts", fmt.Sprint(now))
		st.GrowthNote = "已建立增长基线，30 分钟后给出实测速率"
	}
	if st.OldestTS > 0 && st.NewestTS > st.OldestTS {
		st.SpanHours = round2(float64(st.NewestTS-st.OldestTS) / 3600)
	}

	s.db.QueryRow(`SELECT COALESCE(SUM(raw_bytes),0), COALESCE(SUM(gz_bytes),0) FROM snapshots`).
		Scan(&st.SnapshotRawBytes, &st.SnapshotGzBytes)
	if st.CompressionRatio == 0 && st.SnapshotRawBytes > 0 && st.SnapshotGzBytes > 0 {
		st.CompressionRatio = round2(float64(st.SnapshotRawBytes) / float64(st.SnapshotGzBytes))
	}
	st.RetentionDays = h.RetentionDays
	st.SnapRetentionDay = h.SnapshotRetentionDays
	st.LogRetentionDays = h.LogRetentionDays
	st.MaxDBMB = h.MaxDBMB
	st.PrunedTotal = s.pruned.Load()
	st.DroppedLogLines = s.sink.drops.Load()

	// Data-quality counters. schema_version is read from meta (what the file
	// actually is) and compared with the version this build writes, so a
	// half-migrated database is visible rather than assumed.
	st.SchemaExpected = schemaVersion
	st.SampleColumnsN = metricColumnCount()
	if v, ok := s.metaGet("schema"); ok {
		fmt.Sscanf(v, "%d", &st.SchemaVersion)
	}
	if v, ok := s.metaGet("samples_deduped_total"); ok {
		fmt.Sscanf(v, "%d", &st.DedupedSamples)
	}
	if v, ok := s.metaGet("rollup_hour"); ok {
		fmt.Sscanf(v, "%d", &st.RollupWatermark)
	}
	s.db.QueryRow(`SELECT COALESCE(MIN(ts_hour),0), COALESCE(MAX(ts_hour),0), COUNT(*) FROM rollup_hourly`).
		Scan(&st.RollupOldest, &st.RollupNewest, &st.RollupRows)
	st.RollupEnabled = h.RollupEnabled
	st.RollupRetention = h.RollupRetentionDays
	// A row "has rejected values" when rejected_cols holds a non-empty JSON
	// array. Counting the rows and summing the entries needs SQLite's JSON
	// functions to be present, so the entries are counted from the stored
	// column text instead — the value is small (it is an anomaly log).
	s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE rejected_cols IS NOT NULL AND rejected_cols <> '[]'`).
		Scan(&st.RejectedSamples)
	st.QualityRows = count(`SELECT COUNT(*) FROM samples WHERE measured_cols IS NOT NULL`)

	rows, err := s.db.Query(`SELECT source, COUNT(*) FROM logs GROUP BY source`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var src string
			var n int
			if err := rows.Scan(&src, &n); err == nil {
				st.LogSource[src] = n
			}
		}
	}
	return st, nil
}

// SampleColumns exposes the metric names for the API/UI.
func SampleColumns() []string {
	out := make([]string, 0, len(sampleColumns))
	for _, c := range sampleColumns {
		if c == "ts" || c == "wakefulness" || c == "battery_status" || c == "battery_health" ||
			c == "hottest_zone" || c == "wifi_ssid" || c == "cpu_freqs_khz" {
			continue // strings/arrays: not graphed
		}
		// The schema v3 quality metadata describes the row rather than the
		// phone: listing absent_cols among the "metrics to graph" would invite
		// someone to chart it.
		if sampleQualityColumnSet[c] {
			continue
		}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// metricColumnCount is how many columns a fully measured row carries: the
// metric columns, excluding ts and the quality metadata. It is the denominator
// behind measured_cols, so it must not drift from sampleColumns.
func metricColumnCount() int {
	n := 0
	for _, c := range sampleColumns {
		if c == "ts" || sampleQualityColumnSet[c] {
			continue
		}
		n++
	}
	return n
}
