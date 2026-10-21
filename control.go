package main

// control.go — driving the phone.
//
// Three layers, in order of how much they depend on:
//
//   - platform commands (input, am, pm, cmd, settings): a rooted process can
//     use these directly, which is what most of this file does.
//   - the Termux:API bridge: only for the handful of things that need an app
//     context — torch, vibration, toast, clipboard, TTS, IR blaster, media
//     playback, sensors, location, Wi-Fi toggle.
//   - a root shell: genuinely powerful, so it is off by default and gated by
//     security.allow_shell.
//
// Every handler answers with what actually happened. A tap reports the
// coordinates it sent; a launch reports the foreground package it found
// afterwards, not the intent it hoped to start.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- shared helpers ----------

// controlGate refuses every control endpoint when control is switched off.
func (a *App) controlGate(w http.ResponseWriter) bool {
	if !a.config().Control.Enabled {
		writeErr(w, http.StatusServiceUnavailable, "control endpoints are disabled (config control.enabled=false)")
		return false
	}
	return true
}

func (a *App) inputTimeout() time.Duration {
	ms := a.config().Control.DefaultTimeoutMS
	if ms <= 0 {
		ms = 10000
	}
	return time.Duration(ms) * time.Millisecond
}

// cappedBuffer collects process output up to a limit and reports truncation
// instead of letting a runaway command eat memory.
type cappedBuffer struct {
	buf       strings.Builder
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.max <= 0 {
		c.max = 256 << 10
	}
	if c.buf.Len() >= c.max {
		c.truncated = true
		return len(p), nil
	}
	room := c.max - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// runShell runs a command through /system/bin/sh and reports the exit code.
func (a *App) runShell(ctx context.Context, cmd string, timeoutMS, maxOut int) (string, int, int64, bool, error) {
	if timeoutMS <= 0 {
		timeoutMS = a.config().Security.ShellTimeoutMS
	}
	if t := a.config().Control.MaxTimeoutMS; t > 0 && timeoutMS > t {
		timeoutMS = t
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	start := time.Now()
	c := exec.CommandContext(cctx, "/system/bin/sh", "-c", cmd)
	buf := &cappedBuffer{max: maxOut}
	c.Stdout, c.Stderr = buf, buf
	code := 0
	err := c.Run()
	if ee := new(exec.ExitError); errors.As(err, &ee) {
		code = ee.ExitCode()
		err = nil
	}
	ms := time.Since(start).Milliseconds()
	if cctx.Err() == context.DeadlineExceeded {
		return buf.String(), -1, ms, buf.truncated, fmt.Errorf("command timed out after %dms", timeoutMS)
	}
	return buf.String(), code, ms, buf.truncated, err
}

type panelSize struct {
	W int `json:"width"`
	H int `json:"height"`
}

// displaySize reports the panel size, used to keep injected coordinates sane.
func (a *App) displaySize(ctx context.Context) (int, int) {
	ps, err := cacheGet(a.cache, "wmsize", 5*time.Minute, func() (panelSize, error) {
		out, err := a.runCmd(ctx, 10*time.Second, "/system/bin/wm", "size")
		if err != nil && out == "" {
			return panelSize{}, fmt.Errorf("wm size: %v", err)
		}
		w, h := 0, 0
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, "size:") {
				continue
			}
			part := strings.TrimSpace(line[strings.Index(line, "size:")+5:])
			if i := strings.Index(part, "x"); i > 0 {
				nw, _ := strconv.Atoi(strings.TrimSpace(part[:i]))
				nh, _ := strconv.Atoi(strings.TrimSpace(strings.Fields(part[i+1:])[0]))
				if nw > 0 && nh > 0 {
					w, h = nw, nh // Physical size wins when Override is also printed
				}
			}
		}
		if w == 0 || h == 0 {
			return panelSize{}, fmt.Errorf("could not parse `wm size` output")
		}
		return panelSize{W: w, H: h}, nil
	})
	if err != nil {
		return 0, 0
	}
	return ps.W, ps.H
}

// ---------- input ----------

func (a *App) handleInputTap(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		X int `json:"x"`
		Y int `json:"y"`
		D int `json:"display"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if body.X <= 0 && body.Y <= 0 {
		writeErr(w, http.StatusBadRequest, `give "x" and "y" (pixels, top-left origin)`)
		return
	}
	args := []string{}
	if body.D > 0 {
		args = append(args, "-d", strconv.Itoa(body.D))
	}
	args = append(args, "tap", strconv.Itoa(body.X), strconv.Itoa(body.Y))
	out, err := a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", args...)
	res := map[string]any{"ok": err == nil, "tapped": []int{body.X, body.Y}}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleInputSwipe(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		X1, Y1, X2, Y2 int
		DurationMS     int `json:"duration_ms"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	dur := body.DurationMS
	if dur <= 0 {
		dur = 300
	}
	out, err := a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "swipe",
		strconv.Itoa(body.X1), strconv.Itoa(body.Y1), strconv.Itoa(body.X2), strconv.Itoa(body.Y2), strconv.Itoa(dur))
	res := map[string]any{"ok": err == nil, "swiped": []int{body.X1, body.Y1, body.X2, body.Y2}, "duration_ms": dur}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

