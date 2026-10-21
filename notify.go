package main

// notify.go — notifications, and the device's own message stores.
//
// Notifications are read from the platform's notification service dump rather
// than through an app: `dumpsys notification --noredact` gives every posted
// notification with its title, text, package, uid, channel, flags and post
// time, and it works as root with nothing installed. That is why this module
// needs no listener permission and no Termux:API for the notification feed.
//
// The dump is a human-readable report, not a stable API, so the parser is
// written defensively: a record it cannot read is reported as
// `parse_incomplete` with the lines it did understand, and ?raw=1 hands back
// the untouched text for whoever wants to look. An empty list is never
// reported as "no notifications" unless the dump really contained none.
//
// Message stores (SMS, call log, contacts) are queried through `content query`
// as root. On a device with no SIM the SMS and call tables are simply empty;
// the response says rows:0 and why instead of inventing a failure.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notification is one posted notification, cleaned up.
type Notification struct {
	Key               string `json:"key"`
	Package           string `json:"package"`
	UID               int    `json:"uid,omitempty"`
	User              string `json:"user,omitempty"`
	ID                int    `json:"id,omitempty"`
	Tag               string `json:"tag,omitempty"`
	Channel           string `json:"channel,omitempty"`
	Title             string `json:"title,omitempty"`
	Text              string `json:"text,omitempty"`
	BigText           string `json:"big_text,omitempty"`
	Ticker            string `json:"ticker_text,omitempty"`
	SubText           string `json:"sub_text,omitempty"`
	WhenMS            int64  `json:"when_ms,omitempty"`
	When              string `json:"when,omitempty"`
	Ongoing           bool   `json:"ongoing"`
	Clearable         bool   `json:"clearable"`
	ForegroundService bool   `json:"foreground_service"`
	GroupSummary      bool   `json:"group_summary"`
	GroupKey          string `json:"group_key,omitempty"`
	Importance        string `json:"importance,omitempty"`
	Visibility        string `json:"visibility,omitempty"`
	Flags             string `json:"flags,omitempty"`
	Category          string `json:"category,omitempty"`
	Progress          int    `json:"progress,omitempty"`
	ProgressMax       int    `json:"progress_max,omitempty"`
	Actions           []string `json:"actions,omitempty"`
	ParseIncomplete   bool     `json:"parse_incomplete,omitempty"`
}

type notifyDump struct {
	mu   sync.Mutex
	at   time.Time
	raw  string
	recs []Notification
	err  string
}

var notifyCache notifyDump

var (
	reNotifRecord = regexp.MustCompile(`^\s*NotificationRecord\(0x[0-9a-f]+: pkg=(\S+) user=(\S+) id=(-?\d+) tag=(\S+) importance=(\d+) key=(.*?) bbbc=.*?Notification\(channel=(.*?) shortcut=`)
	reNotifKey    = regexp.MustCompile(`^\s+key=(\S+)\s*$`)
	reNotifUID    = regexp.MustCompile(`^\s+uid=(\d+) userId=(-?\d+)`)
	reNotifFlags  = regexp.MustCompile(`^\s+(?:original)?[fF]lags=(.*)$`)
	reNotifKeyAlt = regexp.MustCompile(`^\s+(?:group)?[kK]ey=(.*)$`)
	reNotifWhen   = regexp.MustCompile(`^\s+when=(-?\d+)/(-?\d+)`)
	reNotifTicker = regexp.MustCompile(`^\s+tickerText=(.*)$`)
	reNotifVis    = regexp.MustCompile(`^\s+vis=(\d+|PRIVATE|PUBLIC|SECRET)\s*$`)
	reNotifCat    = regexp.MustCompile(`^\s+category=(.*)$`)
	reExtrasStr   = regexp.MustCompile(`^\s+(android\.(?:title|text|bigText|subText|infoText|summaryText|progress|progressMax))=String \((.*)\)\s*$`)
	reExtrasChar  = regexp.MustCompile(`^\s+(android\.(?:title|text|bigText|subText|infoText|summaryText))=CharSequence \((.*)\)\s*$`)
	reExtrasNum   = regexp.MustCompile(`^\s+(android\.(?:progress|progressMax))=Integer \((\d+)\)`)
	reActionLine  = regexp.MustCompile(`^\s+\[(\d+)\] "(.*?)" ->`)
	reImportance  = regexp.MustCompile(`^\s+mImportance=(\S+)`)
	reFGS         = regexp.MustCompile(`^\s+isForegroundService=(true|false)`)
	reGroupSum    = regexp.MustCompile(`^\s+isGroupSummary=(true|false)`)
	reBigTextArr  = regexp.MustCompile(`^\s+android\.bigText=String \[(.*)\]\s*$`)
)

