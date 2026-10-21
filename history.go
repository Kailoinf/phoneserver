// history.go — the sampling loop that feeds the SQLite store.
//
// Battery cost was the thing to get right here, so the design is explicit:
//
//   - The loop sleeps for the whole interval (no 1-second heartbeat), so the
//     daemon stays parked on a timer instead of waking up constantly.
//   - A "full" tick calls the ordinary snapshot collector: measured at ~146 ms
//     of process CPU time on this device, because it spawns getprop / cmd wifi
//     / dumpsys. A "cheap" tick reuses only sysfs and /proc collectors and
//     costs a few ms. Both write a samples row; the `full` column says which.
//   - With full_seconds == interval_seconds (the default) every tick is full,
//     i.e. exactly "all data every 10 seconds". Raise full_seconds to trade
//     completeness for battery: at 10s/60s the loop averages ~25 ms per tick.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// HistoryConfig controls local persistence.
type HistoryConfig struct {
	Enabled         bool   `json:"enabled"`
	DBPath          string `json:"db_path"`
	IntervalSeconds int    `json:"interval_seconds"`
	// FullSeconds is how often the *complete* document is collected and stored.
	// Equal to IntervalSeconds means every tick is complete. Larger values mean
	// only the scalar columns are written on the in-between ticks, which is
	// where the battery savings come from.
	FullSeconds int `json:"full_seconds"`
	// RetentionDays applies to samples; snapshots (the bulky table) and logs
	// have their own, usually shorter, windows.
	RetentionDays         int  `json:"retention_days"`
	SnapshotRetentionDays int  `json:"snapshot_retention_days"`
	LogRetentionDays      int  `json:"log_retention_days"`
	MaxDBMB               int  `json:"max_db_mb"`
	BackfillLogs          bool `json:"backfill_logs"`
	PruneEverySeconds     int  `json:"prune_every_seconds"`
}

func defaultHistoryConfig() HistoryConfig {
	return HistoryConfig{
		Enabled:               true,
		DBPath:                "/data/adb/phoneserver/history.db",
		IntervalSeconds:       10,
		FullSeconds:           10,
		RetentionDays:         30,
		SnapshotRetentionDays: 3,
		LogRetentionDays:      7,
		MaxDBMB:               512,
		BackfillLogs:          true,
		PruneEverySeconds:     300,
	}
}

// norm fills in sane values for anything the config left zero/absurd, so a
// hand-edited or older config cannot produce a 0-second loop.
func (h HistoryConfig) norm() HistoryConfig {
	def := defaultHistoryConfig()
	if h.DBPath == "" {
		h.DBPath = def.DBPath
	}
	if h.IntervalSeconds < 2 {
		h.IntervalSeconds = def.IntervalSeconds
	}
	if h.IntervalSeconds > 3600 {
		h.IntervalSeconds = 3600
	}
	if h.FullSeconds <= 0 {
		h.FullSeconds = h.IntervalSeconds
	}
	if h.FullSeconds < h.IntervalSeconds {
		h.FullSeconds = h.IntervalSeconds
	}
	if h.RetentionDays <= 0 {
		h.RetentionDays = def.RetentionDays
	}
	if h.SnapshotRetentionDays <= 0 {
		h.SnapshotRetentionDays = def.SnapshotRetentionDays
	}
	if h.LogRetentionDays <= 0 {
		h.LogRetentionDays = def.LogRetentionDays
	}
	if h.MaxDBMB <= 0 {
		h.MaxDBMB = def.MaxDBMB
	}
	if h.PruneEverySeconds < 30 {
		h.PruneEverySeconds = def.PruneEverySeconds
	}
	return h
}

// ---------- snapshot -> sample row ----------

