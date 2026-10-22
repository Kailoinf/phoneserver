// phoneserver — a self-hosted control and telemetry server for one Android phone.
//
// Design notes:
//   - stdlib only, so it cross-compiles offline for linux/arm64.
//   - every collector is best-effort: a missing sysfs node yields a zero value
//     rather than failing the whole request.
//   - expensive sources (getprop, `cmd wifi status`, `dumpsys`) are cached
//     behind a TTL, so a 3-second dashboard poll does not spam the system.
//   - there is deliberately NO remote/cloud upload of anything. Captures are
//     answered to the caller and never leave the device on their own.
//   - the listener defaults to loopback. Reaching it from another machine is an
//     explicit opt-in (security.allow_non_loopback_listen).
//   - there is no arbitrary-file-read endpoint. A shell endpoint exists but is
//     off by default and gated behind config.
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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // Android has no system zoneinfo; embed the database
)

const (
	version    = "2.0.0"
	defaultCfg = "/data/adb/phoneserver/config.json"
)

// ---------- config ----------

type SecurityConfig struct {
	// AllowNonLoopbackListen has to be true before `listen` may point anywhere
	// other than 127.0.0.1/localhost/::1. It is the single switch that turns
	// this phone service into something the LAN can reach.
	AllowNonLoopbackListen bool `json:"allow_non_loopback_listen"`
	AllowShell             bool `json:"allow_shell"`
	AllowPowerCommands     bool `json:"allow_power_commands"`
	ShellTimeoutMS         int  `json:"shell_timeout_ms"`
	ShellMaxOutputBytes    int  `json:"shell_max_output_bytes"`
}

type ControlConfig struct {
	Enabled          bool `json:"enabled"`
	DefaultTimeoutMS int  `json:"default_timeout_ms"`
	MaxTimeoutMS     int  `json:"max_timeout_ms"`
}

type CaptureConfig struct {
	CameraEnabled  bool   `json:"camera_enabled"`
	MicEnabled     bool   `json:"mic_enabled"`
	DefaultCamera  int    `json:"default_camera"`
	PhotoFormat    string `json:"photo_format"` // webp | jpeg | png | raw
	PhotoQuality   int    `json:"photo_quality"`
	PhotoMaxWidth  int    `json:"photo_max_width"` // 0 = keep the sensor size
	PreviewFPS     int    `json:"preview_fps"`
	PreviewMaxWide int    `json:"preview_max_width"`
	PreviewQuality int    `json:"preview_quality"`
	MicSeconds     int    `json:"mic_default_seconds"`
	MicMaxSeconds  int    `json:"mic_max_seconds"`
	MicEncoder     string `json:"mic_encoder"` // aac | opus | amr_wb | amr_nb
	MicBitrateKbps int    `json:"mic_bitrate_kbps"`
	MicRateHz      int    `json:"mic_rate_hz"`
	MicChannels    int    `json:"mic_channels"`
	KeepFiles      bool   `json:"keep_capture_files"`
}

type StreamConfig struct {
	Enabled        bool   `json:"enabled"`
	MaxClients     int    `json:"max_clients"`
	DefaultSize    string `json:"default_size"`
	DefaultBitrate int    `json:"default_bitrate"`
	// DefaultFPS is a hint used to pick a sensible bitrate; screenrecord encodes
	// at the display refresh rate and this server does not drop frames.
	DefaultFPS    int  `json:"default_fps"`
	IdleTimeoutS  int  `json:"idle_timeout_seconds"`
	WakeDisplay   bool `json:"wake_display"`
	KeepDisplayOn bool `json:"keep_display_on"`
	MjpegFPS      int  `json:"mjpeg_fps"`
	MjpegMaxWidth int  `json:"mjpeg_max_width"`
	MjpegQuality  int  `json:"mjpeg_quality"`
}

type NotifyConfig struct {
	Enabled        bool `json:"enabled"`
	IncludeOngoing bool `json:"include_ongoing"`
	Max            int  `json:"max"`
	CacheSeconds   int  `json:"cache_seconds"`
}

// FilesConfig turns the phone into a file server: a plain HTTP browse/upload
// surface at /files/ and a WebDAV share at /dav/ (mountable as a network drive).
//
// Shares are a *whitelist*: the URL carries a share name, never a raw
// filesystem path, so there is no directory-traversal surface at all. Writes
// are separately switchable (read_only for the whole service, allow_delete for
// deletion specifically) and uploads are size-capped.
type FilesConfig struct {
	Enabled        bool              `json:"enabled"`
	Shares         map[string]string `json:"shares"` // URL share name -> absolute directory
	ReadOnly       bool              `json:"read_only"`
	AllowDelete    bool              `json:"allow_delete"`
	MaxUploadMB    int               `json:"max_upload_mb"`
	ShowHidden     bool              `json:"show_hidden"`
	HTTP           bool              `json:"http_files"`
	WebDAV         bool              `json:"webdav"`
	Listing        bool              `json:"listing"` // HTML directory listing on GET
	MaxListEntries int               `json:"max_list_entries"`
}

type MCPConfig struct {
	// HTTP exposes the MCP endpoint next to the REST API (POST /mcp).
	// stdio mode (`phoneserver -mcp`) works regardless of this flag.
	HTTP     bool   `json:"http"`
	ReadOnly bool   `json:"read_only"`
	Path     string `json:"path"`
}

