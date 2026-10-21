package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- tiny sysfs/exec helpers ----------

// rd returns the trimmed contents of a sysfs/proc file, or "" when unreadable.
func rd(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func rdInt(path string) (int64, bool) {
	s := rd(path)
	if s == "" {
		return 0, false
	}
	// Some sysfs files are "15 %" or "0.4" — take the leading number.
	s = strings.TrimSpace(strings.Fields(s)[0])
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func rdFloat(path string) (float64, bool) {
	s := rd(path)
	if s == "" {
		return 0, false
	}
	s = strings.TrimSpace(strings.Fields(s)[0])
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// binPath resolves an Android system binary, preferring the real path over PATH.
func binPath(name string) string {
	for _, c := range []string{"/system/bin/" + name, "/system/xbin/" + name} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return name
}

// shOut runs a command with a hard timeout so a hung dumpsys cannot wedge a request.
func shOut(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("%s timed out after %s", filepath.Base(name), timeout)
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("%s: %s", filepath.Base(name), msg)
	}
	return out.String(), nil
}

func round2(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*100) / 100
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return round2(part / whole * 100)
}

// ---------- ttl cache ----------

type cacheEntry struct {
	val any
	exp time.Time
}

type ttlCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

func newTTLCache() *ttlCache { return &ttlCache{m: map[string]cacheEntry{}} }

func (c *ttlCache) get(k string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		return nil, false
	}
	return e.val, true
}

func (c *ttlCache) put(k string, v any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = cacheEntry{val: v, exp: time.Now().Add(ttl)}
}

// cacheGet memoizes fn's result for ttl. Errors are also cached but for a short
// period, so a broken source is retried soon instead of on every request.
func cacheGet[T any](c *ttlCache, k string, ttl time.Duration, fn func() (T, error)) (T, error) {
	if v, ok := c.get(k); ok {
		switch t := v.(type) {
		case T:
			return t, nil
		case error:
			var zero T
			return zero, t
		}
	}
	v, err := fn()
	if err != nil {
		c.put(k, err, 5*time.Second)
		var zero T
		return zero, err
	}
	c.put(k, v, ttl)
	return v, nil
}

// ---------- document types ----------

type Device struct {
	Manufacturer       string `json:"manufacturer"`
	Brand              string `json:"brand"`
	Model              string `json:"model"`
	MarketName         string `json:"market_name"`
	Device             string `json:"device"`
	Product            string `json:"product"`
	Board              string `json:"board"`
	SoC                string `json:"soc_platform"`
	SoCModel           string `json:"soc_model"`
	AndroidRelease     string `json:"android_release"`
	AndroidSDK         int    `json:"android_sdk"`
	BuildID            string `json:"build_id"`
	BuildFlavor        string `json:"build_flavor"`
	BuildType          string `json:"build_type"`
	BuildDate          string `json:"build_date"`
	BuildFingerprint   string `json:"build_fingerprint"`
	SecurityPatch      string `json:"security_patch"`
	ROMSystem          string `json:"rom_system"`
	ROMIncremental     string `json:"rom_incremental"`
	ROMDisplayID       string `json:"rom_display_id"`
	Arch               string `json:"arch"`
	ArchList32         string `json:"arch_list32"`
	Kernel             string `json:"kernel"`
	Baseband           string `json:"baseband"`
	Serial             string `json:"serial"`
	Bootloader         string `json:"bootloader"`
	SlotSuffix         string `json:"slot_suffix"`
	BootloaderUnlocked bool   `json:"bootloader_unlocked"`
	Hostname           string `json:"hostname"`
	DeviceID           string `json:"device_id"`
}

type Battery struct {
	Present                 bool    `json:"present"`
	Status                  string  `json:"status"`
	Health                  string  `json:"health"`
	CapacityPercent         int64   `json:"capacity_percent"`
	TempC                   float64 `json:"temp_c"`
	VoltageV                float64 `json:"voltage_v"`
	VoltageOCPV             float64 `json:"voltage_ocv_v"`
	CurrentNowUA            int64   `json:"current_now_ua"`
	CurrentNowA             float64 `json:"current_now_a"`
	ChargeCounterMAh        float64 `json:"charge_counter_mah"`
	ChargeFullMAh           float64 `json:"charge_full_mah"`
	ChargeFullDesignMAh     float64 `json:"charge_full_design_mah"`
	HealthPercent           float64 `json:"health_percent"`
	CycleCount              int64   `json:"cycle_count"`
	TimeToFullSeconds       int64   `json:"time_to_full_seconds"`
	TimeToEmptySeconds      int64   `json:"time_to_empty_seconds"`
	Technology              string  `json:"technology"`
	ModelName               string  `json:"model_name"`
	ChargeControlLimit      int64   `json:"charge_control_limit"`
	ConstantChargeCurrentUA int64   `json:"constant_charge_current_ua"`
	Charging                bool    `json:"charging"`
}

type PowerSupply struct {
	Name            string `json:"name"`
	Type            string `json:"type,omitempty"`
	RealType        string `json:"real_type,omitempty"`
	Online          *bool  `json:"online,omitempty"`
	Present         bool   `json:"present,omitempty"`
	Status          string `json:"status,omitempty"`
	Health          string `json:"health,omitempty"`
	Technology      string `json:"technology,omitempty"`
	CapacityPercent int64  `json:"capacity_percent,omitempty"`
	// Raw sysfs integers are always exposed; the *_v/*_a fields are the same
	// values converted assuming the kernel power_supply convention. Vendor
	// drivers can deviate (the "wireless" node on this device reports mV), so
	// keep the raw value if a number looks implausible.
	VoltageNowRaw        int64   `json:"voltage_now_raw,omitempty"`
	VoltageNowV          float64 `json:"voltage_now_v,omitempty"`
	VoltageMaxRaw        int64   `json:"voltage_max_raw,omitempty"`
	VoltageMaxV          float64 `json:"voltage_max_v,omitempty"`
	CurrentNowRaw        int64   `json:"current_now_raw,omitempty"`
	CurrentNowA          float64 `json:"current_now_a,omitempty"`
	CurrentMaxRaw        int64   `json:"current_max_raw,omitempty"`
	CurrentMaxA          float64 `json:"current_max_a,omitempty"`
	InputCurrentLimitRaw int64   `json:"input_current_limit_raw,omitempty"`
	InputCurrentLimitA   float64 `json:"input_current_limit_a,omitempty"`
	ChargeType           string  `json:"charge_type,omitempty"`
	TempC                float64 `json:"temp_c,omitempty"`
}