func sampleFromSnapshot(snap Snapshot, full int, ts int64) Sample {
	sm := Sample{Ts: ts, Full: full}

	b := snap.Battery
	sm.BatteryCapacityPct = float64(b.CapacityPercent)
	sm.BatteryStatus = b.Status
	sm.BatteryHealth = b.Health
	sm.BatteryTempC = b.TempC
	sm.BatteryVoltageV = b.VoltageV
	sm.BatteryCurrentA = b.CurrentNowA
	sm.BatteryChargeMAh = b.ChargeCounterMAh
	sm.BatteryFullMAh = b.ChargeFullMAh
	sm.BatteryHealthPct = b.HealthPercent
	sm.BatteryCycles = b.CycleCount
	sm.BatteryTimeFullS = b.TimeToFullSeconds
	sm.BatteryTimeEmptyS = b.TimeToEmptySeconds

	bin := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if u := snap.Power.USB; u != nil {
		if u.Online != nil {
			sm.UsbOnline = bin(*u.Online)
		}
		sm.UsbPresent = bin(u.Present)
		sm.UsbVoltageV = u.VoltageNowV
		sm.UsbCurrentA = u.CurrentNowA
	}
	if w := snap.Power.Wireless; w != nil && w.Online != nil {
		sm.WirelessOnline = bin(*w.Online)
	}
	sm.Wakefulness = snap.Power.Wakefulness
	if sm.Wakefulness == "" {
		sm.Wakefulness = snap.Display.Wakefulness
	}
	sm.ScreenOn = bin(snap.Display.ScreenOn)
	sm.Brightness = snap.Display.Brightness
	sm.BrightnessMax = snap.Display.BrightnessMax

	c := snap.CPU
	sm.CPUUsagePct = c.UsagePercent
	sm.CPUCores = c.CoreCount
	sm.CPUSampleMS = c.SampleIntervalMS
	if len(c.LoadAvg) > 0 {
		sm.CPULoad1 = c.LoadAvg[0]
	}
	if len(c.LoadAvg) > 1 {
		sm.CPULoad5 = c.LoadAvg[1]
	}
	if len(c.LoadAvg) > 2 {
		sm.CPULoad15 = c.LoadAvg[2]
	}
	if len(c.Cores) > 0 {
		freqs := make([]int64, 0, len(c.Cores))
		for _, core := range c.Cores {
			freqs = append(freqs, core.CurKHz)
		}
		if body, err := json.Marshal(freqs); err == nil {
			sm.CPUFreqKHz = string(body)
		}
	}

	sm.GPUBusyPct = snap.GPU.BusyPercent
	sm.GPUHz = snap.GPU.CurrentHz
	sm.GPUTempC = snap.GPU.TempC

	sm.MemTotalKB = snap.Memory.TotalKB
	sm.MemAvailableKB = snap.Memory.AvailableKB
	sm.MemUsedPct = snap.Memory.UsedPercent
	sm.SwapFreeKB = snap.Memory.SwapFreeKB

	for _, st := range snap.Storage {
		switch st.Mount {
		case "/data":
			sm.DataFreeBytes, sm.DataTotalBytes, sm.DataUsedPct = st.FreeBytes, st.TotalBytes, st.UsedPercent
		case "/":
			sm.RootFreeBytes = st.FreeBytes
		}
	}

	sm.MaxTempC = snap.Thermal.MaxTempC
	sm.HottestZone = snap.Thermal.HottestZone
	sm.ZoneCount = snap.Thermal.ZoneCount
	sm.TempZoneCount = snap.Thermal.TemperatureZoneCount

	n := snap.Network
	sm.NetRxBytes, sm.NetTxBytes = n.TotalRxBytes, n.TotalTxBytes
	sm.NetRxBps, sm.NetTxBps = n.TotalRxPerSec, n.TotalTxPerSec
	sm.NetSampleMS = n.SampleIntervalMS

	w := snap.Wifi
	sm.WifiConnected = bin(w.Connected)
	sm.WifiSSID = w.SSID
	sm.WifiRSSIDbm = w.RSSIDbm
	sm.WifiSignalPct = w.SignalPercent
	sm.WifiLinkMbps = w.LinkSpeedMbps
	sm.WifiFreqMHz = w.FrequencyMHz
	sm.WifiChannel = w.Channel

	sm.UptimeS = snap.System.UptimeSeconds
	sm.ProcCount = snap.System.ProcessCount
	if snap.System.SELinuxEnforcing {
		sm.SelinuxEnforcing = 1
	}
	sm.PkgTotal = snap.Packages.Total
	sm.PkgUser = snap.Packages.User
	sm.PkgSystem = snap.Packages.System
	sm.PkgDisabled = snap.Packages.Disabled
	return sm
}