var importanceNames = map[string]string{
	"0": "unspecified", "1": "min", "2": "low", "3": "default", "4": "high", "5": "max",
}

var visibilityNames = map[string]string{
	"0": "secret", "1": "private", "2": "public",
}

// clearText unwraps the escaping dumpsys uses inside String (...).
func clearText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ")")
	if strings.HasPrefix(s, "String (") {
		s = strings.TrimPrefix(s, "String (")
	}
	return strings.TrimSpace(s)
}

func (a *App) notifications(ctx context.Context, force bool) ([]Notification, string, error) {
	cfg := a.config().Notify
	if !cfg.Enabled {
		return nil, "", fmt.Errorf("notifications are disabled in config (notify.enabled=false)")
	}
	notifyCache.mu.Lock()
	if !force && notifyCache.raw != "" && time.Since(notifyCache.at) < time.Duration(cfg.CacheSeconds)*time.Second {
		recs, raw := notifyCache.recs, notifyCache.raw
		notifyCache.mu.Unlock()
		return recs, raw, nil
	}
	notifyCache.mu.Unlock()

	raw, err := a.runCmd(ctx, 25*time.Second, "/system/bin/dumpsys", "notification", "--noredact")
	if err != nil && raw == "" {
		// --noredact is not accepted everywhere; fall back to the plain dump.
		raw, err = a.runCmd(ctx, 25*time.Second, "/system/bin/dumpsys", "notification")
	}
	if err != nil && raw == "" {
		return nil, "", fmt.Errorf("dumpsys notification failed: %v", err)
	}
	recs := parseNotifications(raw)
	notifyCache.mu.Lock()
	notifyCache.raw, notifyCache.recs, notifyCache.at = raw, recs, time.Now()
	notifyCache.mu.Unlock()
	return recs, raw, nil
}

