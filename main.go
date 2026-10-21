// phoneserver — expose Android device telemetry over a RESTful JSON API.
//
// Design notes:
//   - stdlib only, so it cross-compiles offline for linux/arm64.
//   - every collector is best-effort: a missing sysfs node yields a zero value
//     rather than failing the whole request.
//   - expensive sources (getprop, `cmd wifi status`, `dumpsys`) are cached
//     behind a TTL, so a 3-second dashboard poll does not spam the system.
//   - the same "Snapshot" document is what the pusher POSTs to a cloud server.
//   - there is deliberately NO arbitrary-file-read endpoint: this process runs
//     as root and the port is reachable from the network.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // Android has no system zoneinfo; embed the database
)

const (
	version    = "1.3.1"
	defaultCfg = "/data/adb/phoneserver/config.json"
)

// ---------- config ----------

type PushConfig struct {
	Enabled      bool              `json:"enabled"`
	URL          string            `json:"url"`
	Interval     int               `json:"interval_seconds"`
	BearerToken  string            `json:"bearer_token"`
	TimeoutS     int               `json:"timeout_seconds"`
	Retries      int               `json:"retries"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
}

type Config struct {
	Listen      string        `json:"listen"`
	APIToken    string        `json:"api_token"`
	DeviceID    string        `json:"device_id"`
	LogDir      string        `json:"log_dir"`
	LogRequests bool          `json:"log_requests"`
	LogMaxBytes int64         `json:"log_max_bytes"`
	GetpropTTL  int           `json:"getprop_ttl_seconds"`
	WifiTTL     int           `json:"wifi_ttl_seconds"`
	PowerTTL    int           `json:"power_ttl_seconds"`
	PackagesTTL int           `json:"packages_ttl_seconds"`
	ProcsTTL    int           `json:"procs_ttl_seconds"`
	Mounts      []string      `json:"storage_mounts"`
	Push        PushConfig    `json:"push"`
	History     HistoryConfig `json:"history"`
}

func defaultConfig() Config {
	return Config{
		Listen:      "0.0.0.0:8080",
		APIToken:    newToken(),
		DeviceID:    "",
		LogDir:      "/data/adb/phoneserver/logs",
		LogRequests: true,
		LogMaxBytes: 4 << 20,
		GetpropTTL:  600,
		WifiTTL:     5,
		PowerTTL:    10,
		PackagesTTL: 300,
		ProcsTTL:    10,
		Mounts:      []string{"/data", "/", "/system", "/storage/emulated/0"},
		Push: PushConfig{
			Enabled:  false,
			URL:      "",
			Interval: 60,
			TimeoutS: 10,
			Retries:  3,
		},
		History: defaultHistoryConfig(),
	}
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "changeme"
	}
	return hex.EncodeToString(b)
}

func loadConfig(path string, warnf func(string, ...any)) (Config, bool, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return cfg, false, err
		}
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
			return cfg, false, mkErr
		}
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if wErr := os.WriteFile(path, out, 0o600); wErr != nil {
			return cfg, false, wErr
		}
		return cfg, true, nil
	}
	// Start from defaults so that a partial/older config keeps sane values.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return defaultConfig(), false, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:8080"
	}
	if cfg.LogMaxBytes <= 0 {
		cfg.LogMaxBytes = 4 << 20
	}
	if len(cfg.Mounts) == 0 {
		cfg.Mounts = defaultConfig().Mounts
	}
	if warnf != nil && cfg.APIToken == "" {
		warnf("api_token is empty — the API is reachable by anyone who can route to this port")
	}
	return cfg, false, nil
}

func (c Config) save(path string) error {
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- rotating log writer ----------

type rotatingWriter struct {
	path string
	max  int64
	keep int
	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingWriter(path string, max int64, keep int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &rotatingWriter{path: path, max: max, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil {
		w.size = st.Size()
	}
	w.f = f
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, io.ErrClosedPipe
	}
	if w.size+int64(len(p)) > w.max {
		w.f.Close()
		for i := w.keep - 1; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
		}
		os.Rename(w.path, w.path+".1")
		w.size = 0
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ---------- app ----------

type App struct {
	cfgPath string
	mu      sync.RWMutex
	cfg     Config
	cache   *ttlCache
	sampler *sampler
	// tickSampler belongs to the history loop: separate delta state, so the
	// 10-second sampling cadence cannot distort the rates the API reports.
	tickSampler *sampler
	pusher      *pusher
	store       *Store
	histStop    chan struct{}
	histTicks   atomic.Int64
	histFulls   atomic.Int64
	histErrf    atomic.Value // string
	// Loop diagnostics. "The loop is slower than the configured interval" is
	// otherwise unanswerable from outside: these split one cycle into the
	// bounded wait, the collection and the database write.
	histLastCollect atomic.Int64
	histLastWrite   atomic.Int64
	histLastGap     atomic.Int64
	histSumCollect  atomic.Int64
	histSumWrite    atomic.Int64
	histSumGap      atomic.Int64
	histDiagN       atomic.Int64
	// shotMu serialises screenshot captures; a second request is told to retry
	// rather than queueing another screencap behind the first.
	shotMu  sync.Mutex
	started time.Time
	logf    *log.Logger
}

func (a *App) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *App) setConfig(c Config) {
	a.mu.Lock()
	a.cfg = c
	a.mu.Unlock()
}

func (a *App) ttl(secs int, fallback time.Duration) time.Duration {
	if secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

// ---------- HTTP plumbing ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, `{"error":"marshal failed"}`, http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Phoneserver-Version", version)
	w.WriteHeader(code)
	w.Write(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg, "status": code, "server_version": version})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func (a *App) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		if r.Method == http.MethodOptions {
			rec.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(rec, r)
		if a.config().LogRequests {
			a.logf.Printf("access %s %s -> %d %dB %s %s",
				r.Method, r.URL.Path, rec.code, r.ContentLength,
				time.Since(start).Round(time.Millisecond), r.RemoteAddr)
		}
	})
}

// tokenOK implements the auth check: X-API-Key header, Authorization: Bearer,
// or ?key= query parameter. When api_token is empty, auth is disabled.
func (a *App) tokenOK(r *http.Request) bool {
	want := a.config().APIToken
	if want == "" {
		return true
	}
	got := r.Header.Get("X-API-Key")
	if got == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
	}
	if got == "" {
		got = r.URL.Query().Get("key")
	}
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.tokenOK(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="phoneserver"`)
			writeErr(w, http.StatusUnauthorized, "missing or invalid api token (send X-API-Key, Authorization: Bearer, or ?key=)")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h(w, r)
	}
}

