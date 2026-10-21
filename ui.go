package main

// ui.go — reading the screen's view hierarchy, and clicking on what is in it.
//
// Everything here is built on the platform's own uiautomator dump: the daemon
// asks for the hierarchy to a file in /data/local/tmp, parses the XML, and
// turns it into a flat, addressable list of elements. That makes a "tap this
// button" instruction expressible as "tap this text", which survives layout
// changes that would break hard-coded coordinates.
//
// Two honest limits:
//   - a network dump costs ~2 s on this device (uiautomator starts a Java
//     process), so the result is cached for a short while.
//   - while the phone is locked or the screen is off, the dump contains only
//     keyguard nodes; the response says so instead of pretending the app has no
//     text.

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const uiDumpPath = "/data/local/tmp/phoneserver_ui.xml"

// UIElement is one node of the hierarchy, flattened.
type UIElement struct {
	Index         int    `json:"index"`      // depth-first position in the tree
	Depth         int    `json:"depth"`      //
	Class         string `json:"class"`      //
	Package       string `json:"package"`    //
	Text          string `json:"text"`       //
	Desc          string `json:"desc"`       // content-desc
	ResourceID    string `json:"resource_id"`//
	Hint          string `json:"hint,omitempty"`
	Bounds        [4]int `json:"bounds"`     // left, top, right, bottom
	Center        [2]int `json:"center"`     // click point
	Width         int    `json:"width"`      //
	Height        int    `json:"height"`     //
	Clickable     bool   `json:"clickable"`  //
	LongClickable bool   `json:"long_clickable"`
	Scrollable    bool   `json:"scrollable"`
	Checkable     bool   `json:"checkable"`
	Checked       bool   `json:"checked"`
	Enabled       bool   `json:"enabled"`
	Focused       bool   `json:"focused"`
	Selected      bool   `json:"selected"`
	Password      bool   `json:"password"`
}

// Interesting reports whether an element is worth showing by default: it says
// something to a human or it can be touched.
func (e UIElement) interesting() bool {
	return e.Text != "" || e.Desc != "" || e.ResourceID != "" || e.Clickable
}

