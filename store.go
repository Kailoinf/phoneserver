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

const schemaVersion = 1

// ---------- flattened sample row ----------

// Sample is one tick of scalar metrics. Every field becomes a column, so the
// set is deliberately "the numbers you would graph". Anything not here is
// still recoverable from the gzipped snapshots table.
type Sample struct {
	Ts int64 `json:"ts"`

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
}

// values mirrors sampleColumns exactly — keep the two in lockstep.
func (s Sample) values() []any {
	return []any{
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
	}
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

	mu      sync.Mutex // serialises writers; SQLite wants one writer
	sink    *logSink
	stopCh  chan struct{}
	stopped sync.Once

	lastPrune atomic.Int64
	pruned    atomic.Int64 // total rows removed
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
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path, sink: &logSink{ch: make(chan logRow, 8192)}}
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
				switch {
				case c == "ts":
					cols = append(cols, "ts INTEGER PRIMARY KEY")
				case c == "battery_status", c == "battery_health", c == "wakefulness",
					c == "hottest_zone", c == "wifi_ssid", c == "cpu_freqs_khz":
					cols = append(cols, c+" TEXT")
				case strings.HasSuffix(c, "_bytes"):
					cols = append(cols, c+" INTEGER")
				default:
					cols = append(cols, c+" REAL")
				}
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
			line TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_logs_unique ON logs(ts_ms, line)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_ts ON snapshots(ts)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w (statement: %s)", err, firstLine(q))
		}
	}
	_ = schemaVersion // keep the version visible next to the migration list
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES('schema',?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, fmt.Sprint(schemaVersion))
	return err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
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
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
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
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
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
	st, err := tx.Prepare(`INSERT OR IGNORE INTO logs (ts_ms, level, source, line) VALUES (?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	n := 0
	for _, r := range rows {
		res, err := st.Exec(r.TsMS, r.Level, r.Source, r.Line)
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
	SamplesDeleted   int64 `json:"samples_deleted"`
	SnapshotsDeleted int64 `json:"snapshots_deleted"`
	LogsDeleted      int64 `json:"logs_deleted"`
	SizeCapEvicted   int64 `json:"size_cap_evicted"`
	DBBytesBefore    int64 `json:"db_bytes_before"`
	DBBytesAfter     int64 `json:"db_bytes_after"`
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
	if c := cut(h.RetentionDays); c > 0 {
		res.SamplesDeleted = exec(`DELETE FROM samples WHERE ts < ?`, c)
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
	Count     int       `json:"count"`
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

	out := &SeriesResult{
		Count: len(points), Columns: cols, Points: points,
		Since: since, Until: until, Truncated: truncated,
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
			st.Last[c] = round2(vals[len(vals)-1])
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
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