// get makes an endpoint read-only: any method other than GET gets a 405 with an
// Allow header, instead of quietly returning the resource body for PUT/POST too.
func (a *App) get(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed; this endpoint is read-only")
			return
		}
		a.guard(h)(w, r)
	}
}

func (a *App) dispatch(table map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := table[r.Method]; ok {
			h(w, r)
			return
		}
		allow := make([]string, 0, len(table))
		for m := range table {
			allow = append(allow, m)
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed; allowed: "+strings.Join(allow, ", "))
	}
}

// ---------- handlers ----------

func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeErr(w, http.StatusNotFound, "no such path; see GET /api/v1/endpoints")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, dashboardHTML)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	h := cfg.History.norm()
	out := map[string]any{
		"status":          "ok",
		"server_version":  version,
		"server_uptime_s": round2(time.Since(a.started).Seconds()),
		"auth_required":   cfg.APIToken != "",
		"push_enabled":    cfg.Push.Enabled && cfg.Push.URL != "",
		"history_enabled": a.store != nil && h.Enabled,
	}
	if a.store != nil && h.Enabled {
		out["history_ticks"] = a.histTicks.Load()
		out["history_interval_seconds"] = h.IntervalSeconds
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server_version": version,
		"auth":           "X-API-Key / Authorization: Bearer / ?key=",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/api/v1/health", "desc": "liveness + server state (no auth needed)"},
			{"method": "GET", "path": "/api/v1/endpoints", "desc": "this index"},
			{"method": "GET", "path": "/api/v1/all", "desc": "every section in one document"},
			{"method": "GET", "path": "/api/v1/device", "desc": "identity, ROM, kernel, build"},
			{"method": "GET", "path": "/api/v1/props", "desc": "full getprop dump (1000+ keys)"},
			{"method": "GET", "path": "/api/v1/battery", "desc": "charge, health, cycles, currents, temps"},
			{"method": "GET", "path": "/api/v1/power", "desc": "charging inputs (usb/wireless) + power supplies"},
			{"method": "GET", "path": "/api/v1/display", "desc": "screen state, brightness"},
			{"method": "GET", "path": "/api/v1/cpu", "desc": "per-core freq, usage, governor, loadavg"},
			{"method": "GET", "path": "/api/v1/gpu", "desc": "Adreno busy %, clock, temp"},
			{"method": "GET", "path": "/api/v1/memory", "desc": "/proc/meminfo"},
			{"method": "GET", "path": "/api/v1/storage", "desc": "statvfs per configured mount"},
			{"method": "GET", "path": "/api/v1/thermal", "desc": "all thermal zones in Celsius"},
			{"method": "GET", "path": "/api/v1/network", "desc": "interfaces, counters, live rates"},
			{"method": "GET", "path": "/api/v1/network/wifi", "desc": "SSID, RSSI, link speed, freq, packet stats"},
			{"method": "GET", "path": "/api/v1/screenshot", "desc": "current screen as webp (default), jpeg or png; ?quality= ?scale= ?display= ?meta=1 ?base64=1 ?download=1; never stored, never pushed"},
			{"method": "GET", "path": "/api/v1/system", "desc": "uptime, kernel, selinux, loadavg"},
			{"method": "GET", "path": "/api/v1/processes", "desc": "process count + top by RSS"},
			{"method": "GET", "path": "/api/v1/packages", "desc": "installed package counts"},
			{"method": "GET", "path": "/api/v1/config", "desc": "current config (token redacted)"},
			{"method": "PUT", "path": "/api/v1/config", "desc": "update push settings / token / ttl"},
			{"method": "GET", "path": "/api/v1/push", "desc": "last push result + queue state"},
			{"method": "POST", "path": "/api/v1/push", "desc": "push the current snapshot to the configured URL now"},
			{"method": "GET", "path": "/api/v1/history", "desc": "query stored samples (metrics, since/until, limit)"},
			{"method": "GET", "path": "/api/v1/history/metrics", "desc": "list the queryable metric columns"},
			{"method": "GET", "path": "/api/v1/history/logs", "desc": "query stored log lines (grep/level/since)"},
			{"method": "GET", "path": "/api/v1/history/snapshot", "desc": "the full stored document at/near a timestamp"},
			{"method": "GET", "path": "/api/v1/history/stats", "desc": "db size, row counts, growth rate, cost estimate"},
			{"method": "POST", "path": "/api/v1/history/prune", "desc": "apply the retention policy now"},
		},
	})
}