type uiDumpResult struct {
	Elements  []UIElement `json:"elements"`
	Total     int         `json:"total_nodes"`
	Rotation  string      `json:"rotation"`
	Package   string      `json:"package,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
	DumpMS    int64       `json:"dump_ms"`
	CachedAt  string      `json:"cached_at,omitempty"`
}

type uiCache struct {
	mu   sync.Mutex
	at   time.Time
	xml  []byte
	ms   int64
	err  string
}

var uiCacheStore = uiCache{}

// xNode mirrors one <node> element. Attributes that uiautomator always writes
// are strings here; they are converted once, on the way into UIElement.
type xNode struct {
	Index         string  `xml:"index,attr"`
	Text          string  `xml:"text,attr"`
	ResourceID    string  `xml:"resource-id,attr"`
	Class         string  `xml:"class,attr"`
	Package       string  `xml:"package,attr"`
	ContentDesc   string  `xml:"content-desc,attr"`
	Hint          string  `xml:"hint,attr"`
	Checkable     string  `xml:"checkable,attr"`
	Checked       string  `xml:"checked,attr"`
	Clickable     string  `xml:"clickable,attr"`
	Enabled       string  `xml:"enabled,attr"`
	Focusable     string  `xml:"focusable,attr"`
	Focused       string  `xml:"focused,attr"`
	Scrollable    string  `xml:"scrollable,attr"`
	LongClickable string  `xml:"long-clickable,attr"`
	Password      string  `xml:"password,attr"`
	Selected      string  `xml:"selected,attr"`
	Bounds        string  `xml:"bounds,attr"`
	Nodes         []xNode `xml:"node"`
}

type xHierarchy struct {
	XMLName  xml.Name `xml:"hierarchy"`
	Rotation string   `xml:"rotation,attr"`
	Nodes    []xNode  `xml:"node"`
}

var boundsRe = regexp.MustCompile(`\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]`)

func parseBounds(s string) ([4]int, bool) {
	var b [4]int
	m := boundsRe.FindStringSubmatch(s)
	if len(m) != 5 {
		return b, false
	}
	for i := 0; i < 4; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return b, false
		}
		b[i] = n
	}
	return b, true
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

func truthy(s string) bool { return strings.TrimSpace(s) == "true" }

// dumpUI returns the parsed hierarchy, reusing a dump taken within `ttl`.
func (a *App) dumpUI(ctx context.Context, ttl time.Duration) (uiDumpResult, error) {
	uiCacheStore.mu.Lock()
	if uiCacheStore.xml != nil && time.Since(uiCacheStore.at) < ttl {
		raw := uiCacheStore.xml
		at := uiCacheStore.at
		ms := uiCacheStore.ms
		uiCacheStore.mu.Unlock()
		res, err := parseUIHierarchy(raw)
		if err == nil {
			res.CachedAt = at.Format(time.RFC3339)
			res.DumpMS = ms
		}
		return res, err
	}
	uiCacheStore.mu.Unlock()

	start := time.Now()
	out, err := a.runCmd(ctx, 25*time.Second, "/system/bin/uiautomator", "dump", uiDumpPath)
	if err != nil {
		// uiautomator writes to stdout as well; only a missing file is fatal.
		if _, serr := os.Stat(uiDumpPath); serr != nil {
			return uiDumpResult{}, fmt.Errorf("uiautomator dump: %s", firstLine(out))
		}
	}
	raw, rerr := os.ReadFile(uiDumpPath)
	if rerr != nil {
		return uiDumpResult{}, fmt.Errorf("read %s: %w", uiDumpPath, rerr)
	}
	ms := time.Since(start).Milliseconds()

	uiCacheStore.mu.Lock()
	uiCacheStore.xml, uiCacheStore.at, uiCacheStore.ms = raw, time.Now(), ms
	uiCacheStore.mu.Unlock()

	res, perr := parseUIHierarchy(raw)
	if perr != nil {
		return res, perr
	}
	res.DumpMS = ms
	return res, nil
}

func parseUIHierarchy(raw []byte) (uiDumpResult, error) {
	var h xHierarchy
	if err := xml.Unmarshal(raw, &h); err != nil {
		return uiDumpResult{}, fmt.Errorf("parse hierarchy: %w", err)
	}
	res := uiDumpResult{Rotation: h.Rotation}
	var walk func(nodes []xNode, depth int)
	walk = func(nodes []xNode, depth int) {
		for i := range nodes {
			n := &nodes[i]
			res.Total++
			b, ok := parseBounds(n.Bounds)
			if !ok {
				b = [4]int{}
			}
			el := UIElement{
				Index:         len(res.Elements),
				Depth:         depth,
				Class:         n.Class,
				Package:       n.Package,
				Text:          n.Text,
				Desc:          n.ContentDesc,
				ResourceID:    n.ResourceID,
				Hint:          n.Hint,
				Bounds:        b,
				Center:        [2]int{(b[0] + b[2]) / 2, (b[1] + b[3]) / 2},
				Width:         b[2] - b[0],
				Height:        b[3] - b[1],
				Clickable:     truthy(n.Clickable),
				LongClickable: truthy(n.LongClickable),
				Scrollable:    truthy(n.Scrollable),
				Checkable:     truthy(n.Checkable),
				Checked:       truthy(n.Checked),
				Enabled:       truthy(n.Enabled),
				Focused:       truthy(n.Focused),
				Selected:      truthy(n.Selected),
				Password:      truthy(n.Password),
			}
			if res.Package == "" && el.Package != "" {
				res.Package = el.Package
			}
			res.Elements = append(res.Elements, el)
			if len(n.Nodes) > 0 {
				walk(n.Nodes, depth+1)
			}
		}
	}
	walk(h.Nodes, 0)
	return res, nil
}

// windowState is the cheap "what is on screen" answer that does not need
// uiautomator at all.
type windowState struct {
	FocusedWindow  string `json:"focused_window"`
	ForegroundApp  string `json:"foreground_app"`
	TopActivity    string `json:"top_activity,omitempty"`
	Keyguard       bool   `json:"keyguard_showing"`
	ScreenOn       bool   `json:"screen_on"`
	LayoutReady    bool   `json:"ui_automatable"`
	Note           string `json:"note,omitempty"`
}

func (a *App) windowState(ctx context.Context) windowState {
	ws := windowState{}
	if out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/dumpsys", "window"); err == nil || out != "" {
		for _, line := range strings.Split(out, "\n") {
			l := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(l, "mCurrentFocus="):
				ws.FocusedWindow = strings.TrimSpace(strings.TrimPrefix(l, "mCurrentFocus="))
			case strings.HasPrefix(l, "mFocusedApp="):
				ws.ForegroundApp = focusedPkg(l)
			case strings.Contains(l, "isKeyguardShowing="):
				ws.Keyguard = strings.Contains(l, "true")
			case strings.Contains(l, "mShowingLockscreen="):
				ws.Keyguard = ws.Keyguard || strings.Contains(l, "true")
			}
		}
	}
	if out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/dumpsys", "activity", "activities"); err == nil || out != "" {
		for _, line := range strings.Split(out, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "topResumedActivity=") || strings.Contains(l, "mResumedActivity:") {
				if i := strings.Index(l, "u0 "); i >= 0 {
					ws.TopActivity = strings.TrimSpace(l[i+3:])
				}
			}
		}
	}
	if d := a.collectDisplay(); true {
		ws.ScreenOn = d.ScreenOn
	}
	ws.LayoutReady = !ws.Keyguard && ws.ScreenOn
	if !ws.ScreenOn {
		ws.Note = "the screen is off; a hierarchy dump would only return keyguard nodes — wake the display first (POST /api/v1/control/screen)"
	} else if ws.Keyguard {
		ws.Note = "the lock screen is up; unlock the phone for app-level automation"
	}
	return ws
}

var pkgFromRecordRe = regexp.MustCompile(`u0 ([A-Za-z0-9_.]+)/`)

func focusedPkg(line string) string {
	if m := pkgFromRecordRe.FindStringSubmatch(line); len(m) == 2 {
		return m[1]
	}
	// mFocusedApp=ActivityRecord{... com.x/.MainActivity t5}
	fields := strings.Fields(line)
	for _, f := range fields {
		if strings.Contains(f, "/") && !strings.HasPrefix(f, "}") {
			return strings.SplitN(f, "/", 2)[0]
		}
	}
	return ""
}

// ---------- handlers ----------

func (a *App) handleUIDump(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	q := r.URL.Query()
	ttl := 1500 * time.Millisecond
	if v := q.Get("max_age_ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ttl = time.Duration(n) * time.Millisecond
		}
	}
	res, err := a.dumpUI(ctx, ttl)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ws := a.windowState(ctx)
	res.Warnings = nil
	if ws.Note != "" {
		res.Warnings = append(res.Warnings, ws.Note)
	}
	if !ws.LayoutReady {
		res.Warnings = append(res.Warnings, "this dump may only contain keyguard/launcher nodes; the useful ones appear once the phone is awake and unlocked")
	}
	if q.Get("all") == "1" {
		writeJSON(w, http.StatusOK, map[string]any{"total_nodes": res.Total, "rotation": res.Rotation, "dump_ms": res.DumpMS, "elements": res.Elements, "window": ws})
		return
	}
	filtered := make([]UIElement, 0, len(res.Elements))
	for _, e := range res.Elements {
		if e.interesting() {
			filtered = append(filtered, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_nodes":       res.Total,
		"interesting_nodes": len(filtered),
		"rotation":          res.Rotation,
		"dump_ms":           res.DumpMS,
		"cached_at":         res.CachedAt,
		"window":            ws,
		"warnings":          res.Warnings,
		"elements":          filtered,
	})
}

func (a *App) handleUICurrent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out := map[string]any{"window": a.windowState(ctx)}
	if v, err := a.runCmd(ctx, 10*time.Second, "/system/bin/dumpsys", "input_method"); err == nil || v != "" {
		out["keyboard_shown"] = strings.Contains(v, "mInputShown=true")
		out["keyboard_target"] = firstMatch(v, `mCurFocusedWindow=(\S+)`)
	}
	if v, err := a.runCmd(ctx, 10*time.Second, "/system/bin/dumpsys", "power"); err == nil || v != "" {
		for _, line := range strings.Split(v, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "mWakefulness=") || strings.HasPrefix(l, "Display Power: state=") {
				out[strings.SplitN(l, "=", 2)[0]] = strings.TrimSpace(strings.SplitN(l, "=", 2)[1])
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

var reCache = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}

func cachedRe(pat string) *regexp.Regexp {
	reCache.Lock()
	defer reCache.Unlock()
	if r, ok := reCache.m[pat]; ok {
		return r
	}
	r := regexp.MustCompile(pat)
	reCache.m[pat] = r
	return r
}

func firstMatch(s, pat string) string {
	m := cachedRe(pat).FindStringSubmatch(s)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// uiQuery describes a text/desc/id search.
type uiQuery struct {
	Text      string `json:"text"`
	Desc      string `json:"desc"`
	ResourceID string `json:"resource_id"`
	Class     string `json:"class"`
	Contains  bool   `json:"contains"`
	Clickable bool   `json:"clickable_only"`
}

func uiQueryFromValues(v map[string]string) uiQuery {
	return uiQuery{
		Text:      v["text"],
		Desc:      v["desc"],
		ResourceID: v["resource_id"],
		Class:     v["class"],
		Contains:  boolParam(v["contains"]) || boolParam(v["fuzzy"]),
		Clickable: boolParam(v["clickable"]),
	}
}

func (q uiQuery) matches(e UIElement) bool {
	match := func(want, got string) bool {
		if want == "" {
			return true
		}
		if q.Contains {
			return strings.Contains(strings.ToLower(got), strings.ToLower(want))
		}
		return got == want
	}
	if !match(q.Text, e.Text) || !match(q.Desc, e.Desc) || !match(q.ResourceID, e.ResourceID) {
		return false
	}
	if q.Class != "" {
		if q.Contains {
			if !strings.Contains(strings.ToLower(e.Class), strings.ToLower(q.Class)) {
				return false
			}
		} else if e.Class != q.Class {
			return false
		}
	}
	if q.Clickable {
		// An element's own clickable flag is often false on the text inside a
		// button, so accept an unclickable match that has a clickable ancestor
		// in the same bounds only when the caller asked for clickability and we
		// cannot find anything better. The caller side handles that fallback.
		return e.Clickable
	}
	return true
}

// findElements returns matches, and — when a clickable-only search finds
// nothing — the same matches without the clickable requirement, so the caller
// can fall back to tapping the centre coordinates anyway (input taps land on
// whatever is on top at that point, which is usually the button).
func (a *App) findElements(ctx context.Context, q uiQuery) ([]UIElement, uiDumpResult, error) {
	res, err := a.dumpUI(ctx, 1500*time.Millisecond)
	if err != nil {
		return nil, res, err
	}
	var hits []UIElement
	for _, e := range res.Elements {
		if q.matches(e) {
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 && q.Clickable {
		relaxed := q
		relaxed.Clickable = false
		for _, e := range res.Elements {
			if relaxed.matches(e) {
				hits = append(hits, e)
			}
		}
	}
	return hits, res, nil
}

func (a *App) handleUIFind(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	q := uiQueryFromValues(map[string]string{
		"text": r.URL.Query().Get("text"), "desc": r.URL.Query().Get("desc"),
		"resource_id": r.URL.Query().Get("resource_id"), "class": r.URL.Query().Get("class"),
		"contains": r.URL.Query().Get("contains"), "clickable": r.URL.Query().Get("clickable"),
	})
	if q.Text == "" && q.Desc == "" && q.ResourceID == "" && q.Class == "" {
		writeErr(w, http.StatusBadRequest, "give at least one of text, desc, resource_id, class")
		return
	}
	hits, res, err := a.findElements(ctx, q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	first := boolParam(r.URL.Query().Get("first"))
	if first && len(hits) > 1 {
		hits = hits[:1]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":        q,
		"match_count":  len(hits),
		"dump_ms":      res.DumpMS,
		"total_nodes":  res.Total,
		"matches":      hits,
		"window":       a.windowState(ctx),
	})
}

func (a *App) handleUIClick(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	var body struct {
		Text       string `json:"text"`
		Desc       string `json:"desc"`
		ResourceID string `json:"resource_id"`
		Class      string `json:"class"`
		Contains   bool   `json:"contains"`
		Index      int    `json:"index"`
		LongPress  bool   `json:"long_press"`
		DurationMS int    `json:"duration_ms"`
	}
	if err := readJSONBody(r, &body, 1<<14); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	q := uiQuery{Text: body.Text, Desc: body.Desc, ResourceID: body.ResourceID, Class: body.Class, Contains: body.Contains}
	if q.Text == "" && q.Desc == "" && q.ResourceID == "" && q.Class == "" {
		writeErr(w, http.StatusBadRequest, "give at least one of text, desc, resource_id, class")
		return
	}
	hits, _, err := a.findElements(ctx, q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(hits) == 0 {
		writeErr(w, http.StatusNotFound, "no element matched; GET /api/v1/ui/find with the same query shows what is on screen")
		return
	}
	if body.Index < 0 || body.Index >= len(hits) {
		body.Index = 0
	}
	target := hits[body.Index]
	x, y := target.Center[0], target.Center[1]
	dur := a.config().Control.DefaultTimeoutMS
	if body.DurationMS > 0 {
		dur = body.DurationMS
	}
	var out string
	if body.LongPress {
		out, err = a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "swipe",
			strconv.Itoa(x), strconv.Itoa(y), strconv.Itoa(x), strconv.Itoa(y), strconv.Itoa(maxInt(dur, 600)))
	} else {
		out, err = a.runCmd(ctx, a.inputTimeout(), "/system/bin/input", "tap", strconv.Itoa(x), strconv.Itoa(y))
	}
	res := map[string]any{"tapped": []int{x, y}, "element": target, "match_count": len(hits)}
	if err != nil {
		res["error"] = firstLine(out)
	}
	writeJSON(w, http.StatusOK, res)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}