type Power struct {
	Wakefulness string        `json:"wakefulness"`
	ScreenOn    bool          `json:"screen_on"`
	Supplies    []PowerSupply `json:"supplies"`
	UnitsNote   string        `json:"units_note"`
	USB         *PowerSupply  `json:"usb,omitempty"`
	Wireless    *PowerSupply  `json:"wireless,omitempty"`
	Error       string        `json:"error,omitempty"`
}

type Display struct {
	Wakefulness      string `json:"wakefulness"`
	ScreenOn         bool   `json:"screen_on"`
	Brightness       int64  `json:"brightness"`
	BrightnessMax    int64  `json:"brightness_max"`
	BrightnessSource string `json:"brightness_source"`
	RefreshHint      string `json:"refresh_hint,omitempty"`
}

type CPUCore struct {
	ID       int    `json:"id"`
	CurKHz   int64  `json:"cur_khz"`
	MinKHz   int64  `json:"min_khz"`
	MaxKHz   int64  `json:"max_khz"`
	Governor string `json:"governor"`
}

type CPUInfo struct {
	Model            string    `json:"model"`
	CoreCount        int       `json:"core_count"`
	UsagePercent     float64   `json:"usage_percent"`
	PerCorePercent   []float64 `json:"per_core_percent"`
	Cores            []CPUCore `json:"cores"`
	LoadAvg          []float64 `json:"load_avg"`
	LoadRaw          string    `json:"load_raw"`
	UptimeSeconds    float64   `json:"uptime_seconds"`
	SampleIntervalMS int64     `json:"sample_interval_ms"`
}

type GPU struct {
	Model            string  `json:"model"`
	BusyPercent      int64   `json:"busy_percent"`
	CurrentHz        int64   `json:"current_hz"`
	MaxHz            int64   `json:"max_hz"`
	TempC            float64 `json:"temp_c"`
	MinPowerLevel    int64   `json:"min_power_level"`
	MaxPowerLevel    int64   `json:"max_power_level"`
	AvailableFreqsHz []int64 `json:"available_freqs_hz,omitempty"`
}

type Memory struct {
	TotalKB     int64   `json:"total_kb"`
	FreeKB      int64   `json:"free_kb"`
	AvailableKB int64   `json:"available_kb"`
	BuffersKB   int64   `json:"buffers_kb"`
	CachedKB    int64   `json:"cached_kb"`
	ShmemKB     int64   `json:"shmem_kb"`
	DirtyKB     int64   `json:"dirty_kb"`
	SlabKB      int64   `json:"slab_kb"`
	SwapTotalKB int64   `json:"swap_total_kb"`
	SwapFreeKB  int64   `json:"swap_free_kb"`
	UsedPercent float64 `json:"used_percent"`
}

type Storage struct {
	Mount       string  `json:"mount"`
	Device      string  `json:"device,omitempty"`
	FSType      string  `json:"fstype,omitempty"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
	Writable    bool    `json:"writable"`
	FSFrom      string  `json:"fs_from,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type ThermalZone struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	TempC   float64 `json:"temp_c"`
	TempRaw int64   `json:"temp_raw"`
	IsTemp  bool    `json:"is_temperature"`
	Note    string  `json:"note,omitempty"`
}

type Thermal struct {
	ZoneCount            int           `json:"zone_count"`
	TemperatureZoneCount int           `json:"temperature_zone_count"`
	CoolingDeviceCount   int           `json:"cooling_device_count"`
	MaxTempC             float64       `json:"max_temp_c"`
	HottestZone          string        `json:"hottest_zone"`
	Zones                []ThermalZone `json:"zones"`
	NonTemperatureZones  []ThermalZone `json:"non_temperature_zones,omitempty"`
}

type NetIface struct {
	Name            string   `json:"name"`
	Up              bool     `json:"up"`
	MAC             string   `json:"mac,omitempty"`
	MTU             int      `json:"mtu,omitempty"`
	IPv4            []string `json:"ipv4,omitempty"`
	IPv6            []string `json:"ipv6,omitempty"`
	RxBytes         uint64   `json:"rx_bytes"`
	TxBytes         uint64   `json:"tx_bytes"`
	RxPackets       uint64   `json:"rx_packets"`
	TxPackets       uint64   `json:"tx_packets"`
	RxErrors        uint64   `json:"rx_errors"`
	TxErrors        uint64   `json:"tx_errors"`
	RxDropped       uint64   `json:"rx_dropped"`
	TxDropped       uint64   `json:"tx_dropped"`
	RxBytesPerSec   float64  `json:"rx_bytes_per_sec"`
	TxBytesPerSec   float64  `json:"tx_bytes_per_sec"`
	RxPacketsPerSec float64  `json:"rx_packets_per_sec"`
	TxPacketsPerSec float64  `json:"tx_packets_per_sec"`
}

type Network struct {
	Interfaces       []NetIface `json:"interfaces"`
	TotalRxBytes     uint64     `json:"total_rx_bytes"`
	TotalTxBytes     uint64     `json:"total_tx_bytes"`
	TotalRxPerSec    float64    `json:"total_rx_bytes_per_sec"`
	TotalTxPerSec    float64    `json:"total_tx_bytes_per_sec"`
	SampleIntervalMS int64      `json:"sample_interval_ms"`
}

type Wifi struct {
	Enabled         bool    `json:"enabled"`
	Connected       bool    `json:"connected"`
	SSID            string  `json:"ssid,omitempty"`
	BSSID           string  `json:"bssid,omitempty"`
	DeviceMAC       string  `json:"device_mac,omitempty"`
	IPv4            string  `json:"ipv4,omitempty"`
	RSSIDbm         int     `json:"rssi_dbm"`
	SignalPercent   int     `json:"signal_percent"`
	LinkSpeedMbps   int     `json:"link_speed_mbps"`
	TxLinkSpeedMbps int     `json:"tx_link_speed_mbps"`
	RxLinkSpeedMbps int     `json:"rx_link_speed_mbps"`
	FrequencyMHz    int     `json:"frequency_mhz"`
	Channel         int     `json:"channel"`
	Band            string  `json:"band,omitempty"`
	Standard        string  `json:"standard,omitempty"`
	SecurityType    int     `json:"security_type"`
	SupplicantState string  `json:"supplicant_state,omitempty"`
	Metered         bool    `json:"metered"`
	TxPackets       int64   `json:"tx_packets"`
	TxRetries       int64   `json:"tx_retries"`
	TxLost          int64   `json:"tx_lost"`
	RxPackets       int64   `json:"rx_packets"`
	TxPacketsPerSec float64 `json:"tx_packets_per_sec"`
	RxPacketsPerSec float64 `json:"rx_packets_per_sec"`
	TxRetriesPerSec float64 `json:"tx_retries_per_sec"`
	TxLostPerSec    float64 `json:"tx_lost_per_sec"`
	Error           string  `json:"error,omitempty"`
}