func (a *App) handleAll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.snapshot())
}

func (a *App) handleDevice(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectDevice())
}

func (a *App) handleProps(w http.ResponseWriter, r *http.Request) {
	p, err := a.props()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "getprop failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(p), "props": p})
}

func (a *App) handleBattery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectBattery())
}
func (a *App) handlePower(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectPower())
}
func (a *App) handleDisplay(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectDisplay())
}
func (a *App) handleCPU(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.sampler.cpu())
}
func (a *App) handleGPU(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectGPU())
}
func (a *App) handleMemory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectMemory())
}
func (a *App) handleStorage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectStorage(a.config().Mounts))
}
func (a *App) handleThermal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, collectThermal())
}
func (a *App) handleNetwork(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.sampler.network())
}
func (a *App) handleWifi(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectWifi())
}
func (a *App) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectSystem())
}
func (a *App) handleProcesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectProcesses())
}
func (a *App) handlePackages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.collectPackages())
}

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	c := a.config()
	out := c
	out.APIToken = redact(c.APIToken)
	out.Push.BearerToken = redact(c.Push.BearerToken)
	writeJSON(w, http.StatusOK, map[string]any{"config_path": a.cfgPath, "config": out})
}

func (a *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	cur := a.config()
	next := cur
	if err := json.Unmarshal(body, &next); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Refuse to let a redacted placeholder overwrite a real secret.
	if strings.Contains(next.APIToken, "…") || strings.Contains(next.APIToken, "...") {
		next.APIToken = cur.APIToken
	}
	if strings.Contains(next.Push.BearerToken, "…") || strings.Contains(next.Push.BearerToken, "...") {
		next.Push.BearerToken = cur.Push.BearerToken
	}
	if next.Push.Enabled && next.Push.URL != "" {
		if !strings.HasPrefix(next.Push.URL, "http://") && !strings.HasPrefix(next.Push.URL, "https://") {
			writeErr(w, http.StatusBadRequest, "push.url must start with http:// or https://")
			return
		}
	}
	if next.Push.Interval < 10 {
		next.Push.Interval = 10
	}
	next.History = next.History.norm()
	if next.Listen == "" {
		next.Listen = cur.Listen
	}
	if err := next.save(a.cfgPath); err != nil {
		writeErr(w, http.StatusInternalServerError, "save config: "+err.Error())
		return
	}
	prev := a.config()
	a.setConfig(next)
	out := next
	out.APIToken = redact(next.APIToken)
	out.Push.BearerToken = redact(next.Push.BearerToken)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"config":                out,
		"listen_changed":        prev.Listen != next.Listen,
		"note":                  "listen/port changes need a restart of the service (module action button or reboot)",
		"config_path":           a.cfgPath,
		"push_settings_applied": true,
	})
}