// handleInputGesture walks a path of points with real DOWN/MOVE/UP events, which
// is what long-press-and-drag and multi-step gestures need (a single `input
// swipe` cannot express a path with corners, and `input draganddrop` uses a
// different event stream that some apps ignore).
func (a *App) handleInputGesture(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Points     [][]int `json:"points"`
		DurationMS int     `json:"duration_ms"`
		Steps      int     `json:"steps"` // intermediate MOVE events per segment
	}
	if err := readJSONBody(r, &body, 1<<14); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Points) < 2 {
		writeErr(w, http.StatusBadRequest, `"points" needs at least two [x,y] pairs`)
		return
	}
	for _, p := range body.Points {
		if len(p) != 2 {
			writeErr(w, http.StatusBadRequest, "every point must be [x,y]")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if body.DurationMS <= 0 {
		body.DurationMS = 400
	}
	segments := len(body.Points) - 1
	perSegment := body.DurationMS / segments
	if perSegment < 40 {
		perSegment = 40
	}
	steps := body.Steps
	if steps <= 0 {
		steps = 8
	}
	sent := 0
	first := body.Points[0]
	run := func(ev string, x, y int) error {
		out, err := a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "motionevent", ev, strconv.Itoa(x), strconv.Itoa(y))
		if err != nil {
			return fmt.Errorf("%s %d,%d: %s", ev, x, y, firstLine(out))
		}
		sent++
		return nil
	}
	if err := run("DOWN", first[0], first[1]); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	for i := 1; i < len(body.Points); i++ {
		from, to := body.Points[i-1], body.Points[i]
		stepSleep := time.Duration(perSegment/steps) * time.Millisecond
		if stepSleep < 5*time.Millisecond {
			stepSleep = 5 * time.Millisecond
		}
		for s := 1; s <= steps; s++ {
			x := from[0] + (to[0]-from[0])*s/steps
			y := from[1] + (to[1]-from[1])*s/steps
			if err := run("MOVE", x, y); err != nil {
				a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "motionevent", "CANCEL", strconv.Itoa(x), strconv.Itoa(y))
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
			time.Sleep(stepSleep)
		}
	}
	last := body.Points[len(body.Points)-1]
	err := run("UP", last[0], last[1])
	res := map[string]any{"ok": err == nil, "points": body.Points, "events_sent": sent, "duration_ms": body.DurationMS}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// handleInputText types into the focused field.
//
// `input text` only injects ASCII: non-ASCII characters come through as
// question marks, which is a dead end for Chinese text. For those, the honest
// route on a stock device is the clipboard plus the paste key, so that is what
// this handler does when the bridge is available, and it says which path it
// took. Either way the text ends up in the same place; only the mechanism and
// the latency differ.
func (a *App) handleInputText(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSONBody(r, &body, 1<<18); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, `"text" is required`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res := map[string]any{"text": body.Text, "length": len([]rune(body.Text))}

	if isASCII(body.Text) {
		// `input text` treats %s as a space; everything else is passed through.
		arg := strings.ReplaceAll(body.Text, " ", "%s")
		out, err := a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "text", arg)
		res["method"] = "input text"
		res["ok"] = err == nil
		if err != nil {
			res["error"] = firstLine(out)
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	if _, err := a.bridgeExec(ctx, 20*time.Second, "termux-clipboard-set", body.Text); err != nil {
		res["method"] = "clipboard+paste"
		res["ok"] = false
		res["error"] = "non-ASCII text needs the clipboard route and the Termux:API bridge is unavailable: " + err.Error()
		res["hint"] = "POST /api/v1/bridge/fix, or type ASCII only with /api/v1/input/text"
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	// KEYCODE_PASTE (279) hands the clipboard to the focused field.
	out, err := a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "keyevent", "279")
	res["method"] = "clipboard+paste"
	res["ok"] = err == nil
	res["keyevent"] = "KEYCODE_PASTE"
	res["clipboard_length"] = len([]rune(body.Text))
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

var reKeyName = regexp.MustCompile(`^(KEYCODE_)?([A-Za-z0-9_]+)$`)

func normalizeKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if _, err := strconv.Atoi(v); err == nil {
		return v
	}
	if m := reKeyName.FindStringSubmatch(v); len(m) == 3 {
		return "KEYCODE_" + strings.ToUpper(m[2])
	}
	return v
}

func (a *App) handleInputKey(w http.ResponseWriter, r *http.Request) {
	a.inputKeyCommon(w, r, false)
}

func (a *App) handleInputKeys(w http.ResponseWriter, r *http.Request) {
	a.inputKeyCommon(w, r, true)
}

func (a *App) inputKeyCommon(w http.ResponseWriter, r *http.Request, many bool) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Key        string   `json:"key"`
		Code       int      `json:"code"`
		Keys       []string `json:"keys"`
		LongPress  bool     `json:"longpress"`
		DurationMS int      `json:"duration_ms"`
	}
	if err := readJSONBody(r, &body, 1<<13); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	keys := []string{}
	if many {
		for _, k := range body.Keys {
			if v := normalizeKey(k); v != "" {
				keys = append(keys, v)
			}
		}
	} else {
		if body.Code != 0 {
			keys = append(keys, strconv.Itoa(body.Code))
		} else if v := normalizeKey(body.Key); v != "" {
			keys = append(keys, v)
		}
	}
	if len(keys) == 0 {
		writeErr(w, http.StatusBadRequest, `give "key" (e.g. HOME, BACK, ENTER) or "code" (e.g. 66), or "keys" as a list`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	args := []string{"keyevent"}
	if body.LongPress {
		args = append(args, "--longpress")
	}
	if body.DurationMS > 0 {
		args = append(args, "--duration", strconv.Itoa(body.DurationMS))
	}
	args = append(args, keys...)
	out, err := a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", args...)
	res := map[string]any{"ok": err == nil, "keys": keys}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- apps ----------

type appInfo struct {
	Package  string `json:"package"`
	Path     string `json:"path,omitempty"`
	System   bool   `json:"system"`
	Disabled bool   `json:"disabled"`
	Updated  bool   `json:"updated,omitempty"`
}

var appsCache = struct {
	mu   sync.Mutex
	at   time.Time
	list []appInfo
}{}

func (a *App) installedApps(ctx context.Context) ([]appInfo, error) {
	ttl := a.ttl(a.config().PackagesTTL, 5*time.Minute)
	appsCache.mu.Lock()
	if appsCache.list != nil && time.Since(appsCache.at) < ttl {
		list := appsCache.list
		appsCache.mu.Unlock()
		return list, nil
	}
	appsCache.mu.Unlock()

	byPkg := map[string]*appInfo{}
	add := func(out string, apply func(*appInfo)) {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "package:") {
				continue
			}
			rest := strings.TrimPrefix(line, "package:")
			pkg := rest
			path := ""
			if i := strings.Index(rest, "="); i >= 0 {
				path, pkg = rest[:i], rest[i+1:]
			}
			if pkg == "" {
				continue
			}
			ai, ok := byPkg[pkg]
			if !ok {
				ai = &appInfo{Package: pkg}
				byPkg[pkg] = ai
			}
			if path != "" {
				ai.Path = path
			}
			apply(ai)
		}
	}
	if out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/pm", "list", "packages", "-f"); err == nil || out != "" {
		add(out, func(ai *appInfo) {})
	}
	if out, err := a.runCmd(ctx, 20*time.Second, "/system/bin/pm", "list", "packages", "-s"); err == nil || out != "" {
		add(out, func(ai *appInfo) { ai.System = true })
	}
	if out, err := a.runCmd(ctx, 20*time.Second, "/system/bin/pm", "list", "packages", "-d"); err == nil || out != "" {
		add(out, func(ai *appInfo) { ai.Disabled = true })
	}
	if out, err := a.runCmd(ctx, 20*time.Second, "/system/bin/pm", "list", "packages", "-3"); err == nil || out != "" {
		// -3 = installed in /data; on HyperOS that includes MIUI preloads, so it
		// is reported as "updated" rather than "user installed".
		add(out, func(ai *appInfo) { ai.Updated = true })
	}
	out := make([]appInfo, 0, len(byPkg))
	for _, ai := range byPkg {
		out = append(out, *ai)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	appsCache.mu.Lock()
	appsCache.list, appsCache.at = out, time.Now()
	appsCache.mu.Unlock()
	if len(out) == 0 {
		return nil, fmt.Errorf("no packages listed")
	}
	return out, nil
}

func (a *App) handleApps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	list, err := a.installedApps(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	q := r.URL.Query()
	grep := strings.ToLower(q.Get("grep"))
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	out := make([]appInfo, 0, len(list))
	for _, ai := range list {
		if grep != "" && !strings.Contains(strings.ToLower(ai.Package), grep) {
			continue
		}
		out = append(out, ai)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	res := map[string]any{
		"count":  len(out),
		"total":  len(list),
		"system": countIf(list, func(ai appInfo) bool { return ai.System }),
		"data":   countIf(list, func(ai appInfo) bool { return !ai.System }),
		"apps":   out,
		"note":   "no labels: reading an APK's label needs aapt, which this device does not ship. `path` and the package name are what is available.",
	}
	if pkg := q.Get("package"); pkg != "" {
		act, err := a.resolveActivity(ctx, pkg)
		res["resolved_package"] = pkg
		res["launch_activity"] = act
		if err != nil {
			res["resolve_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func countIf(list []appInfo, f func(appInfo) bool) int {
	n := 0
	for _, ai := range list {
		if f(ai) {
			n++
		}
	}
	return n
}

func (a *App) resolveActivity(ctx context.Context, pkg string) (string, error) {
	out, err := a.runCmd(ctx, 20*time.Second, "/system/bin/cmd", "package", "resolve-activity", "--brief", "--user", "0", pkg)
	if err != nil && out == "" {
		return "", fmt.Errorf("resolve-activity %s: %v", pkg, err)
	}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "priority=") || strings.HasPrefix(l, "No activity") {
			continue
		}
		if strings.Contains(l, "/") {
			return l, nil
		}
	}
	return "", fmt.Errorf("no launchable activity for %s", pkg)
}

func (a *App) handleAppsCurrent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ws := a.windowState(ctx)
	out := map[string]any{"window": ws, "package": ws.ForegroundApp}
	if ws.TopActivity != "" {
		out["top_activity"] = ws.TopActivity
	}
	if out["package"] == "" && ws.TopActivity != "" {
		out["package"] = strings.SplitN(ws.TopActivity, "/", 2)[0]
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleAppsActivity(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	limit := 60
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = minInt(n, 400)
		}
	}
	out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/dumpsys", "activity", "activities")
	if err != nil && out == "" {
		writeErr(w, http.StatusBadGateway, "dumpsys activity failed: "+err.Error())
		return
	}
	lines := strings.Split(out, "\n")
	interesting := []string{}
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "topResumedActivity=") || strings.HasPrefix(l, "mResumedActivity") ||
			strings.HasPrefix(l, "ResumedActivity") || strings.HasPrefix(l, "mFocusedApp") ||
			strings.HasPrefix(l, "ACTIVITY ") || strings.HasPrefix(l, "Task{") ||
			strings.HasPrefix(l, "Hist #") || strings.Contains(l, "state=") {
			interesting = append(interesting, l)
			if len(interesting) >= limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_lines": len(lines),
		"summary":     interesting,
		"window":      a.windowState(ctx),
	})
}