type Config struct {
	Listen      string         `json:"listen"`
	APIToken    string         `json:"api_token"`
	DeviceID    string         `json:"device_id"`
	LogDir      string         `json:"log_dir"`
	LogRequests bool           `json:"log_requests"`
	LogMaxBytes int64          `json:"log_max_bytes"`
	GetpropTTL  int            `json:"getprop_ttl_seconds"`
	WifiTTL     int            `json:"wifi_ttl_seconds"`
	PowerTTL    int            `json:"power_ttl_seconds"`
	PackagesTTL int            `json:"packages_ttl_seconds"`
	ProcsTTL    int            `json:"procs_ttl_seconds"`
	Mounts      []string       `json:"storage_mounts"`
	Security    SecurityConfig `json:"security"`
	Control     ControlConfig  `json:"control"`
	Capture     CaptureConfig  `json:"capture"`
	Stream      StreamConfig   `json:"stream"`
	Notify      NotifyConfig   `json:"notify"`
	MCP         MCPConfig      `json:"mcp"`
	Bridge      BridgeConfig   `json:"bridge"`
	Files       FilesConfig    `json:"files"`
	History     HistoryConfig  `json:"history"`
}

func defaultConfig() Config {
	return Config{
		// Loopback by default: nothing on the network can reach a fresh install.
		// Turning on LAN access is one switch in the WebUI, next to a warning.
		Listen:      "127.0.0.1:8080",
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
		Security: SecurityConfig{
			AllowNonLoopbackListen: false,
			AllowShell:             false,
			AllowPowerCommands:     false,
			ShellTimeoutMS:         15000,
			ShellMaxOutputBytes:    256 << 10,
		},
		Control: ControlConfig{
			Enabled:          true,
			DefaultTimeoutMS: 10000,
			MaxTimeoutMS:     120000,
		},
		Capture: CaptureConfig{
			CameraEnabled:  true,
			MicEnabled:     true,
			DefaultCamera:  0,
			PhotoFormat:    "jpeg",
			PhotoQuality:   85,
			PhotoMaxWidth:  0,
			PreviewFPS:     2,
			PreviewMaxWide: 540,
			PreviewQuality: 60,
			MicSeconds:     10,
			MicMaxSeconds:  300,
			MicEncoder:     "aac",
			MicBitrateKbps: 128,
			MicRateHz:      48000,
			MicChannels:    1,
			KeepFiles:      false,
		},
		Stream: StreamConfig{
			Enabled:        true,
			MaxClients:     3,
			DefaultSize:    "720x1560",
			DefaultBitrate: 3000000,
			DefaultFPS:     60,
			IdleTimeoutS:   20,
			WakeDisplay:    true,
			KeepDisplayOn:  false,
			MjpegFPS:       1,
			MjpegMaxWidth:  540,
			MjpegQuality:   60,
		},
		Notify: NotifyConfig{
			Enabled:        true,
			IncludeOngoing: false,
			Max:            200,
			CacheSeconds:   2,
		},
		MCP: MCPConfig{
			HTTP:     true,
			ReadOnly: false,
			Path:     "/mcp",
		},
		Bridge: defaultBridgeConfig(),
		Files: FilesConfig{
			Enabled: true,
			Shares: map[string]string{
				"sdcard":   "/storage/emulated/0",
				"download": "/storage/emulated/0/Download",
				"tmp":      "/data/local/tmp",
			},
			ReadOnly:       false,
			AllowDelete:    false,
			MaxUploadMB:    512,
			ShowHidden:     false,
			HTTP:           true,
			WebDAV:         true,
			Listing:        true,
			MaxListEntries: 2000,
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

// loopbackHost reports whether a listen address stays on the device itself.
//
// An empty host is NOT loopback: net.Listen treats ":8080" as "every
// interface", so accepting it here let a config of ":8080" (or a listen string
// the parser could not split) pass the loopback gate and expose the phone.
// Caught by TestEffectiveListenGatesNonLoopback.
func loopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	h = strings.Trim(h, "[]")
	if h == "::1" || h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// splitListen returns host and port, tolerating ":8080" and "127.0.0.1:8080".
func splitListen(listen string) (string, string) {
	l := strings.TrimSpace(listen)
	host, port, err := net.SplitHostPort(l)
	if err != nil {
		if strings.HasPrefix(l, ":") {
			return "", strings.TrimPrefix(l, ":")
		}
		return "", ""
	}
	return host, port
}

func mustHost(listen string) string {
	h, _ := splitListen(listen)
	return h
}

func (c *Config) norm() {
	def := defaultConfig()
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = def.Listen
	}
	if c.LogMaxBytes <= 0 {
		c.LogMaxBytes = def.LogMaxBytes
	}
	if len(c.Mounts) == 0 {
		c.Mounts = def.Mounts
	}
	if c.LogDir == "" {
		c.LogDir = def.LogDir
	}
	if c.Security.ShellTimeoutMS <= 0 {
		c.Security.ShellTimeoutMS = def.Security.ShellTimeoutMS
	}
	if c.Security.ShellMaxOutputBytes <= 0 {
		c.Security.ShellMaxOutputBytes = def.Security.ShellMaxOutputBytes
	}
	if c.Control.DefaultTimeoutMS <= 0 {
		c.Control.DefaultTimeoutMS = def.Control.DefaultTimeoutMS
	}
	if c.Control.MaxTimeoutMS <= 0 {
		c.Control.MaxTimeoutMS = def.Control.MaxTimeoutMS
	}
	if c.Capture.PhotoFormat == "" {
		c.Capture.PhotoFormat = def.Capture.PhotoFormat
	}
	if c.Capture.PhotoQuality <= 0 || c.Capture.PhotoQuality > 100 {
		c.Capture.PhotoQuality = def.Capture.PhotoQuality
	}
	if c.Capture.PreviewFPS <= 0 {
		c.Capture.PreviewFPS = def.Capture.PreviewFPS
	}
	if c.Capture.PreviewMaxWide <= 0 {
		c.Capture.PreviewMaxWide = def.Capture.PreviewMaxWide
	}
	if c.Capture.PreviewQuality <= 0 || c.Capture.PreviewQuality > 100 {
		c.Capture.PreviewQuality = def.Capture.PreviewQuality
	}
	if c.Capture.MicSeconds <= 0 {
		c.Capture.MicSeconds = def.Capture.MicSeconds
	}
	if c.Capture.MicMaxSeconds < c.Capture.MicSeconds {
		c.Capture.MicMaxSeconds = def.Capture.MicMaxSeconds
	}
	if c.Capture.MicEncoder == "" {
		c.Capture.MicEncoder = def.Capture.MicEncoder
	}
	if c.Capture.MicBitrateKbps <= 0 {
		c.Capture.MicBitrateKbps = def.Capture.MicBitrateKbps
	}
	if c.Capture.MicRateHz <= 0 {
		c.Capture.MicRateHz = def.Capture.MicRateHz
	}
	if c.Capture.MicChannels <= 0 {
		c.Capture.MicChannels = def.Capture.MicChannels
	}
	if c.Stream.MaxClients <= 0 {
		c.Stream.MaxClients = def.Stream.MaxClients
	}
	if c.Stream.DefaultSize == "" {
		c.Stream.DefaultSize = def.Stream.DefaultSize
	}
	if c.Stream.DefaultBitrate <= 0 {
		c.Stream.DefaultBitrate = def.Stream.DefaultBitrate
	}
	if c.Stream.IdleTimeoutS <= 0 {
		c.Stream.IdleTimeoutS = def.Stream.IdleTimeoutS
	}
	if c.Stream.MjpegFPS <= 0 {
		c.Stream.MjpegFPS = def.Stream.MjpegFPS
	}
	if c.Stream.MjpegMaxWidth <= 0 {
		c.Stream.MjpegMaxWidth = def.Stream.MjpegMaxWidth
	}
	if c.Stream.MjpegQuality <= 0 || c.Stream.MjpegQuality > 100 {
		c.Stream.MjpegQuality = def.Stream.MjpegQuality
	}
	if c.Notify.Max <= 0 {
		c.Notify.Max = def.Notify.Max
	}
	if c.Notify.CacheSeconds < 0 {
		c.Notify.CacheSeconds = def.Notify.CacheSeconds
	}
	// File shares: keep only sane names and absolute directories. A share name
	// is what appears in the URL, so a name with a slash or a dot-dot would
	// rebuild the traversal problem the share model exists to remove.
	if c.Files.MaxUploadMB <= 0 {
		c.Files.MaxUploadMB = def.Files.MaxUploadMB
	}
	if c.Files.MaxListEntries <= 0 {
		c.Files.MaxListEntries = def.Files.MaxListEntries
	}
	clean := map[string]string{}
	for name, dir := range c.Files.Shares {
		n := strings.Trim(strings.TrimSpace(name), "/")
		d := strings.TrimSuffix(strings.TrimSpace(dir), "/")
		if n == "" || strings.ContainsAny(n, "/\\ ") || d == "" || !strings.HasPrefix(d, "/") {
			continue
		}
		clean[n] = d
	}
	if len(clean) == 0 {
		clean = def.Files.Shares
	}
	c.Files.Shares = clean
	c.Bridge = c.Bridge.norm()
	c.History = c.History.norm()
	if c.MCP.Path == "" {
		c.MCP.Path = "/mcp"
	}
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
	cfg.norm()
	if warnf != nil {
		if cfg.APIToken == "" {
			warnf("api_token is empty — anyone who can reach this port has full control of the phone")
		}
		if h := mustHost(cfg.Listen); !loopbackHost(h) && !cfg.Security.AllowNonLoopbackListen {
			warnf("listen=%s is not loopback but security.allow_non_loopback_listen is false; falling back to 127.0.0.1", cfg.Listen)
		}
	}
	return cfg, false, nil
}

// effectiveListen applies the loopback gate at bind time as well as on config
// writes, so a hand-edited config cannot silently expose the phone.
func (c Config) effectiveListen() string {
	host, port := splitListen(c.Listen)
	if port == "" {
		port = "8080"
	}
	if !loopbackHost(host) && !c.Security.AllowNonLoopbackListen {
		return "127.0.0.1:" + port
	}
	return c.Listen
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
	shotMu sync.Mutex

	// bridge talks to the Termux:API app for camera/microphone and the
	// app-level extras that a root process cannot reach (bridge.go).
	bridge *bridgeState
	// streams owns the screen encoders (stream.go).
	streams *streamManager
	// mcpSessions backs the HTTP MCP transport (mcp.go).
	mcpSessions *mcpSessions

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
	code      int
	bytesSent int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	n, err := s.ResponseWriter.Write(p)
	s.bytesSent += n
	return n, err
}

// Flush keeps streaming responses working through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		// CORS preflight short-circuit — except for WebDAV, where OPTIONS is a
		// protocol request that has to reach the handler (it advertises DAV
		// compliance and the allowed methods). Answering 204 there makes a
		// WebDAV client think the server does not speak DAV at all.
		if r.Method == http.MethodOptions && !strings.HasPrefix(r.URL.Path, "/dav") {
			rec.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(rec, r)
		if a.config().LogRequests {
			// Streams are long-lived; log their size so a hang is visible.
			a.logf.Printf("access %s %s -> %d %dB %s %s",
				r.Method, r.URL.Path, rec.code, rec.bytesSent,
				time.Since(start).Round(time.Millisecond), r.RemoteAddr)
		}
	})
}