func (a *App) handlePushStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.pusher.status())
}

func (a *App) handlePushNow(w http.ResponseWriter, r *http.Request) {
	res := a.pusher.pushOnce("manual")
	if res.OK {
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusBadGateway, res)
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "…"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// ---------- main ----------

func main() {
	var (
		cfgPath = flag.String("config", defaultCfg, "path to config.json")
		listen  = flag.String("listen", "", "override listen address, e.g. 0.0.0.0:8080")
		once    = flag.Bool("once", false, "print one snapshot as JSON to stdout and exit (no server)")
		ver     = flag.Bool("version", false, "print version and exit")
		initCfg = flag.Bool("init-config", false, "create config.json if missing, print its path, and exit")
	)
	flag.Parse()

	if *ver {
		fmt.Printf("phoneserver %s (built with %s)\n", version, runtime.Version())
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, created, err := loadConfig(*cfgPath, log.Printf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		// A broken config should not brick the boot loop; fall back to defaults.
		cfg = defaultConfig()
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *initCfg {
		// service.sh calls this at first boot so the API token exists before the
		// server starts; loadConfig already wrote the file if it was missing.
		fmt.Println(*cfgPath)
		return
	}

	var base io.Writer = os.Stdout
	if rw, rerr := newRotatingWriter(filepath.Join(cfg.LogDir, "phoneserver.log"), cfg.LogMaxBytes, 3); rerr == nil {
		base = io.MultiWriter(os.Stdout, rw)
	} else {
		fmt.Fprintf(os.Stderr, "log file unavailable (%v), logging to stdout only\n", rerr)
	}

	app := &App{
		cfgPath: *cfgPath,
		cfg:     cfg,
		cache:   newTTLCache(),
		started: time.Now(),
	}
	app.sampler = newSampler(app)

	// The history database is opened before the logger is built so its sink can
	// be part of the writer: every log line from here on is mirrored into the
	// database as well as the file. A failure only disables local history.
	logw := base
	if !*once {
		var herr error
		logw, herr = app.openHistory(cfg, base, os.Stderr)
		if herr != nil {
			fmt.Fprintf(os.Stderr, "phoneserver: %v\n", herr)
		}
	}
	logger := log.New(logw, "", log.LstdFlags|log.Lmicroseconds)
	app.logf = logger
	app.pusher = newPusher(app, logger)
	app.setupTimezone()

	if app.store != nil {
		app.store.StartLogFlusher(logger.Printf)
		app.backfillExistingLogs(cfg, logger)
	}

	if *once {
		out, merr := json.MarshalIndent(app.snapshot(), "", "  ")
		if merr != nil {
			fmt.Fprintf(os.Stderr, "marshal: %v\n", merr)
			os.Exit(1)
		}
		fmt.Println(string(out))
		return
	}

	logger.Printf("phoneserver %s starting: listen=%s config=%s (created=%v) push=%v device_id=%q",
		version, cfg.Listen, *cfgPath, created, cfg.Push.Enabled && cfg.Push.URL != "", app.deviceID())
	if created {
		logger.Printf("generated api token; read it with: grep api_token %s", *cfgPath)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleRoot)
	mux.HandleFunc("/api/v1/health", app.handleHealth)
	mux.HandleFunc("/api/v1/endpoints", app.get(app.handleEndpoints))
	mux.HandleFunc("/api/v1/all", app.get(app.handleAll))
	mux.HandleFunc("/api/v1/device", app.get(app.handleDevice))
	mux.HandleFunc("/api/v1/props", app.get(app.handleProps))
	mux.HandleFunc("/api/v1/battery", app.get(app.handleBattery))
	mux.HandleFunc("/api/v1/power", app.get(app.handlePower))
	mux.HandleFunc("/api/v1/display", app.get(app.handleDisplay))
	mux.HandleFunc("/api/v1/cpu", app.get(app.handleCPU))
	mux.HandleFunc("/api/v1/gpu", app.get(app.handleGPU))
	mux.HandleFunc("/api/v1/memory", app.get(app.handleMemory))
	mux.HandleFunc("/api/v1/storage", app.get(app.handleStorage))
	mux.HandleFunc("/api/v1/thermal", app.get(app.handleThermal))
	mux.HandleFunc("/api/v1/network", app.get(app.handleNetwork))
	mux.HandleFunc("/api/v1/network/wifi", app.get(app.handleWifi))
	mux.HandleFunc("/api/v1/system", app.get(app.handleSystem))
	mux.HandleFunc("/api/v1/processes", app.get(app.handleProcesses))
	mux.HandleFunc("/api/v1/packages", app.get(app.handlePackages))
	mux.HandleFunc("/api/v1/screenshot", app.get(app.handleScreenshot))
	mux.HandleFunc("/api/v1/config", app.guard(app.dispatch(map[string]http.HandlerFunc{
		http.MethodGet: app.handleGetConfig,
		http.MethodPut: app.handlePutConfig,
	})))
	mux.HandleFunc("/api/v1/push", app.guard(app.dispatch(map[string]http.HandlerFunc{
		http.MethodGet:  app.handlePushStatus,
		http.MethodPost: app.handlePushNow,
	})))
	mux.HandleFunc("/api/v1/history", app.get(app.handleHistory))
	mux.HandleFunc("/api/v1/history/metrics", app.get(app.handleHistoryMetrics))
	mux.HandleFunc("/api/v1/history/logs", app.get(app.handleHistoryLogs))
	mux.HandleFunc("/api/v1/history/snapshot", app.get(app.handleHistorySnapshot))
	mux.HandleFunc("/api/v1/history/stats", app.get(app.handleHistoryStats))
	mux.HandleFunc("/api/v1/history/prune", app.guard(app.dispatch(map[string]http.HandlerFunc{
		http.MethodPost: app.handleHistoryPrune,
	})))

	if app.store != nil {
		app.histStop = make(chan struct{})
		go app.startHistoryLoop(logger, app.histStop)
		h := cfg.History.norm()
		logger.Printf("history: db=%s interval=%ds full=%ds retention=%dd(samples)/%dd(snapshots)/%dd(logs) cap=%dMB",
			h.DBPath, h.IntervalSeconds, h.FullSeconds,
			h.RetentionDays, h.SnapshotRetentionDays, h.LogRetentionDays, h.MaxDBMB)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          logger,
	}

	go app.pusher.loop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errCh:
		logger.Printf("FATAL listen on %s failed: %v", cfg.Listen, err)
		os.Exit(1)
	case sig := <-sigCh:
		logger.Printf("received %s, shutting down", sig)
		if app.histStop != nil {
			close(app.histStop)
		}
		if app.store != nil {
			// Give the batched log writer a moment to drain before closing.
			time.Sleep(150 * time.Millisecond)
			app.store.Close()
		}
		srv.Close()
	}
}