func (a *App) handleAppsLaunch(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Package        string            `json:"package"`
		Activity       string            `json:"activity"`
		Action         string            `json:"action"`
		Data           string            `json:"data"`
		Extras         map[string]string `json:"extras"`
		WaitMS         int               `json:"wait_ms"`
		ForceStopFirst bool              `json:"force_stop_first"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Package == "" && body.Activity == "" && body.Action == "" {
		writeErr(w, http.StatusBadRequest, `give "package" (and optionally "activity"), or an "action" to send`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	before := a.windowState(ctx).ForegroundApp
	if body.ForceStopFirst && body.Package != "" {
		a.runCmd(ctx, 20*time.Second, "/system/bin/am", "force-stop", body.Package)
		time.Sleep(300 * time.Millisecond)
	}
	args := []string{"start", "--user", "0"}
	if body.WaitMS > 0 {
		args = append(args, "-W")
	}
	if body.Package != "" {
		act := body.Activity
		if act == "" {
			if resolved, err := a.resolveActivity(ctx, body.Package); err == nil {
				if i := strings.Index(resolved, "/"); i > 0 {
					act = resolved[i+1:]
				}
			}
		}
		if act != "" {
			if !strings.Contains(act, "/") {
				act = body.Package + "/" + act
			}
			args = append(args, "-n", act)
		} else {
			// No entry activity resolved: let the launcher intent decide.
			args = append(args, "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", body.Package)
		}
		args = append(args, "-p", body.Package)
	}
	if body.Action != "" {
		args = append(args, "-a", body.Action)
	}
	if body.Data != "" {
		args = append(args, "-d", body.Data)
	}
	keys := make([]string, 0, len(body.Extras))
	for k := range body.Extras {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--es", k, body.Extras[k])
	}
	start := time.Now()
	out, err := a.runCmd(ctx, 60*time.Second, "/system/bin/am", args...)
	launchMS := time.Since(start).Milliseconds()
	if body.WaitMS > 0 {
		time.Sleep(time.Duration(body.WaitMS) * time.Millisecond)
	} else {
		time.Sleep(700 * time.Millisecond)
	}
	after := a.windowState(ctx)
	res := map[string]any{
		"ok":                 err == nil,
		"am_args":            args,
		"launch_ms":          launchMS,
		"foreground_before":  before,
		"foreground_after":   after.ForegroundApp,
		"changed_foreground": after.ForegroundApp != before,
		"window":             after,
	}
	if err != nil {
		res["error"] = firstLine(out)
	} else if out != "" {
		res["am_output"] = firstLine(out)
	}
	if err == nil && body.Package != "" && after.ForegroundApp != "" && after.ForegroundApp != body.Package {
		res["warning"] = "the intent was accepted but a different package is in the foreground; check `window.note` (a locked screen or a permission dialog can steal focus)"
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleAppsStop(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Package string `json:"package"`
		Kill    bool   `json:"kill"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Package == "" {
		writeErr(w, http.StatusBadRequest, `"package" is required`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	args := []string{"force-stop", body.Package}
	if body.Kill {
		args = []string{"kill", body.Package}
	}
	out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/am", args...)
	res := map[string]any{"ok": err == nil, "package": body.Package, "mode": args[0]}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- device settings ----------

func (a *App) handleControlScreen(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Action string `json:"action"` // on | off | toggle | sleep | wake
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	action := strings.ToLower(body.Action)
	if action == "" {
		action = "toggle"
	}
	before := a.collectDisplay().ScreenOn
	wake := func() (string, error) {
		return a.runCmd(ctx, 15*time.Second, "/system/bin/input", "keyevent", "KEYCODE_WAKEUP")
	}
	sleep := func() (string, error) {
		return a.runCmd(ctx, 15*time.Second, "/system/bin/input", "keyevent", "KEYCODE_SLEEP")
	}
	var err error
	used := ""
	switch action {
	case "on", "wake":
		_, err = wake()
		used = "KEYCODE_WAKEUP"
	case "off", "sleep":
		_, err = sleep()
		used = "KEYCODE_SLEEP"
	case "toggle":
		if before {
			_, err = sleep()
			used = "KEYCODE_SLEEP"
		} else {
			_, err = wake()
			used = "KEYCODE_WAKEUP"
		}
	default:
		writeErr(w, http.StatusBadRequest, `"action" must be one of on, off, toggle, wake, sleep`)
		return
	}
	time.Sleep(400 * time.Millisecond)
	after := a.collectDisplay()
	res := map[string]any{
		"ok":            err == nil,
		"action":        action,
		"keyevent":      used,
		"screen_before": before,
		"screen_after":  after.ScreenOn,
		"wakefulness":   after.Wakefulness,
	}
	if err != nil {
		res["error"] = err.Error()
	}
	if action == "on" || action == "wake" {
		res["note"] = "waking the display does not unlock the phone; a keyguard still blocks app-level automation"
	}
	writeJSON(w, http.StatusOK, res)
}

var reVolumeGate = regexp.MustCompile(`(?m)^\s*-\s*STREAM_(\w+):\s*(\d+)`)

// audioVolumes returns the per-stream volume as the platform reports it.
func (a *App) audioVolumes(ctx context.Context) (map[string]int, string) {
	if out, err := a.bridgeExec(ctx, 15*time.Second, "termux-volume"); err == nil {
		var arr []struct {
			Stream string `json:"stream"`
			Volume int    `json:"volume"`
			Max    int    `json:"max_volume"`
		}
		if jerr := json.Unmarshal([]byte(out), &arr); jerr == nil && len(arr) > 0 {
			m := map[string]int{}
			for _, v := range arr {
				m[v.Stream] = v.Volume
			}
			return m, "termux-volume"
		}
	}
	out, _ := a.runCmd(ctx, 15*time.Second, "/system/bin/dumpsys", "audio")
	m := map[string]int{}
	for _, match := range reVolumeGate.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(match[2])
		m[strings.ToLower(match[1])] = n
	}
	return m, "dumpsys audio"
}

func (a *App) handleControlVolume(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Action string `json:"action"` // up | down | mute | toggle_mute
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	key := ""
	switch strings.ToLower(body.Action) {
	case "up", "raise", "louder":
		key = "KEYCODE_VOLUME_UP"
	case "down", "lower", "quieter":
		key = "KEYCODE_VOLUME_DOWN"
	case "mute", "toggle_mute":
		key = "KEYCODE_VOLUME_MUTE"
	default:
		writeErr(w, http.StatusBadRequest, `"action" must be up, down or mute (use /api/v1/control/audio to read the levels)`)
		return
	}
	// A stream that is not the active one can be targeted with --stream; the
	// key events above always move the active stream, which is what the volume
	// rocker on the phone does. Say which one happened.
	out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/input", "keyevent", key)
	time.Sleep(250 * time.Millisecond)
	vols, src := a.audioVolumes(ctx)
	res := map[string]any{"ok": err == nil, "keyevent": key, "volumes": vols, "volumes_source": src,
		"note": "volume keys move the active stream; use POST /api/v1/control/media or the phone's UI for a specific stream"}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlBrightness(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Value int   `json:"value"` // absolute, in the panel's own range
		Pct   int   `json:"percent"`
		Auto  *bool `json:"auto"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	before := a.collectDisplay()
	maxB := int(before.BrightnessMax)
	if maxB <= 0 {
		maxB = 255
	}
	val := body.Value
	if val == 0 && body.Pct > 0 {
		val = maxB * body.Pct / 100
	}
	if val <= 0 && body.Auto == nil {
		writeErr(w, http.StatusBadRequest, `give "value" (0..max), "percent", or "auto":true|false`)
		return
	}
	if val > maxB {
		val = maxB
	}
	steps := []any{}
	if body.Auto != nil {
		mode := "0"
		if *body.Auto {
			mode = "1"
		}
		if out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/settings", "put", "system", "screen_brightness_mode", mode); err != nil {
			steps = append(steps, map[string]any{"step": "auto", "ok": false, "error": firstLine(out)})
		} else {
			steps = append(steps, map[string]any{"step": "auto", "ok": true, "auto": *body.Auto})
		}
	}
	if val > 0 {
		if out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/settings", "put", "system", "screen_brightness", strconv.Itoa(val)); err != nil {
			steps = append(steps, map[string]any{"step": "brightness", "ok": false, "error": firstLine(out)})
		} else {
			steps = append(steps, map[string]any{"step": "brightness", "ok": true, "value": val})
		}
	}
	time.Sleep(200 * time.Millisecond)
	after := a.collectDisplay()
	writeJSON(w, http.StatusOK, map[string]any{
		"steps": steps, "brightness_before": before.Brightness, "brightness_after": after.Brightness,
		"brightness_max": maxB, "source_before": before.BrightnessSource,
	})
}

func (a *App) handleControlRotation(w http.ResponseWriter, r *http.Request) {
	if !a.controlGate(w) {
		return
	}
	var body struct {
		Value string `json:"value"` // 0..3 or "auto"
		Lock  *bool  `json:"lock"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := strings.ToLower(strings.TrimSpace(body.Value))
	steps := []any{}
	if v == "auto" {
		out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/settings", "put", "system", "accelerometer_rotation", "1")
		steps = append(steps, map[string]any{"step": "auto-rotate on", "ok": err == nil, "out": firstLine(out)})
	} else {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 3 {
			writeErr(w, http.StatusBadRequest, `"value" must be 0,1,2,3 or "auto"`)
			return
		}
		out1, err1 := a.runCmd(ctx, 15*time.Second, "/system/bin/settings", "put", "system", "accelerometer_rotation", "0")
		out2, err2 := a.runCmd(ctx, 15*time.Second, "/system/bin/settings", "put", "system", "user_rotation", strconv.Itoa(n))
		steps = append(steps,
			map[string]any{"step": "auto-rotate off", "ok": err1 == nil, "out": firstLine(out1)},
			map[string]any{"step": "rotation", "ok": err2 == nil, "value": n, "out": firstLine(out2)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps, "note": "0=natural 1=90° 2=180° 3=270°"})
}

// ---------- bridge-backed extras ----------

func (a *App) handleControlTorch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On *bool `json:"on"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	arg := ""
	if body.On != nil {
		arg = "off"
		if *body.On {
			arg = "on"
		}
	}
	args := []string{}
	if arg != "" {
		args = append(args, arg)
	}
	out, err := a.bridgeExec(ctx, 20*time.Second, "termux-torch", args...)
	res := map[string]any{"ok": err == nil, "action": firstNonEmpty(arg, "query"), "output": firstLine(out)}
	if err != nil {
		res["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlVibrate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MS int `json:"ms"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.MS <= 0 {
		body.MS = 300
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	out, err := a.bridgeExec(ctx, 20*time.Second, "termux-vibrate", "-d", strconv.Itoa(body.MS))
	res := map[string]any{"ok": err == nil, "ms": body.MS, "output": firstLine(out)}
	if err != nil {
		res["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlToast(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string `json:"text"`
		Short    bool   `json:"short"`
		Position string `json:"position"`
	}
	if err := readJSONBody(r, &body, 1<<14); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, `"text" is required`)
		return
	}
	args := []string{"-s"}
	if !body.Short {
		args = []string{}
	}
	if body.Position != "" {
		args = append(args, "-p", body.Position)
	}
	args = append(args, body.Text)
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	out, err := a.bridgeExec(ctx, 20*time.Second, "termux-toast", args...)
	res := map[string]any{"ok": err == nil, "text": body.Text, "output": firstLine(out)}
	if err != nil {
		res["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlClipboard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"` // get | set
		Text   string `json:"text"`
	}
	if err := readJSONBody(r, &body, 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	switch strings.ToLower(body.Action) {
	case "set":
		out, err := a.bridgeExec(ctx, 20*time.Second, "termux-clipboard-set", body.Text)
		res := map[string]any{"ok": err == nil, "action": "set", "length": len([]rune(body.Text)), "output": firstLine(out)}
		if err != nil {
			res["error"] = err.Error()
			writeJSON(w, http.StatusServiceUnavailable, res)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "get", "":
		out, err := a.bridgeExec(ctx, 20*time.Second, "termux-clipboard-get")
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "get", "text": out, "length": len([]rune(out))})
	default:
		writeErr(w, http.StatusBadRequest, `"action" must be get or set`)
	}
}

func (a *App) handleControlTTS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string  `json:"text"`
		Rate  float64 `json:"rate"`
		Pitch float64 `json:"pitch"`
		Lang  string  `json:"lang"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, `"text" is required`)
		return
	}
	args := []string{}
	if body.Rate > 0 {
		args = append(args, "-r", strconv.FormatFloat(body.Rate, 'f', 2, 64))
	}
	if body.Pitch > 0 {
		args = append(args, "-p", strconv.FormatFloat(body.Pitch, 'f', 2, 64))
	}
	if body.Lang != "" {
		args = append(args, "-l", body.Lang)
	}
	args = append(args, body.Text)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out, err := a.bridgeExec(ctx, 60*time.Second, "termux-tts-speak", args...)
	res := map[string]any{"ok": err == nil, "text": body.Text, "output": firstLine(out)}
	if err != nil {
		res["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlIR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FreqHz    int    `json:"freq_hz"`
		Pattern   string `json:"pattern"`
		Repeat    int    `json:"repeat"`
		FreqsOnly bool   `json:"frequencies"`
	}
	if err := readJSONBody(r, &body, 1<<14); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if body.FreqsOnly {
		out, err := a.bridgeExec(ctx, 20*time.Second, "termux-infrared-frequencies")
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frequencies": out})
		return
	}
	if body.Pattern == "" {
		writeErr(w, http.StatusBadRequest, `"pattern" is required: comma-separated on/off durations in microseconds, e.g. "9000,4500,560,560"`)
		return
	}
	args := []string{}
	if body.FreqHz > 0 {
		args = append(args, "-f", strconv.Itoa(body.FreqHz))
	}
	args = append(args, "-p", body.Pattern)
	if body.Repeat > 0 {
		args = append(args, "-r", strconv.Itoa(body.Repeat))
	}
	out, err := a.bridgeExec(ctx, 30*time.Second, "termux-infrared-transmit", args...)
	res := map[string]any{"ok": err == nil, "freq_hz": body.FreqHz, "pattern": body.Pattern, "output": firstLine(out)}
	if err != nil {
		res["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlMedia(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"` // play | pause | stop | next | prev | info
		File   string `json:"file"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	action := strings.ToLower(body.Action)
	args := []string{}
	switch action {
	case "play":
		if body.File == "" {
			writeErr(w, http.StatusBadRequest, `"file" is required to start playback`)
			return
		}
		args = []string{"play", body.File}
	case "pause":
		args = []string{"pause"}
	case "stop":
		args = []string{"stop"}
	case "info":
		args = []string{"info"}
	default:
		// next/prev are not in termux-media-player; media keys are the platform
		// way to reach a player that owns a session.
		key := map[string]string{"next": "KEYCODE_MEDIA_NEXT", "prev": "KEYCODE_MEDIA_PREVIOUS",
			"previous": "KEYCODE_MEDIA_PREVIOUS", "playpause": "KEYCODE_MEDIA_PLAY_PAUSE"}[action]
		if key == "" {
			writeErr(w, http.StatusBadRequest, `"action" must be play, pause, stop, info, next, prev or playpause`)
			return
		}
		out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/input", "keyevent", key)
		res := map[string]any{"ok": err == nil, "keyevent": key, "method": "media key"}
		if err != nil {
			res["error"] = firstLine(out)
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	out, err := a.bridgeExec(ctx, 45*time.Second, "termux-media-player", args...)
	res := map[string]any{"ok": err == nil, "action": action, "output": out}
	if err != nil {
		res["error"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlWifi(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On   *bool  `json:"on"`
		SSID string `json:"ssid"`
	}
	if err := readJSONBody(r, &body, 1<<13); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res := map[string]any{}
	if body.On != nil {
		state := "disabled"
		if *body.On {
			state = "enabled"
		}
		out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/cmd", "wifi", "set-wifi-enabled", state)
		res["set"] = state
		res["ok"] = err == nil
		if err != nil {
			res["error"] = firstLine(out)
			if fallback, ferr := a.bridgeExec(ctx, 25*time.Second, "termux-wifi-enable", map[bool]string{true: "true", false: "false"}[*body.On]); ferr == nil {
				res["fallback"] = "termux-wifi-enable"
				res["fallback_output"] = firstLine(fallback)
				res["ok"] = true
				delete(res, "error")
			}
		}
		time.Sleep(1 * time.Second)
	}
	res["wifi"] = a.collectWifi()
	if body.SSID != "" {
		out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/cmd", "wifi", "connect-network", body.SSID, "open")
		res["connect"] = map[string]any{"ok": err == nil, "ssid": body.SSID, "output": firstLine(out)}
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleControlSensors(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	q := r.URL.Query()
	if s := q.Get("sensor"); s != "" {
		n := 1
		if v := q.Get("n"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
				n = minInt(parsed, 50)
			}
		}
		out, err := a.bridgeExec(ctx, 50*time.Second, "termux-sensor", "-s", s, "-n", strconv.Itoa(n))
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sensor": s, "samples": n, "readings": out})
		return
	}
	out, err := a.bridgeExec(ctx, 30*time.Second, "termux-sensor", "-l")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error(),
			"hint": "sensor readings need the Termux:API bridge; POST /api/v1/bridge/fix reports what is missing"})
		return
	}
	var list any
	json.Unmarshal([]byte(out), &list)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sensors": list, "raw": firstNonEmpty(jsonOrEmpty(list), "")})
}

func jsonOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return "parsed"
}

func (a *App) handleControlLocation(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	args := []string{}
	if p := r.URL.Query().Get("provider"); p != "" {
		args = append(args, "-p", p)
	}
	if req := r.URL.Query().Get("request"); req != "" {
		args = append(args, "-r", req)
	}
	out, err := a.bridgeExec(ctx, 75*time.Second, "termux-location", args...)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var parsed any
	json.Unmarshal([]byte(out), &parsed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "location": parsed, "raw": out})
}

func (a *App) handleControlAudio(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	vols, src := a.audioVolumes(ctx)
	out, _ := a.runCmd(ctx, 20*time.Second, "/system/bin/dumpsys", "audio")
	active := []string{}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "- STREAM_") || strings.HasPrefix(l, "Stream volumes") ||
			strings.Contains(l, "mode:") && strings.Contains(l, "STREAM") {
			active = append(active, l)
		}
		if len(active) >= 30 {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volumes": vols, "volumes_source": src, "streams": active,
		"note": "webui/most consumers want /api/v1/control/volume to change them",
	})
}

// ---------- gated: shell and power ----------

func (a *App) handleShell(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Security.AllowShell {
		writeErr(w, http.StatusForbidden,
			"the shell endpoint is disabled. Set security.allow_shell=true to enable it (it runs as root on the phone)")
		return
	}
	var body struct {
		Cmd       string `json:"cmd"`
		TimeoutMS int    `json:"timeout_ms"`
		MaxBytes  int    `json:"max_output_bytes"`
	}
	if err := readJSONBody(r, &body, 1<<18); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Cmd) == "" {
		writeErr(w, http.StatusBadRequest, `"cmd" is required`)
		return
	}
	start := time.Now()
	out, code, ms, truncated, err := a.runShell(r.Context(), body.Cmd, body.TimeoutMS, body.MaxBytes)
	_ = start
	a.logf.Printf("shell: cmd=%q code=%d ms=%d bytes=%d", body.Cmd, code, ms, len(out))
	res := map[string]any{
		"ok":          err == nil && code == 0,
		"cmd":         body.Cmd,
		"exit_code":   code,
		"duration_ms": ms,
		"output":      out,
		"truncated":   truncated,
	}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleDeviceReboot(w http.ResponseWriter, r *http.Request) {
	if !a.config().Security.AllowPowerCommands {
		writeErr(w, http.StatusForbidden,
			"power commands are disabled. Set security.allow_power_commands=true to enable reboot/shutdown")
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := readJSONBody(r, &body, 1<<12); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	switch mode {
	case "", "normal", "system", "android":
		mode = ""
	case "recovery", "bootloader", "fastboot", "sideload", "sideload-auto-reboot", "edl":
		if mode == "fastboot" {
			mode = "bootloader"
		}
	default:
		writeErr(w, http.StatusBadRequest, `"mode" must be empty, recovery, bootloader, sideload or edl`)
		return
	}
	args := []string{"power", "reboot"}
	if mode != "" {
		args = append(args, mode)
	}
	a.logf.Printf("device/reboot requested (mode=%q)", mode)
	out, err := a.runCmd(r.Context(), 15*time.Second, "/system/bin/svc", args...)
	res := map[string]any{"ok": err == nil, "mode": firstNonEmpty(mode, "normal"), "issued": strings.Join(args[1:], " ")}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleDeviceShutdown(w http.ResponseWriter, r *http.Request) {
	if !a.config().Security.AllowPowerCommands {
		writeErr(w, http.StatusForbidden,
			"power commands are disabled. Set security.allow_power_commands=true to enable reboot/shutdown")
		return
	}
	a.logf.Printf("device/shutdown requested")
	out, err := a.runCmd(r.Context(), 15*time.Second, "/system/bin/svc", "power", "shutdown")
	res := map[string]any{"ok": err == nil}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}