type System struct {
	UptimeSeconds    float64   `json:"uptime_seconds"`
	IdleSeconds      float64   `json:"idle_seconds"`
	BootTimeRFC3339  string    `json:"boot_time"`
	Kernel           string    `json:"kernel"`
	KernelVersion    string    `json:"kernel_version"`
	AndroidSDK       int       `json:"android_sdk"`
	SELinuxEnforcing bool      `json:"selinux_enforcing"`
	SELinuxMode      string    `json:"selinux_mode"`
	ProcessCount     int       `json:"process_count"`
	LoadAvg          []float64 `json:"load_avg"`
	LoadRaw          string    `json:"load_raw"`
	Hostname         string    `json:"hostname"`
	Timezone         string    `json:"timezone"`
	TimezoneName     string    `json:"timezone_name"`
	UTCOffsetSeconds int       `json:"utc_offset_seconds"`
	PageSize         int       `json:"page_size"`
}

type ProcEntry struct {
	PID   int    `json:"pid"`
	Name  string `json:"name"`
	RSSKB int64  `json:"rss_kb"`
}

type Processes struct {
	Count  int         `json:"count"`
	TopRSS []ProcEntry `json:"top_by_rss"`
}

type Packages struct {
	Total              int    `json:"total"`
	System             int    `json:"system"`
	User               int    `json:"user"`
	Disabled           int    `json:"disabled"`
	UninstalledForUser int    `json:"uninstalled_for_user"`
	Error              string `json:"error,omitempty"`
}

// Snapshot is the "everything" document, also the push payload.
type Snapshot struct {
	CollectedAt        string    `json:"collected_at"`
	DeviceID           string    `json:"device_id"`
	ServerVersion      string    `json:"server_version"`
	ServerUptimeSecond float64   `json:"server_uptime_seconds"`
	Device             Device    `json:"device"`
	Battery            Battery   `json:"battery"`
	Power              Power     `json:"power"`
	Display            Display   `json:"display"`
	CPU                CPUInfo   `json:"cpu"`
	GPU                GPU       `json:"gpu"`
	Memory             Memory    `json:"memory"`
	Storage            []Storage `json:"storage"`
	Thermal            Thermal   `json:"thermal"`
	Network            Network   `json:"network"`
	Wifi               Wifi      `json:"wifi"`
	System             System    `json:"system"`
	Processes          Processes `json:"processes"`
	Packages           Packages  `json:"packages"`
}

// ---------- sampler (delta-based metrics) ----------

type cpuTimes struct {
	total uint64
	idle  uint64
}

type netCounters struct {
	rxBytes, txBytes     uint64
	rxPackets, txPackets uint64
}

type sampler struct {
	app         *App
	mu          sync.Mutex
	prevCPU     map[string]cpuTimes
	prevCPUTime time.Time
	prevNet     map[string]netCounters
	prevNetTime time.Time
}

func newSampler(a *App) *sampler { return &sampler{app: a} }

func (a *App) setupTimezone() {
	var candidates []string
	if tz := cleanProp(os.Getenv("TZ")); tz != "" {
		candidates = append(candidates, tz)
	}
	if tz := cleanProp(rd("/data/property/persist.sys.timezone")); tz != "" {
		candidates = append(candidates, tz)
	}
	if out, err := shOut(5*time.Second, binPath("getprop"), "persist.sys.timezone"); err == nil {
		if tz := cleanProp(out); tz != "" {
			candidates = append(candidates, tz)
		}
	}
	for _, tz := range candidates {
		if loc, err := time.LoadLocation(tz); err == nil {
			time.Local = loc
			return
		}
	}
}

// cleanProp trims the NUL/whitespace padding Android property files use.
func cleanProp(s string) string {
	return strings.Trim(s, "\x00 	\r\n")
}

// ---------- collectors ----------

func (a *App) props() (map[string]string, error) {
	ttl := a.ttl(a.config().GetpropTTL, 10*time.Minute)
	return cacheGet(a.cache, "getprop", ttl, func() (map[string]string, error) {
		out, err := shOut(15*time.Second, binPath("getprop"))
		if err != nil {
			return nil, fmt.Errorf("getprop failed: %w", err)
		}
		m := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "[") {
				continue
			}
			i := strings.Index(line, "]: [")
			if i < 0 {
				continue
			}
			k := line[1:i]
			// The line is "[key]: [value]" — skip the 4 chars of "]: [".
			// (Skipping 5 silently eats the first character of every value.)
			v := strings.TrimSuffix(line[i+4:], "]")
			m[k] = v
		}
		if len(m) == 0 {
			return nil, fmt.Errorf("getprop returned no parsable props")
		}
		return m, nil
	})
}

func (a *App) deviceID() string {
	if v := strings.TrimSpace(a.config().DeviceID); v != "" {
		return v
	}
	p, err := a.props()
	if err != nil {
		return "unknown-device"
	}
	model := p["ro.product.model"]
	if model == "" {
		model = p["ro.product.device"]
	}
	sn := p["ro.serialno"]
	if sn == "" {
		sn = p["ro.boot.serialno"]
	}
	if sn == "" {
		if model == "" {
			return "unknown-device"
		}
		return model
	}
	if len(sn) > 8 {
		sn = sn[:8]
	}
	if model == "" {
		return sn
	}
	return model + "-" + sn
}

func (a *App) collectDevice() Device {
	p, err := a.props()
	if err != nil {
		p = map[string]string{}
	}
	atoi := func(s string) int { v, _ := strconv.Atoi(s); return v }
	avb := p["ro.boot.flash.locked"] == "0" || p["ro.boot.verifiedbootstate"] == "orange"
	kernel := rd("/proc/version")
	if kernel == "" {
		kernel, _ = shOut(5*time.Second, binPath("uname"), "-a")
		kernel = strings.TrimSpace(kernel)
	}
	host, _ := os.Hostname()
	return Device{
		Manufacturer:       p["ro.product.manufacturer"],
		Brand:              p["ro.product.brand"],
		Model:              p["ro.product.model"],
		MarketName:         p["ro.product.marketname"],
		Device:             p["ro.product.device"],
		Product:            p["ro.product.name"],
		Board:              p["ro.product.board"],
		SoC:                p["ro.board.platform"],
		SoCModel:           p["ro.soc.model"],
		AndroidRelease:     p["ro.build.version.release"],
		AndroidSDK:         atoi(p["ro.build.version.sdk"]),
		BuildID:            p["ro.build.display.id"],
		BuildFlavor:        p["ro.build.flavor"],
		BuildType:          p["ro.build.type"],
		BuildDate:          p["ro.build.date"],
		BuildFingerprint:   p["ro.build.fingerprint"],
		SecurityPatch:      p["ro.build.version.security_patch"],
		ROMSystem:          p["ro.mi.os.version.name"],
		ROMIncremental:     p["ro.mi.os.version.incremental"],
		ROMDisplayID:       p["ro.build.version.incremental"],
		Arch:               p["ro.product.cpu.abi"],
		ArchList32:         p["ro.product.cpu.abilist32"],
		Kernel:             kernel,
		Baseband:           p["gsm.version.baseband"],
		Serial:             p["ro.serialno"],
		Bootloader:         p["ro.bootloader"],
		SlotSuffix:         p["ro.boot.slot_suffix"],
		BootloaderUnlocked: avb,
		Hostname:           host,
		DeviceID:           a.deviceID(),
		// kver is folded into Kernel; kept separate for readability in the API.
	}
}