// ---------- cheap tick ----------

// collectCheapSample fills a row from sysfs and /proc only. It deliberately
// skips the collectors that spawn processes (getprop, `cmd wifi status`,
// dumpsys) — that is the whole point of the cheap path. The columns those
// would have filled stay empty and the row is marked full=0.
func (a *App) collectCheapSample(ts int64) Sample {
	sm := Sample{Ts: ts, Full: 0}

	b := collectBattery()
	sm.BatteryCapacityPct = float64(b.CapacityPercent)
	sm.BatteryStatus = b.Status
	sm.BatteryHealth = b.Health
	sm.BatteryTempC = b.TempC
	sm.BatteryVoltageV = b.VoltageV
	sm.BatteryCurrentA = b.CurrentNowA
	sm.BatteryChargeMAh = b.ChargeCounterMAh
	sm.BatteryFullMAh = b.ChargeFullMAh
	sm.BatteryHealthPct = b.HealthPercent
	sm.BatteryCycles = b.CycleCount

	// Power supplies are plain sysfs reads: keep those, skip wakefulness
	// (dumpsys) and infer the screen from the backlight instead.
	dirs, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, d := range dirs {
		ps := collectPowerSupply(d) // cheap: all sysfs
		switch ps.Name {
		case "usb":
			if ps.Online != nil {
				if *ps.Online {
					sm.UsbOnline = 1
				}
			}
			if ps.Present {
				sm.UsbPresent = 1
			}
			sm.UsbVoltageV, sm.UsbCurrentA = ps.VoltageNowV, ps.CurrentNowA
		case "wireless":
			if ps.Online != nil && *ps.Online {
				sm.WirelessOnline = 1
			}
		}
	}
	for _, pat := range []string{"/sys/class/backlight/*/brightness", "/sys/class/leds/lcd-backlight/brightness"} {
		found := false
		matches, _ := filepath.Glob(pat)
		for _, f := range matches {
			if v, ok := rdInt(f); ok {
				sm.Brightness = v
				if mx, ok := rdInt(filepath.Dir(f) + "/max_brightness"); ok {
					sm.BrightnessMax = mx
				}
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if sm.Brightness > 0 {
		sm.ScreenOn = 1
		sm.Wakefulness = "Awake"
	} else {
		sm.Wakefulness = "Asleep"
	}

	// CPU and network rates come from the ticker's own sampler so the API's
	// request-path deltas are not disturbed by this loop.
	c := a.tickSampler.cpu()
	sm.CPUUsagePct = c.UsagePercent
	sm.CPUCores = c.CoreCount
	sm.CPUSampleMS = c.SampleIntervalMS
	if len(c.LoadAvg) > 0 {
		sm.CPULoad1 = c.LoadAvg[0]
	}
	if len(c.LoadAvg) > 1 {
		sm.CPULoad5 = c.LoadAvg[1]
	}
	if len(c.LoadAvg) > 2 {
		sm.CPULoad15 = c.LoadAvg[2]
	}
	if len(c.Cores) > 0 {
		freqs := make([]int64, 0, len(c.Cores))
		for _, core := range c.Cores {
			freqs = append(freqs, core.CurKHz)
		}
		if body, err := json.Marshal(freqs); err == nil {
			sm.CPUFreqKHz = string(body)
		}
	}
	n := a.tickSampler.network()
	sm.NetRxBytes, sm.NetTxBytes = n.TotalRxBytes, n.TotalTxBytes
	sm.NetRxBps, sm.NetTxBps = n.TotalRxPerSec, n.TotalTxPerSec
	sm.NetSampleMS = n.SampleIntervalMS

	g := collectGPU()
	sm.GPUBusyPct, sm.GPUHz, sm.GPUTempC = g.BusyPercent, g.CurrentHz, g.TempC

	m := collectMemory()
	sm.MemTotalKB, sm.MemAvailableKB, sm.MemUsedPct, sm.SwapFreeKB = m.TotalKB, m.AvailableKB, m.UsedPercent, m.SwapFreeKB

	for _, st := range collectStorage(a.config().Mounts) {
		switch st.Mount {
		case "/data":
			sm.DataFreeBytes, sm.DataTotalBytes, sm.DataUsedPct = st.FreeBytes, st.TotalBytes, st.UsedPercent
		case "/":
			sm.RootFreeBytes = st.FreeBytes
		}
	}

	th := collectThermal()
	sm.MaxTempC, sm.HottestZone = th.MaxTempC, th.HottestZone
	sm.ZoneCount, sm.TempZoneCount = th.ZoneCount, th.TemperatureZoneCount

	if u, ok := rdFloat("/proc/uptime"); ok {
		sm.UptimeS = round2(u)
	}
	if rd("/sys/fs/selinux/enforce") == "1" {
		sm.SelinuxEnforcing = 1
	}
	return sm
}

// ---------- the loop ----------

// startHistoryLoop samples forever. It is a single goroutine that sleeps for
// the whole interval between ticks (no heartbeat polls), and re-reads the
// config each round so interval/retention changes apply without a restart.
func (a *App) startHistoryLoop(logger *log.Logger, stop <-chan struct{}) {
	lastFull := time.Time{}
	failures := 0
	for {
		h := a.config().History.norm()
		if !h.Enabled || a.store == nil {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Second):
			}
			continue
		}

		select {
		case <-stop:
			return
		case <-time.After(time.Duration(h.IntervalSeconds) * time.Second):
		}

		full := h.FullSeconds <= h.IntervalSeconds ||
			time.Since(lastFull) >= time.Duration(h.FullSeconds)*time.Second-time.Second
		start := time.Now()
		ts := start.Unix()

		var sm Sample
		if full {
			snap := a.snapshot()
			sm = sampleFromSnapshot(snap, 1, ts)
			if body, err := json.Marshal(snap); err == nil {
				if _, err := a.store.InsertSnapshot(ts, body); err != nil {
					a.histErrf.Store(err.Error())
				}
			}
			lastFull = start
			a.histFulls.Add(1)
		} else {
			sm = a.collectCheapSample(ts)
		}
		sm.CollectMS = time.Since(start).Milliseconds()
		if err := a.store.InsertSample(sm); err != nil {
			failures++
			a.histErrf.Store(err.Error())
			if failures == 1 || failures%30 == 0 {
				logger.Printf("history: sample insert failed (%d so far): %v", failures, err)
			}
		} else {
			failures = 0
			a.histTicks.Add(1)
		}

		if start.Unix()-a.store.lastPrune.Load() >= int64(h.PruneEverySeconds) {
			if res, err := a.store.Prune(start, h); err != nil {
				logger.Printf("history: prune failed: %v", err)
			} else if res.SamplesDeleted+res.SnapshotsDeleted+res.LogsDeleted+res.SizeCapEvicted > 0 {
				logger.Printf("history: pruned samples=%d snapshots=%d logs=%d size_cap=%d (%d MB -> %d MB)",
					res.SamplesDeleted, res.SnapshotsDeleted, res.LogsDeleted, res.SizeCapEvicted,
					res.DBBytesBefore>>20, res.DBBytesAfter>>20)
			}
		}
	}
}

// historyStatus is what /api/v1/history/stats and the WebUI report.
type historyStatus struct {
	Enabled         bool           `json:"enabled"`
	LoopTicks       int64          `json:"ticks"`
	FullTicks       int64          `json:"full_ticks"`
	IntervalSeconds int            `json:"interval_seconds"`
	FullSeconds     int            `json:"full_seconds"`
	LastError       string         `json:"last_error,omitempty"`
	CostEstimate    map[string]any `json:"cost_estimate"`
	Store           *StoreStats    `json:"store"`
	Config          HistoryConfig  `json:"config"`
}

// costEstimate turns the loop settings into the numbers that actually matter
// to the person paying for the battery.
func costEstimate(h HistoryConfig) map[string]any {
	h = h.norm()
	perDay := 86400.0 / float64(h.IntervalSeconds)
	fullPerDay := 86400.0 / float64(h.FullSeconds)
	if fullPerDay > perDay {
		fullPerDay = perDay
	}
	cheapPerDay := perDay - fullPerDay
	// Measured on the reference device: ~146 ms of process CPU per full
	// collection (it spawns getprop/cmd/dumpsys), ~8 ms for a cheap one.
	const fullMS, cheapMS = 146.0, 8.0
	cpuSecPerDay := (fullPerDay*fullMS + cheapPerDay*cheapMS) / 1000
	return map[string]any{
		"collections_per_day":       int(perDay),
		"full_collections_per_day":  int(fullPerDay),
		"cheap_collections_per_day": int(cheapPerDay),
		"cpu_seconds_per_day":       round2(cpuSecPerDay),
		"avg_cpu_percent_of_core":   round2(cpuSecPerDay / 86400 * 100),
		"assumed_full_tick_ms":      fullMS,
		"assumed_cheap_tick_ms":     cheapMS,
		"note":                      "full ticks run getprop/cmd wifi/dumpsys; cheap ticks read sysfs+proc only",
	}
}

func (a *App) historyStatus() historyStatus {
	h := a.config().History.norm()
	st := historyStatus{
		Enabled:         h.Enabled && a.store != nil,
		LoopTicks:       a.histTicks.Load(),
		FullTicks:       a.histFulls.Load(),
		IntervalSeconds: h.IntervalSeconds,
		FullSeconds:     h.FullSeconds,
		CostEstimate:    costEstimate(h),
		Config:          h,
	}
	if e, ok := a.histErrf.Load().(string); ok && e != "" {
		st.LastError = e
	}
	if a.store != nil {
		if s, err := a.store.Stats(h); err == nil {
			st.Store = s
		}
	}
	return st
}

// openHistory wires up the store, the log tee and the sampling loop. It is
// deliberately forgiving: if any of it fails the API keeps working, just
// without local history.
func (a *App) openHistory(cfg Config, logw io.Writer, stderr io.Writer) (io.Writer, error) {
	h := cfg.History.norm()
	if !h.Enabled {
		return logw, nil
	}
	store, err := openStore(h.DBPath, func(f string, v ...any) {
		fmt.Fprintf(stderr, "phoneserver: "+f+"\n", v...)
	})
	if err != nil {
		return logw, fmt.Errorf("history disabled: %w", err)
	}
	a.store = store
	a.tickSampler = newSampler(a)
	return io.MultiWriter(logw, store.LogSink()), nil
}

// backfillExistingLogs imports the log files that exist before the database
// did, so the history is not "since the upgrade" only.
func (a *App) backfillExistingLogs(cfg Config, logger *log.Logger) {
	h := cfg.History.norm()
	if a.store == nil || !h.BackfillLogs {
		return
	}
	paths := []string{
		filepath.Join(cfg.LogDir, "phoneserver.log"),
		filepath.Join(cfg.LogDir, "phoneserver.log.1"),
		filepath.Join(cfg.LogDir, "phoneserver.log.2"),
		filepath.Join(cfg.LogDir, "phoneserver.log.3"),
		filepath.Join(cfg.LogDir, "stdout.log"),
	}
	var present []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		return
	}
	inserted, files, err := a.store.BackfillLogs(present)
	if err != nil {
		logger.Printf("history: log backfill failed: %v", err)
		return
	}
	logger.Printf("history: backfilled %d log lines from %d file(s) into the database", inserted, files)
}
