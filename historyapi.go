// historyapi.go — HTTP surface for the local history database.
//
//	GET  /api/v1/history            samples query (metrics/since/until/limit)
//	GET  /api/v1/history/metrics    the queryable column names
//	GET  /api/v1/history/logs       stored log lines (grep/level/since)
//	GET  /api/v1/history/snapshot   the full document at/near a timestamp
//	GET  /api/v1/history/stats      sizes, counts, growth, battery-cost estimate
//	POST /api/v1/history/prune      run the retention policy now
package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseTimeParam accepts unix seconds (or millis), RFC3339 / "2006-01-02 15:04:05"
// / "2006-01-02", and relative offsets like -30m, -6h, -7d, -1w.
func parseTimeParam(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if strings.HasPrefix(s, "-") {
		if d, err := parseDur(s[1:]); err == nil {
			return time.Now().Add(-d).Unix()
		}
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		if v > 1e12 { // milliseconds
			return v / 1000
		}
		return v
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	return def
}

func parseDur(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	switch s[len(s)-1] {
	case 'd':
		n, err := strconv.Atoi(s[:len(s)-1])
		return time.Duration(n) * 24 * time.Hour, err
	case 'w':
		n, err := strconv.Atoi(s[:len(s)-1])
		return time.Duration(n) * 7 * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

func (a *App) historyUnavailable(w http.ResponseWriter) bool {
	if a.store == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"local history is not enabled (history.enabled=false or the database could not be opened; see GET /api/v1/history/stats)")
		return true
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handleHistory: GET /api/v1/history?metrics=battery_temp_c,cpu_usage_pct&since=-6h&until=now&limit=720&stats=1
func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	q := r.URL.Query()
	metrics := splitList(q.Get("metrics"))
	if len(metrics) == 0 {
		metrics = splitList(q.Get("metric"))
	}
	until := parseTimeParam(q.Get("until"), time.Now().Unix())
	since := parseTimeParam(q.Get("since"), until-6*3600)
	if since > until {
		since, until = until, since
	}
	limit := 720
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	withStats := q.Get("stats") == "1" || q.Get("stats") == "true"

	res, err := a.store.QuerySamples(metrics, since, until, limit, withStats)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": map[string]any{
			"metrics":     res.Columns[1:],
			"since":       since,
			"until":       until,
			"since_human": time.Unix(since, 0).Format("2006-01-02 15:04:05"),
			"until_human": time.Unix(until, 0).Format("2006-01-02 15:04:05"),
			"limit":       limit,
		},
		"count":     res.Count,
		"columns":   res.Columns,
		"points":    res.Points,
		"truncated": res.Truncated,
		"stats":     res.Stats,
		// Columns the cheap tick cannot measure (Wi-Fi, package counts, process
		// count, remaining charge time) are stored as NULL and carried forward
		// from the last full tick, so a series has no holes. Say which ones were
		// carried: a client should not mistake a flat stretch for real data at
		// full resolution.
		"carried_columns": res.Carried,
		"note": "points are [ts, metric...] oldest first; ts is a unix second. " +
			"Columns listed in carried_columns were measured on an earlier full tick",
	})
}

func (a *App) handleHistoryMetrics(w http.ResponseWriter, r *http.Request) {
	cols := SampleColumns()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":          len(cols),
		"metrics":        cols,
		"default":        []string{"battery_capacity_pct", "battery_temp_c", "cpu_usage_pct", "mem_used_pct", "max_temp_c"},
		"string_columns": []string{"battery_status", "battery_health", "wakefulness", "hottest_zone", "wifi_ssid"},
		"note":           "string columns are stored and returned by /api/v1/history/snapshot; the numeric set above is what /api/v1/history plots",
	})
}

// handleHistoryLogs: GET /api/v1/history/logs?since=-1h&limit=200&grep=access&level=error
func (a *App) handleHistoryLogs(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	q := r.URL.Query()
	untilMS := parseTimeParam(q.Get("until"), time.Now().Unix()) * 1000
	sinceMS := parseTimeParam(q.Get("since"), time.Now().Add(-1*time.Hour).Unix()) * 1000
	limit := 200
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	res, err := a.store.QueryLogs(sinceMS, untilMS, limit, q.Get("grep"), q.Get("level"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleHistorySnapshot: GET /api/v1/history/snapshot?ts=... (or latest) &section=battery
func (a *App) handleHistorySnapshot(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	q := r.URL.Query()
	ts := int64(0)
	if v := q.Get("ts"); v != "" && v != "latest" {
		ts = parseTimeParam(v, 0)
	}
	doc, storedTs, err := a.store.SnapshotDoc(ts)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if sec := q.Get("section"); sec != "" {
		val, ok := doc[sec]
		if !ok {
			writeErr(w, http.StatusNotFound, "no such section in the stored document: "+sec)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ts": storedTs, "section": sec, "value": val,
			"collected_at": doc["collected_at"],
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ts":        storedTs,
		"stored_at": time.Unix(storedTs, 0).Format("2006-01-02 15:04:05"),
		"requested": ts,
		"note":      "the stored snapshot whose timestamp is nearest to ts (latest when ts is omitted)",
		"document":  doc,
	})
}

func (a *App) handleHistoryStats(w http.ResponseWriter, r *http.Request) {
	st := a.historyStatus()
	if a.store == nil {
		st.LastError = "database not open (" + st.LastError + ")"
	}
	writeJSON(w, http.StatusOK, st)
}

// handleHistoryPrune runs retention immediately instead of waiting for the loop.
func (a *App) handleHistoryPrune(w http.ResponseWriter, r *http.Request) {
	if a.historyUnavailable(w) {
		return
	}
	h := a.config().History.norm()
	res, err := a.store.Prune(time.Now(), h)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"result":       res,
		"config":       h,
		"db_mb_before": round2(float64(res.DBBytesBefore) / (1 << 20)),
		"db_mb_after":  round2(float64(res.DBBytesAfter) / (1 << 20)),
	})
}