func collectBattery() Battery {
	b := Battery{}
	if rd("/sys/class/power_supply/battery/present") == "1" || rd("/sys/class/power_supply/battery/type") != "" {
		b.Present = true
	}
	b.Status = rd("/sys/class/power_supply/battery/status")
	b.Health = rd("/sys/class/power_supply/battery/health")
	b.Technology = rd("/sys/class/power_supply/battery/technology")
	b.ModelName = rd("/sys/class/power_supply/battery/model_name")
	b.Charging = b.Status == "Charging" || b.Status == "Full"
	if v, ok := rdInt("/sys/class/power_supply/battery/capacity"); ok {
		b.CapacityPercent = v
	}
	if v, ok := rdFloat("/sys/class/power_supply/battery/temp"); ok {
		b.TempC = v / 10
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/voltage_now"); ok {
		b.VoltageV = round2(float64(v) / 1e6)
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/voltage_ocv"); ok {
		b.VoltageOCPV = round2(float64(v) / 1e6)
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/current_now"); ok {
		b.CurrentNowUA = v
		b.CurrentNowA = round2(float64(v) / 1e6)
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/charge_counter"); ok {
		b.ChargeCounterMAh = round2(float64(v) / 1000)
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/charge_full"); ok {
		b.ChargeFullMAh = round2(float64(v) / 1000)
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/charge_full_design"); ok {
		b.ChargeFullDesignMAh = round2(float64(v) / 1000)
		if b.ChargeFullMAh > 0 {
			b.HealthPercent = round2(b.ChargeFullMAh / b.ChargeFullDesignMAh * 100)
		}
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/cycle_count"); ok {
		b.CycleCount = v
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/time_to_full_now"); ok {
		b.TimeToFullSeconds = v
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/time_to_empty_now"); ok {
		b.TimeToEmptySeconds = v
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/charge_control_limit"); ok {
		b.ChargeControlLimit = v
	}
	if v, ok := rdInt("/sys/class/power_supply/battery/constant_charge_current"); ok {
		b.ConstantChargeCurrentUA = v
	}
	return b
}

func collectPowerSupply(dir string) PowerSupply {
	ps := PowerSupply{Name: filepath.Base(dir)}
	ps.Type = rd(dir + "/type")
	ps.RealType = rd(dir + "/real_type")
	ps.Status = rd(dir + "/status")
	ps.Health = rd(dir + "/health")
	ps.Technology = rd(dir + "/technology")
	ps.ChargeType = rd(dir + "/charge_type")
	if s := rd(dir + "/online"); s != "" {
		v := s == "1"
		ps.Online = &v
	}
	ps.Present = rd(dir+"/present") == "1"
	if v, ok := rdInt(dir + "/capacity"); ok {
		ps.CapacityPercent = v
	}
	if v, ok := rdInt(dir + "/voltage_now"); ok {
		ps.VoltageNowRaw, ps.VoltageNowV = v, round2(float64(v)/1e6)
	}
	if v, ok := rdInt(dir + "/voltage_max"); ok {
		ps.VoltageMaxRaw, ps.VoltageMaxV = v, round2(float64(v)/1e6)
	}
	if v, ok := rdInt(dir + "/current_now"); ok {
		ps.CurrentNowRaw, ps.CurrentNowA = v, round2(float64(v)/1e6)
	}
	if v, ok := rdInt(dir + "/current_max"); ok {
		ps.CurrentMaxRaw, ps.CurrentMaxA = v, round2(float64(v)/1e6)
	}
	if v, ok := rdInt(dir + "/input_current_limit"); ok {
		ps.InputCurrentLimitRaw, ps.InputCurrentLimitA = v, round2(float64(v)/1e6)
	}
	if v, ok := rdFloat(dir + "/temp"); ok {
		ps.TempC = v / 10
	}
	return ps
}

// wakefulness is derived from dumpsys, cached: it is the expensive one.
func (a *App) wakefulness() (string, bool, error) {
	ttl := a.ttl(a.config().PowerTTL, 10*time.Second)
	type wake struct {
		state string
		on    bool
	}
	w, err := cacheGet(a.cache, "wakefulness", ttl, func() (wake, error) {
		out, err := shOut(15*time.Second, binPath("dumpsys"), "power")
		if err != nil {
			return wake{}, err
		}
		state := ""
		if m := regexp.MustCompile(`mWakefulness=(\w+)`).FindStringSubmatch(out); m != nil {
			state = m[1]
		}
		on := state == "Awake"
		if m := regexp.MustCompile(`Display Power: state=(\w+)`).FindStringSubmatch(out); m != nil {
			on = m[1] == "ON"
		}
		if state == "" {
			return wake{}, fmt.Errorf("mWakefulness not found in dumpsys power")
		}
		return wake{state: state, on: on}, nil
	})
	if err != nil {
		return "", false, err
	}
	return w.state, w.on, nil
}

func (a *App) collectPower() Power {
	p := Power{}
	if st, on, err := a.wakefulness(); err != nil {
		p.Error = err.Error()
	} else {
		p.Wakefulness = st
		p.ScreenOn = on
	}
	dirs, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, d := range dirs {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			continue
		}
		ps := collectPowerSupply(d)
		p.Supplies = append(p.Supplies, ps)
	}
	p.UnitsNote = "*_raw are the sysfs integers verbatim; *_v and *_a convert assuming the kernel power_supply convention (microvolt / microamp). Vendor drivers can deviate — the \"wireless\" node on this device reports millivolts."
	sort.Slice(p.Supplies, func(i, j int) bool { return p.Supplies[i].Name < p.Supplies[j].Name })
	for i := range p.Supplies {
		switch p.Supplies[i].Name {
		case "usb":
			cp := p.Supplies[i]
			p.USB = &cp
		case "wireless":
			cp := p.Supplies[i]
			p.Wireless = &cp
		}
	}
	return p
}