// tokenOK implements the auth check: X-API-Key header, Authorization: ***
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
			writeErr(w, http.StatusUnauthorized, "missing or invalid api token (send X-API-Key, Authorization: *** or ?key=)")
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

// post makes an endpoint take POST only.
func (a *App) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed; this endpoint takes POST")
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

// readJSONBody reads and decodes a request body with a size cap. An empty body
// is not an error: several endpoints take only optional fields.
func readJSONBody(r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
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
	_, port := splitListen(cfg.effectiveListen())
	out := map[string]any{
		"status":          "ok",
		"server_version":  version,
		"server_uptime_s": round2(time.Since(a.started).Seconds()),
		"auth_required":   cfg.APIToken != "",
		"listen":          cfg.effectiveListen(),
		"port":            port,
		"lan_reachable":   !loopbackHost(mustHost(cfg.effectiveListen())),
		"history_enabled": a.store != nil && h.Enabled,
		"stream_enabled":  cfg.Stream.Enabled,
		"capture": map[string]any{
			"camera": cfg.Capture.CameraEnabled,
			"mic":    cfg.Capture.MicEnabled,
		},
	}
	if a.store != nil && h.Enabled {
		out["history_ticks"] = a.histTicks.Load()
		out["history_interval_seconds"] = h.IntervalSeconds
	}
	if cfg.MCP.HTTP {
		out["mcp_http"] = true
		out["mcp_endpoint"] = cfg.MCP.Path
	}
	out["mcp_stdio"] = "phoneserver -mcp"
	if cfg.Files.Enabled {
		shares := make([]string, 0, len(cfg.Files.Shares))
		for name := range cfg.Files.Shares {
			shares = append(shares, name)
		}
		sort.Strings(shares)
		out["file_shares"] = shares
		out["file_service"] = map[string]any{
			"http":         cfg.Files.HTTP,
			"webdav":       cfg.Files.WebDAV,
			"read_only":    cfg.Files.ReadOnly,
			"allow_delete": cfg.Files.AllowDelete,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	type ep = map[string]string
	writeJSON(w, http.StatusOK, map[string]any{
		"server_version": version,
		"auth":           "X-API-Key / Authorization: Bearer *** ?key=",
		"listen_note":    "listen defaults to loopback; the LAN switch is security.allow_non_loopback_listen",
		"mcp_note":       "POST /mcp speaks MCP over HTTP; `phoneserver -mcp` runs the stdio transport",
		"endpoints": []ep{
			{"method": "GET", "path": "/api/v1/health", "desc": "liveness + server state (no auth needed)"},
			{"method": "GET", "path": "/api/v1/endpoints", "desc": "this index"},
			{"method": "GET", "path": "/api/v1/all", "desc": "every telemetry section in one document"},
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
			{"method": "GET", "path": "/api/v1/system", "desc": "uptime, kernel, selinux, loadavg"},
			{"method": "GET", "path": "/api/v1/processes", "desc": "process count + top by RSS"},
			{"method": "GET", "path": "/api/v1/packages", "desc": "installed package counts"},

			{"method": "GET", "path": "/api/v1/screenshot", "desc": "current screen as webp/jpeg/png; ?quality= ?scale= ?display= ?meta=1 ?base64=1"},
			{"method": "GET", "path": "/api/v1/stream/screen", "desc": "live screen as H.264 Annex-B over a chunked response (low latency, low bandwidth); ?size= ?bitrate= ?wake=1"},
			{"method": "GET", "path": "/api/v1/stream/screen/mjpeg", "desc": "live screen as multipart webp frames (fallback); ?fps= ?width= ?quality="},
			{"method": "GET", "path": "/api/v1/stream/status", "desc": "active encoders, measured fps/bitrate, display state"},
			{"method": "GET", "path": "/api/v1/stream/display", "desc": "is the panel awake and does screenrecord accept it right now"},

			{"method": "GET", "path": "/api/v1/ui/dump", "desc": "uiautomator hierarchy as JSON (text, bounds, center, clickable)"},
			{"method": "GET", "path": "/api/v1/ui/current", "desc": "focused window, foreground app, keyboard, keyguard"},
			{"method": "GET", "path": "/api/v1/ui/find", "desc": "find elements by text/desc/id/class"},
			{"method": "POST", "path": "/api/v1/ui/click", "desc": "{\"text\":\"登录\"} — find an element and tap its centre"},
			{"method": "POST", "path": "/api/v1/input/tap", "desc": "{\"x\":540,\"y\":1200}"},
			{"method": "POST", "path": "/api/v1/input/swipe", "desc": "{\"x1\":..,\"y1\":..,\"x2\":..,\"y2\":..,\"duration_ms\":300}"},
			{"method": "POST", "path": "/api/v1/input/gesture", "desc": "{\"points\":[[x,y],...],\"duration_ms\":400} — chained swipes"},
			{"method": "POST", "path": "/api/v1/input/text", "desc": "{\"text\":\"hello\"} — IME text into the focused field"},
			{"method": "POST", "path": "/api/v1/input/key", "desc": "{\"key\":\"HOME\"} or {\"code\":66}"},
			{"method": "POST", "path": "/api/v1/input/keys", "desc": "{\"keys\":[\"HOME\",\"BACK\"]}"},

			{"method": "GET", "path": "/api/v1/apps", "desc": "installed packages with launchable activities; ?grep="},
			{"method": "GET", "path": "/api/v1/apps/current", "desc": "foreground package + activity"},
			{"method": "GET", "path": "/api/v1/apps/activity", "desc": "activity/task stack summary"},
			{"method": "POST", "path": "/api/v1/apps/launch", "desc": "{\"package\":\"com.x\",\"activity\":\"a.B\",\"action\":\"\",\"data\":\"\",\"extras\":{},\"wait_ms\":0}"},
			{"method": "POST", "path": "/api/v1/apps/stop", "desc": "{\"package\":\"com.x\",\"force\":false}"},

			{"method": "POST", "path": "/api/v1/control/screen", "desc": "{\"action\":\"on|off|toggle\"}"},
			{"method": "POST", "path": "/api/v1/control/volume", "desc": "{\"stream\":\"music\",\"action\":\"up|down|mute|set\",\"value\":5}"},
			{"method": "POST", "path": "/api/v1/control/brightness", "desc": "{\"value\":0-255}"},
			{"method": "POST", "path": "/api/v1/control/rotation", "desc": "{\"value\":0-3|auto}"},
			{"method": "POST", "path": "/api/v1/control/torch", "desc": "{\"on\":true}"},
			{"method": "POST", "path": "/api/v1/control/vibrate", "desc": "{\"ms\":300}"},
			{"method": "POST", "path": "/api/v1/control/toast", "desc": "{\"text\":\"hi\"}"},
			{"method": "POST", "path": "/api/v1/control/clipboard", "desc": "{\"action\":\"get|set\",\"text\":\"..\"}"},
			{"method": "POST", "path": "/api/v1/control/tts", "desc": "{\"text\":\"说点什么\"}"},
			{"method": "POST", "path": "/api/v1/control/ir", "desc": "{\"freq_hz\":38000,\"pattern\":\"100,200,100\",\"repeat\":0} — IR blaster"},
			{"method": "POST", "path": "/api/v1/control/media", "desc": "{\"action\":\"play|pause|stop|next|prev|info\",\"file\":\"/sdcard/x.mp3\"}"},
			{"method": "POST", "path": "/api/v1/control/wifi", "desc": "{\"on\":true}"},
			{"method": "GET", "path": "/api/v1/control/sensors", "desc": "sensor list, or ?sensor=<name>&n=<count> for readings"},
			{"method": "GET", "path": "/api/v1/control/location", "desc": "last known location (bridge)"},
			{"method": "GET", "path": "/api/v1/control/audio", "desc": "audio streams, volumes, mute state"},

			{"method": "GET", "path": "/api/v1/notifications", "desc": "every posted notification with title/text/pkg/when; ?include_ongoing=1 ?limit= ?grep= ?raw=1"},
			{"method": "POST", "path": "/api/v1/notifications/post", "desc": "{\"title\":\"..\",\"text\":\"..\",\"id\":\"tag\"}"},
			{"method": "GET", "path": "/api/v1/messages/list", "desc": "?source=sms|calls|contacts&limit= — device messages and call log"},
			{"method": "GET", "path": "/api/v1/messages/search", "desc": "?source=sms&q=text&limit="},

			{"method": "GET", "path": "/api/v1/camera/info", "desc": "camera capabilities (bridge)"},
			{"method": "GET", "path": "/api/v1/camera/photo", "desc": "take a photo: ?camera=0&format=jpeg|webp|png|raw&quality= ?maxwidth= ?meta=1 ?base64=1"},
			{"method": "GET", "path": "/api/v1/camera/preview", "desc": "low-rate camera preview as multipart webp frames; ?fps= ?width= ?quality= ?seconds="},
			{"method": "GET", "path": "/api/v1/mic/record", "desc": "record N seconds and answer the audio; ?seconds= ?encoder= ?bitrate= ?rate= ?channels= ?format=m4a|wav"},
			{"method": "POST", "path": "/api/v1/mic/start", "desc": "{\"seconds\":0,\"encoder\":\"aac\"} start recording"},
			{"method": "POST", "path": "/api/v1/mic/stop", "desc": "stop recording and answer the file"},
			{"method": "GET", "path": "/api/v1/mic/status", "desc": "current recording state, if any"},
			{"method": "GET", "path": "/api/v1/bridge/status", "desc": "is the Termux:API bridge usable, and which policy blocks camera/mic"},
			{"method": "POST", "path": "/api/v1/bridge/fix", "desc": "grant permissions, take the wake lock, raise the uid to the foreground"},

			{"method": "POST", "path": "/api/v1/shell", "desc": "{\"cmd\":\"ls /sdcard\"} — root shell; requires security.allow_shell=true"},
			{"method": "POST", "path": "/api/v1/device/reboot", "desc": "{\"mode\":\"normal|recovery|bootloader\"} — requires security.allow_power_commands=true"},
			{"method": "POST", "path": "/api/v1/device/shutdown", "desc": "power off — requires security.allow_power_commands=true"},

			{"method": "GET", "path": "/api/v1/config", "desc": "current config (token redacted)"},
			{"method": "PUT", "path": "/api/v1/config", "desc": "update any config field; listen changes need a restart"},
			{"method": "GET", "path": "/api/v1/history", "desc": "query stored samples (metrics, since/until, limit, bucket)"},
			{"method": "GET", "path": "/api/v1/history/metrics", "desc": "list the queryable metric columns"},
			{"method": "GET", "path": "/api/v1/history/logs", "desc": "query stored log lines (grep/level/since)"},
			{"method": "GET", "path": "/api/v1/history/snapshot", "desc": "the full stored document at/near a timestamp"},
			{"method": "GET", "path": "/api/v1/history/stats", "desc": "db size, row counts, growth rate, cost estimate"},
			{"method": "GET", "path": "/api/v1/history/quality", "desc": "data-quality report: coverage, gaps, rejected values, carried columns"},
			{"method": "GET", "path": "/api/v1/history/gaps", "desc": "sampling gaps outside the expected cadence"},
			{"method": "POST", "path": "/api/v1/history/prune", "desc": "apply the retention policy now"},
			{"method": "POST", "path": "/mcp", "desc": "MCP over HTTP (JSON-RPC 2.0) for AI agents; stdio mode: `phoneserver -mcp`"},

			{"method": "GET", "path": "/files/", "desc": "file service index: the configured shares (JSON), ?listing=1 for HTML"},
			{"method": "GET", "path": "/files/<share>/<path>", "desc": "download a file (Range/HEAD supported) or list a directory as HTML; ?json=1 gives JSON"},
			{"method": "PUT", "path": "/files/<share>/<path>", "desc": "upload or replace a file (raw body); ?mkdir=1 creates missing parent directories"},
			{"method": "POST", "path": "/files/<share>/<path>", "desc": "browser upload (multipart/form-data), or ?op=mkcol|delete|move|copy&dest=<share/path>"},
			{"method": "DELETE", "path": "/files/<share>/<path>", "desc": "delete a file or an empty directory (needs files.allow_delete)"},
			{"method": "OPTIONS", "path": "/dav/<share>/<path>", "desc": "WebDAV: DAV: 1,2 + allowed methods"},
			{"method": "PROPFIND", "path": "/dav/<share>/<path>", "desc": "WebDAV directory/file metadata (Depth: 0/1/infinity)"},
			{"method": "MKCOL", "path": "/dav/<share>/<path>", "desc": "WebDAV make directory"},
			{"method": "PUT", "path": "/dav/<share>/<path>", "desc": "WebDAV upload/replace"},
			{"method": "MOVE|COPY", "path": "/dav/<share>/<path>", "desc": "WebDAV rename/copy (Destination header)"},
			{"method": "DELETE", "path": "/dav/<share>/<path>", "desc": "WebDAV delete (needs files.allow_delete)"},
			{"method": "LOCK|UNLOCK", "path": "/dav/<share>/<path>", "desc": "WebDAV locks: answered, but not enforced (see README)"},
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
	writeJSON(w, http.StatusOK, map[string]any{
		"config_path":      a.cfgPath,
		"config":           out,
		"effective_listen": c.effectiveListen(),
		"loopback_only":    loopbackHost(mustHost(c.effectiveListen())),
		"restart_required_for": []string{
			"listen", "log_dir", "log_max_bytes",
		},
	})
}

func (a *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	cur := a.config()
	next := cur
	if err := readJSONBody(r, &next, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Refuse to let a redacted placeholder overwrite a real secret.
	if strings.Contains(next.APIToken, "…") || strings.Contains(next.APIToken, "...") {
		next.APIToken = cur.APIToken
	}
	next.norm()

	// The loopback gate: a listen address outside the device is only accepted
	// when the config says so explicitly.
	host, port := splitListen(next.Listen)
	if port == "" {
		writeErr(w, http.StatusBadRequest, "listen must look like host:port, e.g. 127.0.0.1:8080")
		return
	}
	if !loopbackHost(host) && !next.Security.AllowNonLoopbackListen {
		writeErr(w, http.StatusBadRequest,
			"listen="+next.Listen+" is not loopback; set security.allow_non_loopback_listen=true to expose the API to the LAN, or keep listen on 127.0.0.1")
		return
	}

	if err := next.save(a.cfgPath); err != nil {
		writeErr(w, http.StatusInternalServerError, "save config: "+err.Error())
		return
	}
	prev := a.config()
	a.setConfig(next)

	out := next
	out.APIToken = redact(next.APIToken)
	restartNeeded := prev.Listen != next.Listen || prev.LogDir != next.LogDir || prev.LogMaxBytes != next.LogMaxBytes
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"config":           out,
		"listen_changed":   prev.Listen != next.Listen,
		"restart_needed":   restartNeeded,
		"config_path":      a.cfgPath,
		"effective_listen": next.effectiveListen(),
		"note":             "listen/port changes need a service restart (WebUI restart button, module Action, or reboot)",
	})
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

// webrootDir locates the built WebUI bundle. In production it sits next to the
// binary inside the module (/data/adb/modules/phoneserver/webroot); when the
// daemon is run from a checkout it is ./module/webroot.
func webrootDir() string {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(base, "..", "webroot"),
			filepath.Join(base, "webroot"))
	}
	candidates = append(candidates,
		"/data/adb/modules/phoneserver/webroot",
		"module/webroot",
		"webroot")
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// ---------- main ----------

func main() {
	var (
		cfgPath     = flag.String("config", defaultCfg, "path to config.json")
		listen      = flag.String("listen", "", "override listen address, e.g. 127.0.0.1:8080")
		once        = flag.Bool("once", false, "print one snapshot as JSON to stdout and exit (no server)")
		ver         = flag.Bool("version", false, "print version and exit")
		initCfg     = flag.Bool("init-config", false, "create config.json if missing, print its path, and exit")
		mcpMode     = flag.Bool("mcp", false, "run as an MCP server on stdio (for AI agents)")
		mcpURL      = flag.String("mcp-url", "", "API base URL the stdio MCP server should call (default: the configured loopback address)")
		allowRemote = flag.Bool("allow-remote", false, "permit a non-loopback --listen for this run only")
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
		cfg.norm()
	}
	if *listen != "" {
		if *allowRemote {
			cfg.Security.AllowNonLoopbackListen = true
		}
		cfg.Listen = *listen
		cfg.norm()
	}
	if *initCfg {
		// service.sh calls this at first boot so the API token exists before the
		// server starts; loadConfig already wrote the file if it was missing.
		fmt.Println(*cfgPath)
		return
	}

	if *mcpMode {
		// stdio MCP server: no listener. stdout is the protocol channel, so the
		// server must not print logs there.
		os.Exit(runMCPStdio(*cfgPath, cfg, *mcpURL))
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
		bridge:  &bridgeState{},
	}
	app.sampler = newSampler(app)
	app.streams = newStreamManager(app)
	app.mcpSessions = newMCPSessions()

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

	logger.Printf("phoneserver %s starting: listen=%s config=%s (created=%v) device_id=%q",
		version, cfg.effectiveListen(), *cfgPath, created, app.deviceID())
	if created {
		logger.Printf("generated api token; read it with: grep api_token %s", *cfgPath)
	}
	if mustHost(cfg.effectiveListen()) != mustHost(cfg.Listen) {
		logger.Printf("listen=%s refused (security.allow_non_loopback_listen=false); bound to %s instead",
			cfg.Listen, cfg.effectiveListen())
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

	// screen streaming (stream.go)
	mux.HandleFunc("/api/v1/stream/screen", app.guard(app.handleScreenStream))
	mux.HandleFunc("/api/v1/stream/screen/mjpeg", app.guard(app.handleScreenMjpeg))
	mux.HandleFunc("/api/v1/stream/status", app.get(app.handleStreamStatus))
	mux.HandleFunc("/api/v1/stream/display", app.get(app.handleStreamDisplay))

	// UI automation (ui.go)
	mux.HandleFunc("/api/v1/ui/dump", app.get(app.handleUIDump))
	mux.HandleFunc("/api/v1/ui/current", app.get(app.handleUICurrent))
	mux.HandleFunc("/api/v1/ui/find", app.get(app.handleUIFind))
	mux.HandleFunc("/api/v1/ui/click", app.post(app.handleUIClick))

	// control (control.go)
	mux.HandleFunc("/api/v1/input/tap", app.post(app.handleInputTap))
	mux.HandleFunc("/api/v1/input/swipe", app.post(app.handleInputSwipe))
	mux.HandleFunc("/api/v1/input/gesture", app.post(app.handleInputGesture))
	mux.HandleFunc("/api/v1/input/text", app.post(app.handleInputText))
	mux.HandleFunc("/api/v1/input/key", app.post(app.handleInputKey))
	mux.HandleFunc("/api/v1/input/keys", app.post(app.handleInputKeys))
	mux.HandleFunc("/api/v1/apps", app.get(app.handleApps))
	mux.HandleFunc("/api/v1/apps/current", app.get(app.handleAppsCurrent))
	mux.HandleFunc("/api/v1/apps/activity", app.get(app.handleAppsActivity))
	mux.HandleFunc("/api/v1/apps/launch", app.post(app.handleAppsLaunch))
	mux.HandleFunc("/api/v1/apps/stop", app.post(app.handleAppsStop))
	mux.HandleFunc("/api/v1/control/screen", app.post(app.handleControlScreen))
	mux.HandleFunc("/api/v1/control/volume", app.post(app.handleControlVolume))
	mux.HandleFunc("/api/v1/control/brightness", app.post(app.handleControlBrightness))
	mux.HandleFunc("/api/v1/control/rotation", app.post(app.handleControlRotation))
	mux.HandleFunc("/api/v1/control/torch", app.post(app.handleControlTorch))
	mux.HandleFunc("/api/v1/control/vibrate", app.post(app.handleControlVibrate))
	mux.HandleFunc("/api/v1/control/toast", app.post(app.handleControlToast))
	mux.HandleFunc("/api/v1/control/clipboard", app.post(app.handleControlClipboard))
	mux.HandleFunc("/api/v1/control/tts", app.post(app.handleControlTTS))
	mux.HandleFunc("/api/v1/control/ir", app.post(app.handleControlIR))
	mux.HandleFunc("/api/v1/control/media", app.post(app.handleControlMedia))
	mux.HandleFunc("/api/v1/control/wifi", app.post(app.handleControlWifi))
	mux.HandleFunc("/api/v1/control/sensors", app.get(app.handleControlSensors))
	mux.HandleFunc("/api/v1/control/location", app.get(app.handleControlLocation))
	mux.HandleFunc("/api/v1/control/audio", app.get(app.handleControlAudio))
	mux.HandleFunc("/api/v1/shell", app.post(app.handleShell))
	mux.HandleFunc("/api/v1/device/reboot", app.post(app.handleDeviceReboot))
	mux.HandleFunc("/api/v1/device/shutdown", app.post(app.handleDeviceShutdown))

	// notifications and messages (notify.go)
	mux.HandleFunc("/api/v1/notifications", app.get(app.handleNotifications))
	mux.HandleFunc("/api/v1/notifications/post", app.post(app.handleNotificationPost))
	mux.HandleFunc("/api/v1/messages/list", app.get(app.handleMessagesList))
	mux.HandleFunc("/api/v1/messages/search", app.get(app.handleMessagesSearch))

	// capture (capture.go)
	mux.HandleFunc("/api/v1/camera/info", app.get(app.handleCameraInfo))
	mux.HandleFunc("/api/v1/camera/photo", app.get(app.handleCameraPhoto))
	mux.HandleFunc("/api/v1/camera/preview", app.guard(app.handleCameraPreview))
	mux.HandleFunc("/api/v1/mic/record", app.guard(app.handleMicRecord))
	mux.HandleFunc("/api/v1/mic/start", app.post(app.handleMicStart))
	mux.HandleFunc("/api/v1/mic/stop", app.post(app.handleMicStop))
	mux.HandleFunc("/api/v1/mic/status", app.get(app.handleMicStatus))
	mux.HandleFunc("/api/v1/bridge/status", app.get(app.handleBridgeStatus))
	mux.HandleFunc("/api/v1/bridge/fix", app.post(app.handleBridgeFix))

	// File service: plain HTTP browse/upload and a WebDAV share (fileserver.go).
	// Both are off unless files.enabled, and both are token-guarded inside their
	// own handlers (WebDAV clients cannot always set custom headers, so those
	// handlers also accept HTTP Basic with the token as the password).
	if cfg.Files.Enabled {
		mux.HandleFunc("/files/", app.handleFilesHTTP)
		mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/files/", http.StatusMovedPermanently)
		})
		if cfg.Files.WebDAV {
			mux.HandleFunc("/dav/", app.handleFilesWebDAV)
			mux.HandleFunc("/dav", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
			})
		}
		logger.Printf("files: shares=%d http=%v webdav=%v read_only=%v allow_delete=%v",
			len(cfg.Files.Shares), cfg.Files.HTTP, cfg.Files.WebDAV, cfg.Files.ReadOnly, cfg.Files.AllowDelete)
	}

	// MCP over HTTP (mcp.go)
	if cfg.MCP.HTTP {
		path := cfg.MCP.Path
		if path == "" {
			path = "/mcp"
		}
		mux.HandleFunc(path, app.handleMCPHTTP)
		logger.Printf("mcp: HTTP endpoint enabled at %s (stdio mode: `phoneserver -mcp`)", path)
	}

	// The built WebUI (multi-page Preact app) is served at /ui/ so the same UI
	// works in a desktop browser as well as inside the manager's WebView. The
	// static files are deliberately not behind the token — exactly like "/",
	// which is also open: the page is a shell that carries no data, and every
	// API call it makes still needs the token, which the page keeps in
	// localStorage. The Vite build uses relative asset paths, hence the
	// trailing-slash mount point.
	if dir := webrootDir(); dir != "" {
		files := http.FileServer(http.Dir(dir))
		mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			http.StripPrefix("/ui/", files).ServeHTTP(w, r)
		})
		mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusFound)
		})
		logger.Printf("webui: serving %s at /ui/", dir)
	} else {
		logger.Printf("webui: no webroot/ with index.html found; /ui/ is not served in this build")
	}

	mux.HandleFunc("/api/v1/config", app.guard(app.dispatch(map[string]http.HandlerFunc{
		http.MethodGet: app.handleGetConfig,
		http.MethodPut: app.handlePutConfig,
	})))
	mux.HandleFunc("/api/v1/history", app.get(app.handleHistory))
	mux.HandleFunc("/api/v1/history/metrics", app.get(app.handleHistoryMetrics))
	mux.HandleFunc("/api/v1/history/logs", app.get(app.handleHistoryLogs))
	mux.HandleFunc("/api/v1/history/snapshot", app.get(app.handleHistorySnapshot))
	mux.HandleFunc("/api/v1/history/stats", app.get(app.handleHistoryStats))
	mux.HandleFunc("/api/v1/history/quality", app.get(app.handleHistoryQuality))
	mux.HandleFunc("/api/v1/history/gaps", app.get(app.handleHistoryGaps))
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

	bind := cfg.effectiveListen()
	srv := &http.Server{
		Addr:              bind,
		Handler:           app.withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streams are long-lived: a write timeout would cut a viewer off
		// mid-frame. The per-stream idle guard in stream.go handles stalls.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     logger,
	}

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
		logger.Printf("FATAL listen on %s failed: %v", bind, err)
		os.Exit(1)
	case sig := <-sigCh:
		logger.Printf("received %s, shutting down", sig)
		if app.histStop != nil {
			close(app.histStop)
		}
		app.streams.stopAll("server shutting down")
		if app.store != nil {
			// Give the batched log writer a moment to drain before closing.
			time.Sleep(150 * time.Millisecond)
			app.store.Close()
		}
		srv.Close()
	}
}