// parseNotifications walks the "Notification List:" section line by line. A new
// NotificationRecord( starts a record; everything indented after it belongs to
// that record until the next one. Only the lines we understand are consumed —
// the rest of the report (stats, ranking, log) is ignored.
func parseNotifications(raw string) []Notification {
	lines := strings.Split(raw, "\n")
	inList := false
	var cur *Notification
	var out []Notification
	var sawExtras bool

	flush := func() {
		if cur != nil {
			cur.Ongoing = strings.Contains(cur.Flags, "ONGOING_EVENT") ||
				strings.Contains(cur.Flags, "FOREGROUND_SERVICE")
			cur.Clearable = !strings.Contains(cur.Flags, "NO_CLEAR")
			if strings.Contains(cur.Flags, "FOREGROUND_SERVICE") {
				cur.ForegroundService = true
			}
			if cur.Title == "" && cur.BigText != "" {
				cur.Title = firstNonEmptyLine(cur.BigText)
			}
			if cur.Text == "" && cur.BigText != "" {
				cur.Text = firstNonEmptyLine(cur.BigText)
			}
			if cur.WhenMS > 0 {
				cur.When = time.UnixMilli(cur.WhenMS).Format(time.RFC3339)
			}
			if cur.Title == "" && cur.Text == "" && cur.Ticker != "" {
				cur.Title = cur.Ticker
			}
			cur.ParseIncomplete = !sawExtras
			out = append(out, *cur)
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Notification List:") {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		// The list section ends at the next top-level report block.
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") &&
			!strings.HasPrefix(trimmed, "NotificationRecord(") {
			flush()
			cur = nil
			inList = false
			continue
		}
		if m := reNotifRecord.FindStringSubmatch(line); len(m) == 8 {
			flush()
			sawExtras = false
			id, _ := strconv.Atoi(m[3])
			imp := strings.TrimSpace(m[5])
			cur = &Notification{
				Package:    m[1],
				User:       m[2],
				ID:         id,
				Tag:        strings.TrimSuffix(m[4], ","),
				Importance: firstNonEmpty(importanceNames[m[5]], imp),
				Key:        strings.TrimSpace(m[6]),
				Channel:    m[7],
			}
			continue
		}
		if cur == nil {
			continue
		}
		if m := reNotifUID.FindStringSubmatch(line); len(m) == 3 {
			cur.UID, _ = strconv.Atoi(m[1])
			continue
		}
		if m := reNotifKeyAlt.FindStringSubmatch(line); len(m) == 2 {
			cur.GroupKey = strings.TrimSpace(m[1])
			if cur.Key == "" {
				cur.Key = cur.GroupKey
			}
			continue
		}
		if m := reNotifFlags.FindStringSubmatch(line); len(m) == 2 {
			cur.Flags = strings.TrimSpace(m[1])
			continue
		}
		if m := reNotifWhen.FindStringSubmatch(line); len(m) == 3 {
			cur.WhenMS, _ = strconv.ParseInt(strings.TrimSpace(m[1]), 10, 64)
			continue
		}
		if m := reNotifTicker.FindStringSubmatch(line); len(m) == 2 {
			cur.Ticker = clearText(m[1])
			continue
		}
		if m := reNotifVis.FindStringSubmatch(line); len(m) == 2 {
			cur.Visibility = firstNonEmpty(visibilityNames[m[1]], m[1])
			continue
		}
		if m := reNotifCat.FindStringSubmatch(line); len(m) == 2 {
			cur.Category = strings.TrimSpace(m[1])
			continue
		}
		if m := reImportance.FindStringSubmatch(line); len(m) == 2 {
			cur.Importance = strings.TrimSpace(m[1])
			continue
		}
		if m := reFGS.FindStringSubmatch(line); len(m) == 2 {
			cur.ForegroundService = m[1] == "true"
			continue
		}
		if m := reGroupSum.FindStringSubmatch(line); len(m) == 2 {
			cur.GroupSummary = m[1] == "true"
			continue
		}
		if m := reExtrasStr.FindStringSubmatch(line); len(m) == 3 {
			sawExtras = true
			setExtra(cur, m[1], m[2])
			continue
		}
		if m := reExtrasChar.FindStringSubmatch(line); len(m) == 3 {
			sawExtras = true
			setExtra(cur, m[1], m[2])
			continue
		}
		if m := reExtrasNum.FindStringSubmatch(line); len(m) == 3 {
			sawExtras = true
			n, _ := strconv.Atoi(m[2])
			if strings.HasSuffix(m[1], "progressMax") {
				cur.ProgressMax = n
			} else {
				cur.Progress = n
			}
			continue
		}
		if m := reBigTextArr.FindStringSubmatch(line); len(m) == 2 {
			sawExtras = true
			cur.BigText = strings.TrimSpace(m[1])
			continue
		}
		if m := reActionLine.FindStringSubmatch(line); len(m) == 3 {
			cur.Actions = append(cur.Actions, m[2])
			continue
		}
	}
	flush()
	return out
}

func setExtra(n *Notification, key, val string) {
	v := clearText(val)
	switch key {
	case "android.title":
		n.Title = v
	case "android.text":
		n.Text = v
	case "android.bigText":
		n.BigText = v
	case "android.subText":
		n.SubText = v
	case "android.summaryText", "android.infoText":
		if n.SubText == "" {
			n.SubText = v
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func (a *App) handleNotifications(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	q := r.URL.Query()
	recs, raw, err := a.notifications(ctx, boolParam(q.Get("refresh")))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if boolParam(q.Get("raw")) {
		writeJSON(w, http.StatusOK, map[string]any{"raw": raw, "parsed": len(recs)})
		return
	}
	includeOngoing := a.config().Notify.IncludeOngoing || boolParam(q.Get("include_ongoing"))
	pkgFilter := q.Get("package")
	grep := strings.ToLower(q.Get("grep"))
	sinceMS := int64(0)
	if s := q.Get("since_ms"); s != "" {
		sinceMS, _ = strconv.ParseInt(s, 10, 64)
	}
	limit := a.config().Notify.Max
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	picked := make([]Notification, 0, len(recs))
	for _, n := range recs {
		if !includeOngoing && n.Ongoing {
			continue
		}
		if pkgFilter != "" && n.Package != pkgFilter {
			continue
		}
		if sinceMS > 0 && n.WhenMS < sinceMS {
			continue
		}
		if grep != "" {
			hay := strings.ToLower(n.Title + " " + n.Text + " " + n.BigText + " " + n.Package + " " + n.Channel)
			if !strings.Contains(hay, grep) {
				continue
			}
		}
		picked = append(picked, n)
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].WhenMS > picked[j].WhenMS })
	total := len(picked)
	if len(picked) > limit {
		picked = picked[:limit]
	}
	incomplete := 0
	pkgs := map[string]int{}
	for _, n := range picked {
		if n.ParseIncomplete {
			incomplete++
		}
		pkgs[n.Package]++
	}
	out := map[string]any{
		"count":             len(picked),
		"total_before_limit": total,
		"ongoing_included":  includeOngoing,
		"captured_at":       time.Now().Format(time.RFC3339),
		"packages":          pkgs,
		"notifications":     picked,
	}
	if incomplete > 0 {
		out["parse_incomplete"] = incomplete
		out["hint"] = "some records had no extras block (the notification body is often empty for ongoing/system entries); ?raw=1 shows the source"
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleNotificationPost(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var body struct {
		Title string `json:"title"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Channel string `json:"channel"`
		Style string `json:"style"` // bigtext | inbox
	}
	if err := readJSONBody(r, &body, 1<<14); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, `"text" is required (the notification body)`)
		return
	}
	args := []string{"notification", "post"}
	if body.Title != "" {
		args = append(args, "-t", body.Title)
	}
	if body.Style != "" {
		args = append(args, "-S", body.Style)
	}
	if body.Channel != "" {
		args = append(args, "-c", body.Channel)
	}
	tag := body.ID
	if tag == "" {
		tag = "phoneserver"
	}
	args = append(args, tag, body.Text)

	out, err := a.runCmd(ctx, 15*time.Second, "/system/bin/cmd", args...)
	res := map[string]any{"posted": err == nil, "tag": tag, "output": firstLine(out)}
	if err != nil {
		res["error"] = firstLine(out)
		writeJSON(w, http.StatusBadGateway, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- messages: SMS, call log, contacts ----------

var reContentRow = regexp.MustCompile(`^Row: (\d+) (.*)$`)

// contentQuery runs a `content query` and turns the Row: lines into records.
// Each record is a map of column->value; the caller decides what to surface.
func (a *App) contentQuery(ctx context.Context, uri string, projection []string, sortOrder string, limit int, where string) ([]map[string]string, error) {
	args := []string{"query", "--uri", uri}
	if len(projection) > 0 {
		args = append(args, "--projection", strings.Join(projection, ":"))
	}
	if where != "" {
		args = append(args, "--where", where)
	}
	if sortOrder != "" {
		args = append(args, "--sort", sortOrder)
	}
	out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/content", args...)
	if err != nil && out == "" {
		return nil, fmt.Errorf("content query %s: %v", uri, err)
	}
	rows := []map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "No result found") {
			continue
		}
		m := reContentRow.FindStringSubmatch(l)
		if len(m) != 3 {
			continue
		}
		rec := map[string]string{}
		for _, kv := range splitTopLevel(m[2]) {
			if i := strings.Index(kv, "="); i > 0 {
				rec[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
			}
		}
		rows = append(rows, rec)
		if limit > 0 && len(rows) >= limit {
			break
		}
	}
	return rows, nil
}

// splitTopLevel splits "a=1, b=2" without breaking on commas inside values that
// the provider quoted.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i, r := range s {
		switch r {
		case '\'':
			inQuote = !inQuote
		case '{', '[':
			if !inQuote {
				depth++
			}
		case '}', ']':
			if !inQuote && depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 && !inQuote {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

type messageSource struct {
	name       string
	uri        string
	projection []string
	sort       string
}

func (a *App) messageSources() map[string]messageSource {
	return map[string]messageSource{
		"sms": {
			name: "sms",
			uri:  "content://sms/",
			projection: []string{
				"_id", "thread_id", "address", "person", "date", "date_sent", "read",
				"status", "type", "body", "subject", "service_center", "locked",
			},
			sort: "date DESC",
		},
		"calls": {
			name: "calls",
			uri:  "content://call_log/calls",
			projection: []string{
				"_id", "number", "date", "duration", "type", "name", "cached_name",
				"countryiso", "geocoded_location", "is_read", "presentation", "sim_id",
			},
			sort: "date DESC",
		},
		"contacts": {
			name: "contacts",
			uri:  "content://com.android.contacts/data/phones",
			projection: []string{
				"_id", "display_name", "data1", "data2", "data3", "contact_id",
				"account_type", "mimetype",
			},
			sort: "display_name ASC",
		},
	}
}

// smsTypeName maps the provider's type column to something readable.
func smsTypeName(t string) string {
	switch t {
	case "1":
		return "inbox"
	case "2":
		return "sent"
	case "3":
		return "draft"
	case "4":
		return "outbox"
	case "5":
		return "failed"
	case "6":
		return "queued"
	}
	return t
}

func callTypeName(t string) string {
	switch t {
	case "1":
		return "incoming"
	case "2":
		return "outgoing"
	case "3":
		return "missed"
	case "4":
		return "voicemail"
	case "5":
		return "rejected"
	case "6":
		return "blocked"
	case "7":
		return "answered_externally"
	}
	return t
}

func (a *App) handleMessagesList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	q := r.URL.Query()
	srcName := strings.ToLower(strings.TrimSpace(q.Get("source")))
	sources := a.messageSources()
	if srcName == "" {
		srcName = "sms"
	}
	src, ok := sources[srcName]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown source; use sms, calls or contacts")
		return
	}
	limit := 50
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = minInt(n, 1000)
		}
	}
	rows, err := a.contentQuery(ctx, src.uri, src.projection, src.sort, limit, q.Get("where"))
	out := map[string]any{
		"source":   srcName,
		"uri":      src.uri,
		"count":    len(rows),
		"messages": []any{},
	}
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusBadGateway, out)
		return
	}
	cleaned := make([]map[string]string, 0, len(rows))
	for _, rec := range rows {
		switch srcName {
		case "sms":
			if v, ok := rec["date"]; ok {
				if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
					rec["date_iso"] = time.UnixMilli(ms).Format(time.RFC3339)
				}
			}
			if v, ok := rec["type"]; ok {
				rec["type_name"] = smsTypeName(v)
			}
		case "calls":
			if v, ok := rec["date"]; ok {
				if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
					rec["date_iso"] = time.UnixMilli(ms).Format(time.RFC3339)
				}
			}
			if v, ok := rec["type"]; ok {
				rec["type_name"] = callTypeName(v)
			}
			if v, ok := rec["duration"]; ok {
				if sec, err := strconv.Atoi(v); err == nil {
					rec["duration_seconds"] = strconv.Itoa(sec)
				}
			}
		}
		cleaned = append(cleaned, rec)
	}
	out["messages"] = cleaned
	// A device with no SIM has an empty SMS table; say that instead of making
	// the caller guess whether the query worked.
	if len(cleaned) == 0 {
		prop := ""
		if p, perr := a.props(); perr == nil {
			prop = p["gsm.sim.state"]
		}
		out["empty_reason"] = "the provider returned no rows (this phone reports no SIM inserted: " +
			firstNonEmpty(prop, "gsm.sim.state unset") + ")"
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleMessagesSearch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	q := r.URL.Query()
	term := strings.TrimSpace(q.Get("q"))
	srcName := strings.ToLower(strings.TrimSpace(q.Get("source")))
	if srcName == "" {
		srcName = "sms"
	}
	sources := a.messageSources()
	src, ok := sources[srcName]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown source; use sms, calls or contacts")
		return
	}
	where := ""
	if term != "" {
		col := "body"
		switch srcName {
		case "calls":
			col = "number"
		case "contacts":
			col = "display_name"
		}
		// The where clause is passed to the provider as-is; escape quotes so a
		// search term cannot break out of the SQL literal it lands in.
		where = fmt.Sprintf("%s LIKE '%%%s%%'", col, strings.ReplaceAll(term, "'", "''"))
	}
	limit := 50
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = minInt(n, 1000)
		}
	}
	rows, err := a.contentQuery(ctx, src.uri, src.projection, src.sort, limit, where)
	res := map[string]any{"source": srcName, "query": term, "where": where, "count": len(rows), "messages": rows}
	if err != nil {
		res["error"] = err.Error()
	}
	if b, merr := json.Marshal(rows); merr == nil {
		res["bytes"] = len(b)
	}
	writeJSON(w, http.StatusOK, res)
}