func (a *App) collectDisplay() Display {
	d := Display{}
	if st, on, err := a.wakefulness(); err == nil {
		d.Wakefulness = st
		d.ScreenOn = on
	}
	for _, pat := range []string{"/sys/class/backlight/*/brightness", "/sys/class/leds/lcd-backlight/brightness"} {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			if v, ok := rdInt(f); ok {
				d.Brightness = v
				d.BrightnessSource = f
				if mx, ok := rdInt(filepath.Dir(f) + "/max_brightness"); ok {
					d.BrightnessMax = mx
				}
				break
			}
		}
		if d.BrightnessSource != "" {
			break
		}
	}
	if m := rd("/sys/class/graphics/fb0/modes"); m != "" {
		d.RefreshHint = m
	}
	return d
}

func readCPUTimes() (map[string]cpuTimes, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	out := map[string]cpuTimes{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		var vals []uint64
		for _, s := range f[1:] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				v = 0
			}
			vals = append(vals, v)
		}
		ct := cpuTimes{}
		for i, v := range vals {
			ct.total += v
			if i == 3 || i == 4 { // idle + iowait
				ct.idle += v
			}
		}
		out[f[0]] = ct
	}
	return out, nil
}

func (s *sampler) cpu() CPUInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	ci := CPUInfo{}
	// arm64 Android usually has no "Hardware :" line in /proc/cpuinfo, so
	// prefer the SoC properties and only fall back to cpuinfo.
	if p, err := s.app.props(); err == nil {
		switch man, model := p["ro.soc.manufacturer"], p["ro.soc.model"]; {
		case man != "" && model != "":
			ci.Model = man + " " + model
		case model != "":
			ci.Model = model
		default:
			ci.Model = p["ro.board.platform"]
		}
	}
	if ci.Model == "" {
		if m := regexp.MustCompile(`(?m)^Hardware\s*:\s*(.+)$`).FindStringSubmatch(rd("/proc/cpuinfo")); m != nil {
			ci.Model = strings.TrimSpace(m[1])
		}
	}
	if up := rd("/proc/uptime"); up != "" {
		if f := strings.Fields(up); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				ci.UptimeSeconds = round2(v)
			}
		}
	}
	ci.LoadRaw = rd("/proc/loadavg")
	if f := strings.Fields(ci.LoadRaw); len(f) >= 3 {
		for _, x := range f[:3] {
			if v, err := strconv.ParseFloat(x, 64); err == nil {
				ci.LoadAvg = append(ci.LoadAvg, v)
			}
		}
	}

	// per-core frequencies
	coreDirs, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*")
	sort.Slice(coreDirs, func(i, j int) bool {
		ai, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(coreDirs[i]), "cpu"))
		aj, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(coreDirs[j]), "cpu"))
		return ai < aj
	})
	for _, d := range coreDirs {
		id, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "cpu"))
		if err != nil {
			continue
		}
		c := CPUCore{ID: id}
		c.CurKHz, _ = rdInt(d + "/cpufreq/scaling_cur_freq")
		c.MinKHz, _ = rdInt(d + "/cpufreq/cpuinfo_min_freq")
		c.MaxKHz, _ = rdInt(d + "/cpufreq/cpuinfo_max_freq")
		c.Governor = rd(d + "/cpufreq/scaling_governor")
		ci.Cores = append(ci.Cores, c)
	}
	ci.CoreCount = len(ci.Cores)

	now := time.Now()
	cur, err := readCPUTimes()
	if err == nil {
		if s.prevCPU != nil {
			gap := now.Sub(s.prevCPUTime)
			ci.SampleIntervalMS = gap.Milliseconds()
			if t, ok := cur["cpu"]; ok {
				if p, ok2 := s.prevCPU["cpu"]; ok2 {
					dt := float64(t.total - p.total)
					di := float64(t.idle - p.idle)
					if dt > 0 {
						ci.UsagePercent = round2((1 - di/dt) * 100)
					}
				}
			}
			for i := 0; i < ci.CoreCount; i++ {
				key := fmt.Sprintf("cpu%d", i)
				t, ok := cur[key]
				p, ok2 := s.prevCPU[key]
				if !ok || !ok2 {
					ci.PerCorePercent = append(ci.PerCorePercent, 0)
					continue
				}
				dt := float64(t.total - p.total)
				di := float64(t.idle - p.idle)
				if dt <= 0 {
					ci.PerCorePercent = append(ci.PerCorePercent, 0)
					continue
				}
				ci.PerCorePercent = append(ci.PerCorePercent, round2((1-di/dt)*100))
			}
		}
		s.prevCPU = cur
		s.prevCPUTime = now
	}
	return ci
}

func collectGPU() GPU {
	const base = "/sys/class/kgsl/kgsl-3d0"
	g := GPU{}
	if v, ok := rdInt(base + "/gpu_busy_percentage"); ok {
		g.BusyPercent = v
	}
	g.CurrentHz, _ = rdInt(base + "/gpuclk")
	g.MaxHz, _ = rdInt(base + "/max_gpuclk")
	if v, ok := rdFloat(base + "/temp"); ok {
		g.TempC = round2(v / 1000) // kgsl reports millidegrees Celsius
	}
	g.MinPowerLevel, _ = rdInt(base + "/min_pwrlevel")
	g.MaxPowerLevel, _ = rdInt(base + "/max_pwrlevel")
	b, err := os.ReadFile(base + "/gpu_available_frequencies")
	if err == nil {
		for _, f := range strings.Fields(string(b)) {
			if v, err := strconv.ParseInt(f, 10, 64); err == nil {
				g.AvailableFreqsHz = append(g.AvailableFreqsHz, v)
			}
		}
	}
	return g
}

func collectMemory() Memory {
	m := Memory{}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return m
	}
	get := func(key string) int64 {
		re := regexp.MustCompile(`(?m)^` + key + `:\s*(\d+)`)
		if mm := re.FindStringSubmatch(string(b)); mm != nil {
			v, _ := strconv.ParseInt(mm[1], 10, 64)
			return v
		}
		return 0
	}
	m.TotalKB = get("MemTotal")
	m.FreeKB = get("MemFree")
	m.AvailableKB = get("MemAvailable")
	m.BuffersKB = get("Buffers")
	m.CachedKB = get("Cached")
	m.ShmemKB = get("Shmem")
	m.DirtyKB = get("Dirty")
	m.SlabKB = get("Slab")
	m.SwapTotalKB = get("SwapTotal")
	m.SwapFreeKB = get("SwapFree")
	if m.TotalKB > 0 {
		used := m.TotalKB - m.AvailableKB
		m.UsedPercent = pct(float64(used), float64(m.TotalKB))
	}
	return m
}

