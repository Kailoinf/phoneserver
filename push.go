package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PushEnvelope is what lands on the cloud endpoint.
type PushEnvelope struct {
	DeviceID    string   `json:"device_id"`
	Seq         int64    `json:"seq"`
	CollectedAt string   `json:"collected_at"`
	Payload     Snapshot `json:"payload"`
}

// PushResult is both the return value and the recorded state of a push.
type PushResult struct {
	OK              bool   `json:"ok"`
	Trigger         string `json:"trigger"`
	URL             string `json:"url"`
	Attempts        int    `json:"attempts"`
	StatusCode      int    `json:"http_status,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	BytesSent       int    `json:"bytes_sent,omitempty"`
	LastSuccess     string `json:"last_success,omitempty"`
	LastAttempt     string `json:"last_attempt,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	ResponseSnippet string `json:"response_snippet,omitempty"`
}

type pusher struct {
	app *App
	log *log.Logger

	mu           sync.Mutex
	seq          int64
	last         PushResult
	lastSuccess  string
	successCount int64
	failCount    int64
	lastError    string
}

func newPusher(a *App, l *log.Logger) *pusher {
	return &pusher{app: a, log: l}
}

// loop pushes on the configured interval. The interval is re-read every tick so
// a config change takes effect without a restart.
func (p *pusher) loop() {
	for {
		cfg := p.app.config()
		iv := cfg.Push.Interval
		if iv < 10 {
			iv = 10
		}
		time.Sleep(time.Duration(iv) * time.Second)
		cfg = p.app.config()
		if !cfg.Push.Enabled || strings.TrimSpace(cfg.Push.URL) == "" {
			continue
		}
		res := p.pushOnce("scheduled")
		if !res.OK {
			p.log.Printf("push failed: %s", res.LastError)
		}
	}
}

func (p *pusher) nextSeq() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return p.seq
}

func (p *pusher) pushOnce(trigger string) PushResult {
	cfg := p.app.config()
	url := strings.TrimSpace(cfg.Push.URL)
	res := PushResult{Trigger: trigger, URL: url, LastAttempt: time.Now().Format(time.RFC3339)}
	if url == "" {
		res.LastError = "push.url is empty — set it with PUT /api/v1/config"
		p.record(res)
		return res
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		res.LastError = "push.url must start with http:// or https://"
		p.record(res)
		return res
	}

	env := PushEnvelope{
		DeviceID:    p.app.deviceID(),
		Seq:         p.nextSeq(),
		CollectedAt: time.Now().Format(time.RFC3339),
		Payload:     p.app.snapshot(),
	}
	body, err := json.Marshal(env)
	if err != nil {
		res.LastError = "marshal: " + err.Error()
		p.record(res)
		return res
	}

	timeout := cfg.Push.TimeoutS
	if timeout <= 0 {
		timeout = 10
	}
	retries := cfg.Push.Retries
	if retries <= 0 {
		retries = 1
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	var lastErr string
	start := time.Now()
	for attempt := 1; attempt <= retries; attempt++ {
		res.Attempts = attempt
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if rerr != nil {
			cancel()
			lastErr = "build request: " + rerr.Error()
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "phoneserver/"+version)
		req.Header.Set("X-Device-Id", env.DeviceID)
		req.Header.Set("X-Seq", fmt.Sprintf("%d", env.Seq))
		if cfg.Push.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Push.BearerToken)
		}
		for k, v := range cfg.Push.ExtraHeaders {
			req.Header.Set(k, v)
		}

		resp, derr := client.Do(req)
		if derr != nil {
			cancel()
			lastErr = "post: " + derr.Error()
		} else {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			cancel()
			res.StatusCode = resp.StatusCode
			res.ResponseSnippet = strings.TrimSpace(string(snippet))
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				res.OK = true
				res.BytesSent = len(body)
				res.DurationMS = time.Since(start).Milliseconds()
				res.LastError = ""
				p.record(res)
				p.log.Printf("push ok: seq=%d status=%d bytes=%d in %dms",
					env.Seq, resp.StatusCode, len(body), res.DurationMS)
				return res
			}
			lastErr = fmt.Sprintf("http %d: %s", resp.StatusCode, res.ResponseSnippet)
		}
		if attempt < retries {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	res.DurationMS = time.Since(start).Milliseconds()
	res.LastError = lastErr
	p.record(res)
	return res
}

func (p *pusher) record(res PushResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = res
	if res.OK {
		p.lastSuccess = res.LastAttempt
		p.successCount++
	} else {
		p.failCount++
		p.lastError = res.LastError
	}
}

func (p *pusher) status() map[string]any {
	cfg := p.app.config()
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{
		"enabled":          cfg.Push.Enabled,
		"configured":       strings.TrimSpace(cfg.Push.URL) != "",
		"url":              cfg.Push.URL,
		"interval_seconds": cfg.Push.Interval,
		"timeout_seconds":  cfg.Push.TimeoutS,
		"retries":          cfg.Push.Retries,
		"auth":             cfg.Push.BearerToken != "",
		"seq_counter":      p.seq,
		"success_count":    p.successCount,
		"failure_count":    p.failCount,
		"last_success":     p.lastSuccess,
		"last_error":       p.lastError,
		"last":             p.last,
		"how_to_configure": "PUT /api/v1/config  {\"push\":{\"enabled\":true,\"url\":\"https://host/ingest\",\"interval_seconds\":60}}",
	}
}