func mountTable() map[string][2]string {
	out := map[string][2]string{}
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		out[f[1]] = [2]string{f[0], f[2]}
	}
	return out
}

func collectStorage(mounts []string) []Storage {
	mt := mountTable()
	var res []Storage
	for _, mnt := range mounts {
		s := Storage{Mount: mnt}
		if pair, ok := mt[mnt]; ok {
			s.Device, s.FSType = pair[0], pair[1]
		} else if best, pair := longestMountPrefix(mt, mnt); best != "" {
			// e.g. /system is not a separate mount point on this device (it is
			// inside the "/" erofs image), so report the fs it lives on.
			s.Device, s.FSType, s.FSFrom = pair[0], pair[1], best
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mnt, &st); err != nil {
			s.Error = err.Error()
			res = append(res, s)
			continue
		}
		bs := uint64(st.Bsize)
		s.TotalBytes = st.Blocks * bs
		s.FreeBytes = st.Bavail * bs
		if s.TotalBytes >= s.FreeBytes {
			s.UsedBytes = s.TotalBytes - s.FreeBytes
		}
		s.UsedPercent = pct(float64(s.UsedBytes), float64(s.TotalBytes))
		s.Writable = syscall.Access(mnt, 2) == nil
		res = append(res, s)
	}
	return res
}

// longestMountPrefix finds the mount point a path lives under, so a path like
// /system (which is not a mount point at all) still reports a filesystem.
func longestMountPrefix(mt map[string][2]string, path string) (string, [2]string) {
	best := ""
	var bestPair [2]string
	for mnt, pair := range mt {
		if path == mnt || strings.HasPrefix(path, strings.TrimSuffix(mnt, "/")+"/") {
			if len(mnt) > len(best) {
				best, bestPair = mnt, pair
			}
		}
	}
	return best, bestPair
}

// Some entries under /sys/class/thermal are NOT temperatures: this platform
// exposes battery-current trip points (pm7550ba-ibat-lvl0/1, in mA) and the
// battery voltage (vbat, in mV) through the same interface. Treating every zone
// as a temperature produced a headline "187 °C" from a 187 mA trip point, so a
// zone is now classified and its raw value kept visible either way.
var nonTemperatureZone = regexp.MustCompile(`(?i)(ibat|vbat|volt|curr|adc|[-_]lvl[0-9])`)

const (
	minPlausibleTempC = -40.0
	maxPlausibleTempC = 150.0
)

func classifyThermalZone(name, typ string, raw int64) ThermalZone {
	z := ThermalZone{Name: name, Type: typ, TempRaw: raw}
	c := float64(raw)
	// Real zones on this platform report millidegrees. Small values are whole
	// degrees (socd reports 58), so never divide anything under 1000.
	if c >= 1000 || c <= -1000 {
		c = c / 1000
	}
	z.TempC = round2(c)
	z.IsTemp = z.TempC >= minPlausibleTempC && z.TempC <= maxPlausibleTempC &&
		!nonTemperatureZone.MatchString(typ)
	if !z.IsTemp {
		z.Note = "not a temperature — current/voltage trip point or out of range"
	}
	return z
}

func collectThermal() Thermal {
	t := Thermal{}
	dirs, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, dir := range dirs {
		name := filepath.Base(dir)
		typ := rd(dir + "/type")
		raw, ok := rdInt(dir + "/temp")
		if !ok {
			continue
		}
		z := classifyThermalZone(name, typ, raw)
		if z.IsTemp {
			t.Zones = append(t.Zones, z)
		} else {
			t.NonTemperatureZones = append(t.NonTemperatureZones, z)
		}
	}
	sort.Slice(t.Zones, func(i, j int) bool { return t.Zones[i].TempC > t.Zones[j].TempC })
	sort.Slice(t.NonTemperatureZones, func(i, j int) bool {
		return t.NonTemperatureZones[i].TempRaw > t.NonTemperatureZones[j].TempRaw
	})
	t.ZoneCount = len(t.Zones) + len(t.NonTemperatureZones)
	t.TemperatureZoneCount = len(t.Zones)
	if len(t.Zones) > 0 {
		t.MaxTempC = t.Zones[0].TempC
		t.HottestZone = t.Zones[0].Type
		if t.HottestZone == "" {
			t.HottestZone = t.Zones[0].Name
		}
	}
	cds, _ := filepath.Glob("/sys/class/thermal/cooling_device*")
	t.CoolingDeviceCount = len(cds)
	return t
}

func readNetDev() (map[string]netCounters, error) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	out := map[string]netCounters{}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 16 {
			continue
		}
		num := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out[name] = netCounters{
			rxBytes: num(0), rxPackets: num(1),
			txBytes: num(8), txPackets: num(9),
		}
	}
	return out, nil
}

type ifaceMeta struct {
	up  bool
	mac string
	mtu int
	v4  []string
	v6  []string
}

func ifaceMetas() map[string]ifaceMeta {
	out := map[string]ifaceMeta{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifs {
		m := ifaceMeta{up: ifi.Flags&net.FlagUp != 0, mac: ifi.HardwareAddr.String(), mtu: ifi.MTU}
		if addrs, err := ifi.Addrs(); err == nil {
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if v4 := ipnet.IP.To4(); v4 != nil {
					m.v4 = append(m.v4, v4.String())
				} else if v6 := ipnet.IP.To16(); v6 != nil {
					m.v6 = append(m.v6, v6.String())
				}
			}
		}
		out[ifi.Name] = m
	}
	return out
}

func (s *sampler) network() Network {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := Network{}
	cur, _ := readNetDev()
	now := time.Now()
	metas := ifaceMetas()
	names := map[string]bool{}
	for name := range cur {
		names[name] = true
	}
	for name := range metas {
		names[name] = true
	}

	var keys []string
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)

	var gap time.Duration
	var havePrev bool
	if s.prevNet != nil {
		havePrev = true
		gap = now.Sub(s.prevNetTime)
		n.SampleIntervalMS = gap.Milliseconds()
	}

	for _, name := range keys {
		iface := NetIface{Name: name}
		if m, ok := metas[name]; ok {
			iface.Up, iface.MAC, iface.MTU = m.up, m.mac, m.mtu
			iface.IPv4, iface.IPv6 = m.v4, m.v6
		}
		if c, ok := cur[name]; ok {
			iface.RxBytes, iface.TxBytes = c.rxBytes, c.txBytes
			iface.RxPackets, iface.TxPackets = c.rxPackets, c.txPackets
			if havePrev && gap > 0 {
				if p, ok := s.prevNet[name]; ok {
					secs := gap.Seconds()
					iface.RxBytesPerSec = round2(float64(c.rxBytes-p.rxBytes) / secs)
					iface.TxBytesPerSec = round2(float64(c.txBytes-p.txBytes) / secs)
					iface.RxPacketsPerSec = round2(float64(c.rxPackets-p.rxPackets) / secs)
					iface.TxPacketsPerSec = round2(float64(c.txPackets-p.txPackets) / secs)
					if iface.RxBytesPerSec < 0 {
						iface.RxBytesPerSec = 0
					}
					if iface.TxBytesPerSec < 0 {
						iface.TxBytesPerSec = 0
					}
				}
			}
		}
		if name != "lo" {
			n.TotalRxBytes += iface.RxBytes
			n.TotalTxBytes += iface.TxBytes
			n.TotalRxPerSec += iface.RxBytesPerSec
			n.TotalTxPerSec += iface.TxBytesPerSec
		}
		n.Interfaces = append(n.Interfaces, iface)
	}
	n.TotalRxPerSec = round2(n.TotalRxPerSec)
	n.TotalTxPerSec = round2(n.TotalTxPerSec)

	// Per-interface errors/drops come from the same table; older kernels expose
	// fewer columns, so this is a separate tolerant pass.
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			i := strings.Index(line, ":")
			if i < 0 {
				continue
			}
			name := strings.TrimSpace(line[:i])
			f := strings.Fields(line[i+1:])
			if len(f) < 16 {
				continue
			}
			for idx := range n.Interfaces {
				if n.Interfaces[idx].Name != name {
					continue
				}
				p := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
				n.Interfaces[idx].RxErrors = p(2)
				n.Interfaces[idx].RxDropped = p(3)
				n.Interfaces[idx].TxErrors = p(10)
				n.Interfaces[idx].TxDropped = p(11)
			}
		}
	}

	if cur != nil {
		s.prevNet = cur
		s.prevNetTime = now
	}
	return n
}

var (
	reWifiEnabled = regexp.MustCompile(`(?m)^Wifi is (enabled|disabled)`)
	reWifiConn    = regexp.MustCompile(`Wifi is connected to "([^"]*)"`)
	reSSID        = regexp.MustCompile(`SSID:\s*(?:"([^"]*)"|(\S+))`)
	reBSSID       = regexp.MustCompile(`BSSID:\s*([0-9a-fA-F:]{17})`)
	reMAC         = regexp.MustCompile(`MAC:\s*([0-9a-fA-F:]{17})`)
	reWifiIP      = regexp.MustCompile(`IP:\s*/?([0-9]{1,3}(?:\.[0-9]{1,3}){3})`)
	reRSSI        = regexp.MustCompile(`RSSI:\s*(-?[0-9]+)`)
	reLinkSpeed   = regexp.MustCompile(`Link speed:\s*([0-9]+)\s*Mbps`)
	reTxLink      = regexp.MustCompile(`Tx Link speed:\s*([0-9]+)\s*Mbps`)
	reRxLink      = regexp.MustCompile(`Rx Link speed:\s*([0-9]+)\s*Mbps`)
	reFreq        = regexp.MustCompile(`Frequency:\s*([0-9]+)\s*MHz`)
	reStd         = regexp.MustCompile(`Wi-Fi standard:\s*([0-9a-zA-Z]+)`)
	reSec         = regexp.MustCompile(`Security type:\s*([0-9]+)`)
	reSupp        = regexp.MustCompile(`Supplicant state:\s*([A-Z_]+)`)
	reMetered     = regexp.MustCompile(`Metered hint:\s*(true|false)`)
	reWifiCount   = regexp.MustCompile(`(?m)^(successfulTxPackets|successfulRxPackets|retriedTxPackets|lostTxPackets|successfulTxPacketsPerSecond|successfulRxPacketsPerSecond|retriedTxPacketsPerSecond|lostTxPacketsPerSecond):\s*([0-9.]+)\s*$`)
)

func (a *App) collectWifi() Wifi {
	ttl := a.ttl(a.config().WifiTTL, 5*time.Second)
	w, err := cacheGet(a.cache, "wifi", ttl, func() (Wifi, error) {
		out, err := shOut(15*time.Second, binPath("cmd"), "wifi", "status")
		if err != nil {
			return Wifi{}, err
		}
		return parseWifiStatus(out), nil
	})
	if err != nil {
		return Wifi{Error: err.Error()}
	}
	return w
}

func parseWifiStatus(out string) Wifi {
	w := Wifi{}
	if m := reWifiEnabled.FindStringSubmatch(out); m != nil {
		w.Enabled = m[1] == "enabled"
	}
	if m := reWifiConn.FindStringSubmatch(out); m != nil {
		w.Connected = true
		w.SSID = m[1]
	}
	if m := reSSID.FindStringSubmatch(out); m != nil && w.SSID == "" {
		if m[1] != "" {
			w.SSID = m[1]
		} else {
			w.SSID = m[2]
		}
		if w.SSID == "<unknown>" || w.SSID == "unknown" {
			w.SSID = ""
		}
	}
	if m := reBSSID.FindStringSubmatch(out); m != nil {
		w.BSSID = m[1]
	}
	if m := reMAC.FindStringSubmatch(out); m != nil {
		w.DeviceMAC = m[1]
	}
	if m := reWifiIP.FindStringSubmatch(out); m != nil {
		w.IPv4 = m[1]
	}
	if m := reRSSI.FindStringSubmatch(out); m != nil {
		w.RSSIDbm, _ = strconv.Atoi(m[1])
		// Standard Android mapping of RSSI to a 0-100 signal bar.
		p := 2 * (w.RSSIDbm + 100)
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		w.SignalPercent = p
	}
	intFrom := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(out); m != nil {
			v, _ := strconv.Atoi(m[1])
			return v
		}
		return 0
	}
	w.LinkSpeedMbps = intFrom(reLinkSpeed)
	w.TxLinkSpeedMbps = intFrom(reTxLink)
	w.RxLinkSpeedMbps = intFrom(reRxLink)
	w.FrequencyMHz = intFrom(reFreq)
	w.SecurityType = intFrom(reSec)
	if m := reStd.FindStringSubmatch(out); m != nil {
		w.Standard = m[1]
	}
	if m := reSupp.FindStringSubmatch(out); m != nil {
		w.SupplicantState = m[1]
	}
	if m := reMetered.FindStringSubmatch(out); m != nil {
		w.Metered = m[1] == "true"
	}
	if w.FrequencyMHz > 0 {
		switch {
		case w.FrequencyMHz < 2500:
			w.Band = "2.4GHz"
			w.Channel = (w.FrequencyMHz - 2407) / 5
		case w.FrequencyMHz < 5900:
			w.Band = "5GHz"
			w.Channel = (w.FrequencyMHz - 5000) / 5
		default:
			w.Band = "6GHz"
			w.Channel = (w.FrequencyMHz - 5950) / 5
		}
	}
	for _, m := range reWifiCount.FindAllStringSubmatch(out, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		switch m[1] {
		case "successfulTxPackets":
			w.TxPackets = int64(v)
		case "successfulRxPackets":
			w.RxPackets = int64(v)
		case "retriedTxPackets":
			w.TxRetries = int64(v)
		case "lostTxPackets":
			w.TxLost = int64(v)
		case "successfulTxPacketsPerSecond":
			w.TxPacketsPerSec = round2(v)
		case "successfulRxPacketsPerSecond":
			w.RxPacketsPerSec = round2(v)
		case "retriedTxPacketsPerSecond":
			w.TxRetriesPerSec = round2(v)
		case "lostTxPacketsPerSecond":
			w.TxLostPerSec = round2(v)
		}
	}
	return w
}

func (a *App) collectSystem() System {
	s := System{PageSize: os.Getpagesize()}
	if up := rd("/proc/uptime"); up != "" {
		if f := strings.Fields(up); len(f) >= 2 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.UptimeSeconds = round2(v)
				s.BootTimeRFC3339 = time.Now().Add(-time.Duration(v * float64(time.Second))).Format(time.RFC3339)
			}
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				s.IdleSeconds = round2(v)
			}
		}
	}
	s.KernelVersion = rd("/proc/sys/kernel/osrelease")
	s.Kernel = rd("/proc/version")
	s.SELinuxMode = "unknown"
	if rd("/sys/fs/selinux/enforce") == "1" {
		s.SELinuxEnforcing = true
		s.SELinuxMode = "Enforcing"
	} else if rd("/sys/fs/selinux/enforce") == "0" {
		s.SELinuxMode = "Permissive"
	} else if rd("/sys/fs/selinux/status") != "" {
		s.SELinuxMode = "disabled"
	}
	s.LoadRaw = rd("/proc/loadavg")
	if f := strings.Fields(s.LoadRaw); len(f) >= 3 {
		for _, x := range f[:3] {
			if v, err := strconv.ParseFloat(x, 64); err == nil {
				s.LoadAvg = append(s.LoadAvg, v)
			}
		}
	}
	s.Hostname, _ = os.Hostname()
	s.Timezone = time.Now().Format("MST")
	s.TimezoneName = time.Local.String()
	_, utcOff := time.Now().Zone()
	s.UTCOffsetSeconds = utcOff
	if p, err := a.props(); err == nil {
		if v, err := strconv.Atoi(p["ro.build.version.sdk"]); err == nil {
			s.AndroidSDK = v
		}
	}
	procs, err := os.ReadDir("/proc")
	if err == nil {
		for _, d := range procs {
			if d.IsDir() && d.Name()[0] >= '0' && d.Name()[0] <= '9' {
				s.ProcessCount++
			}
		}
	}
	return s
}

func (a *App) collectProcesses() Processes {
	ttl := a.ttl(a.config().ProcsTTL, 10*time.Second)
	p, err := cacheGet(a.cache, "procs", ttl, func() (Processes, error) {
		res := Processes{}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return res, err
		}
		page := int64(os.Getpagesize())
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || name[0] < '0' || name[0] > '9' {
				continue
			}
			pid, err := strconv.Atoi(name)
			if err != nil {
				continue
			}
			res.Count++
			statm := rd("/proc/" + name + "/statm")
			if statm == "" {
				continue
			}
			f := strings.Fields(statm)
			if len(f) < 2 {
				continue
			}
			rss, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				continue
			}
			res.TopRSS = append(res.TopRSS, ProcEntry{
				PID:   pid,
				Name:  rd("/proc/" + name + "/comm"),
				RSSKB: rss * page / 1024,
			})
		}
		sort.Slice(res.TopRSS, func(i, j int) bool { return res.TopRSS[i].RSSKB > res.TopRSS[j].RSSKB })
		if len(res.TopRSS) > 15 {
			res.TopRSS = res.TopRSS[:15]
		}
		return res, nil
	})
	if err != nil {
		return Processes{}
	}
	return p
}

func (a *App) collectPackages() Packages {
	ttl := a.ttl(a.config().PackagesTTL, 5*time.Minute)
	p, err := cacheGet(a.cache, "packages", ttl, func() (Packages, error) {
		res := Packages{}
		count := func(args ...string) (int, error) {
			out, err := shOut(30*time.Second, binPath("pm"), append([]string{"list", "packages"}, args...)...)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "package:") {
					n++
				}
			}
			return n, nil
		}
		var firstErr error
		for _, spec := range []struct {
			dst  *int
			args []string
		}{
			{&res.Total, nil},
			{&res.System, []string{"-s"}},
			{&res.User, []string{"-3"}},
			{&res.Disabled, []string{"-d"}},
			{&res.UninstalledForUser, []string{"-u"}},
		} {
			n, err := count(spec.args...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			*spec.dst = n
		}
		if firstErr != nil && res.Total == 0 {
			return res, firstErr
		}
		return res, nil
	})
	if err != nil {
		return Packages{Error: err.Error()}
	}
	return p
}

// snapshot assembles the whole document.
func (a *App) snapshot() Snapshot {
	s := Snapshot{
		CollectedAt:        time.Now().Format(time.RFC3339),
		DeviceID:           a.deviceID(),
		ServerVersion:      version,
		ServerUptimeSecond: round2(time.Since(a.started).Seconds()),
	}
	s.Device = a.collectDevice()
	s.Battery = collectBattery()
	s.Power = a.collectPower()
	s.Display = a.collectDisplay()
	s.CPU = a.sampler.cpu()
	s.GPU = collectGPU()
	s.Memory = collectMemory()
	s.Storage = collectStorage(a.config().Mounts)
	s.Thermal = collectThermal()
	s.Network = a.sampler.network()
	s.Wifi = a.collectWifi()
	s.System = a.collectSystem()
	s.Processes = a.collectProcesses()
	s.Packages = a.collectPackages()
	return s
}
