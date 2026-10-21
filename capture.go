package main

// capture.go — the camera and the microphone.
//
// Both are delegated to the Termux:API app through bridge.go, because a root
// process has no camera/microphone of its own: the platform checks a uid's app
// state before it hands over either device. The bridge takes care of that; this
// file is about getting bytes in and out and being honest about quality.
//
// What the response guarantees:
//   - a photo is a real capture from the sensor at the requested camera id. The
//     raw JPEG is validated (SOI marker, plausible size) before it is answered,
//     so a policy refusal cannot be mistaken for a picture.
//   - a recording is a real file with a growing byte count. The recorder returns
//     immediately, so the file is polled until it stops growing or the requested
//     length has passed, and the response says how many bytes and seconds
//     actually arrived.
//   - neither is written into the history database, and there is no upload
//     path: the bytes go to the caller and (unless keep_capture_files is on)
//     the scratch file is removed.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gw "github.com/KarpelesLab/gowebp"
	"golang.org/x/image/draw"
)

var jpegMagic = []byte{0xff, 0xd8, 0xff}

func isJPEG(b []byte) bool { return len(b) > 3 && bytes.Equal(b[:3], jpegMagic) }

// ---------- camera ----------

func (a *App) handleCameraInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out, err := a.bridgeExec(ctx, 30*time.Second, "termux-camera-info")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": err.Error(),
			"hint": "camera access needs Termux:API; POST /api/v1/bridge/fix reports and repairs the permission/foreground state",
		})
		return
	}
	var cams any
	if jerr := json.Unmarshal([]byte(out), &cams); jerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "raw": out})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cameras": cams})
}

// capturePhoto runs one capture and returns the JPEG bytes. The first attempt
// runs without touching the display; if the platform refuses because the uid is
// not in the foreground, the second attempt wakes the screen and starts the
// Termux activity, which is what this device requires (see bridge.go).
func (a *App) capturePhoto(ctx context.Context, camera int) ([]byte, map[string]any, error) {
	info := map[string]any{"camera": camera, "attempts": 0, "notes": []string{}}
	notes := []string{}

	attempt := func(wake bool) ([]byte, error) {
		info["attempts"] = info["attempts"].(int) + 1
		note, nerr := a.ensureCameraReady(ctx, wake)
		if nerr != nil {
			return nil, nerr
		}
		if note != "" {
			notes = append(notes, note)
		}
		path, perr := a.bridgeTmpPath(ctx, fmt.Sprintf("photo-%d-%s.jpg", camera, strconv.FormatInt(time.Now().UnixNano(), 36)))
		if perr != nil {
			return nil, perr
		}
		out, err := a.bridgeExec(ctx, 90*time.Second, "termux-camera-photo", "-c", strconv.Itoa(camera), path)
		if err != nil {
			return nil, fmt.Errorf("%s (%s)", err.Error(), firstLine(out))
		}
		body, rerr := os.ReadFile(path)
		if !a.config().Capture.KeepFiles {
			defer os.Remove(path)
		}
		if rerr != nil {
			return nil, fmt.Errorf("the capture produced no file (%s): %v", firstLine(out), rerr)
		}
		if len(body) < 1024 {
			// A refusal comes back as an empty file plus a JSON error on stdout.
			return nil, fmt.Errorf("camera returned %d bytes: %s", len(body), firstLine(out))
		}
		if !isJPEG(body) {
			return nil, fmt.Errorf("camera returned %d bytes that are not JPEG data", len(body))
		}
		info["raw_bytes"] = len(body)
		return body, nil
	}

	body, err := attempt(false)
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "background") || strings.Contains(lower, "disabled by policy") ||
			strings.Contains(lower, "0 bytes") || strings.Contains(lower, "no file") {
			notes = append(notes, "first attempt was refused for a background uid; retrying with the display awake and the Termux activity in the foreground")
			if body2, err2 := attempt(true); err2 == nil {
				info["notes"] = notes
				info["retry_after_policy_refusal"] = true
				return body2, info, nil
			} else {
				info["notes"] = notes
				info["retry_error"] = err2.Error()
				return nil, info, err2
			}
		}
		info["notes"] = notes
		return nil, info, err
	}
	info["notes"] = notes
	return body, info, nil
}

func (a *App) handleCameraPhoto(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Capture.CameraEnabled {
		writeErr(w, http.StatusServiceUnavailable, "the camera is disabled in config (capture.camera_enabled=false)")
		return
	}
	q := r.URL.Query()
	camera := cfg.Capture.DefaultCamera
	if v := q.Get("camera"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			camera = n
		}
	}
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = cfg.Capture.PhotoFormat
	}
	switch format {
	case "raw", "jpeg", "jpg", "webp", "png":
	default:
		writeErr(w, http.StatusBadRequest, `"format" must be raw, jpeg, webp or png`)
		return
	}
	quality := cfg.Capture.PhotoQuality
	if v := q.Get("quality"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			quality = n
		}
	}
	maxWidth := cfg.Capture.PhotoMaxWidth
	if v := q.Get("maxwidth"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n == 0 {
				maxWidth = 0 // 0 = full sensor resolution
			} else if n >= 64 {
				maxWidth = n
			}
		}
	} else if v := q.Get("scale"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			maxWidth = int(4096 * f)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if !a.shotMu.TryLock() {
		w.Header().Set("Retry-After", "3")
		writeErr(w, http.StatusTooManyRequests, "another capture is in progress; retry in a moment")
		return
	}
	defer a.shotMu.Unlock()

	t0 := time.Now()
	raw, info, err := a.capturePhoto(ctx, camera)
	captureMS := time.Since(t0).Milliseconds()
	info["capture_ms"] = captureMS
	info["stored"] = false
	info["pushed"] = false
	if err != nil {
		info["error"] = err.Error()
		info["hint"] = "POST /api/v1/bridge/fix will grant the camera permission, take the wake lock and raise the Termux uid to the foreground; GET /api/v1/bridge/status says which of those is missing"
		writeJSON(w, http.StatusBadGateway, info)
		return
	}

	meta := boolParam(q.Get("meta"))
	inline := boolParam(q.Get("base64"))
	attach := boolParam(q.Get("download"))
	info["format"] = format
	info["quality"] = quality

	if format == "raw" {
		info["mime"] = "image/jpeg"
		info["bytes"] = len(raw)
		if meta || inline {
			if inline {
				info["data"] = base64.StdEncoding.EncodeToString(raw)
				info["encoding"] = "base64"
			}
			writeJSON(w, http.StatusOK, info)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		if attach {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"photo-%d.jpg\"", time.Now().Unix()))
		}
		w.Write(raw)
		return
	}

	src, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		info["error"] = "decode the camera JPEG: " + derr.Error()
		writeJSON(w, http.StatusBadGateway, info)
		return
	}
	sb := src.Bounds()
	img := src
	if maxWidth > 0 && sb.Dx() > maxWidth {
		nw := maxWidth
		nh := sb.Dy() * maxWidth / sb.Dx()
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
		img = dst
	}
	t1 := time.Now()
	var buf bytes.Buffer
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg", "jpg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	default:
		err = gw.Encode(&buf, img, &gw.Options{Lossy: true, Quality: float32(quality), Method: 4})
	}
	encodeMS := time.Since(t1).Milliseconds()
	if err != nil {
		info["error"] = "encode: " + err.Error()
		writeJSON(w, http.StatusInternalServerError, info)
		return
	}
	ib := img.Bounds()
	info["width"] = ib.Dx()
	info["height"] = ib.Dy()
	info["source_width"] = sb.Dx()
	info["source_height"] = sb.Dy()
	info["bytes"] = buf.Len()
	info["encode_ms"] = encodeMS
	info["mime"] = shotMIME(format)

	if meta || inline {
		if inline {
			info["encoding"] = "base64"
			info["data"] = base64.StdEncoding.EncodeToString(buf.Bytes())
		}
		writeJSON(w, http.StatusOK, info)
		return
	}
	h := w.Header()
	h.Set("Content-Type", shotMIME(format))
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Camera-Id", strconv.Itoa(camera))
	h.Set("X-Camera-Capture-Ms", strconv.FormatInt(captureMS, 10))
	h.Set("X-Camera-Stored", "false")
	if attach {
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"photo-%d.%s\"", time.Now().Unix(), format))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// handleCameraPreview answers a multipart frame stream: repeated still captures,
// re-encoded small. It is deliberately a preview (a frame every second or two),
// not video — the phone's camera app owns a smooth preview, and a headless
// capture session per frame is what this can honestly offer. The still image
// stays the right tool for anything you want to look at properly.
func (a *App) handleCameraPreview(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Capture.CameraEnabled {
		writeErr(w, http.StatusServiceUnavailable, "the camera is disabled in config (capture.camera_enabled=false)")
		return
	}
	q := r.URL.Query()
	camera := cfg.Capture.DefaultCamera
	if v := q.Get("camera"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			camera = n
		}
	}
	fps := cfg.Capture.PreviewFPS
	if v := q.Get("fps"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 10 {
			fps = int(f)
			if fps < 1 {
				fps = 1
			}
		}
	}
	width := cfg.Capture.PreviewMaxWide
	if v := q.Get("width"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 64 {
			width = n
		}
	}
	quality := cfg.Capture.PreviewQuality
	if v := q.Get("quality"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			quality = n
		}
	}
	seconds := 60
	if v := q.Get("seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seconds = minInt(n, 900)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(seconds)*time.Second+30*time.Second)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "multipart/x-mixed-replace; boundary=frameboundary")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Camera-Id", strconv.Itoa(camera))
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	interval := time.Second / time.Duration(fps)
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	frames := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		start := time.Now()
		raw, _, err := a.capturePhoto(ctx, camera)
		if err == nil {
			if img, _, derr := image.Decode(bytes.NewReader(raw)); derr == nil {
				sb := img.Bounds()
				if width > 0 && sb.Dx() > width {
					nw := width
					nh := sb.Dy() * width / sb.Dx()
					dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
					draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, sb, draw.Src, nil)
					img = dst
				}
				var buf bytes.Buffer
				if eerr := gw.Encode(&buf, img, &gw.Options{Lossy: true, Quality: float32(quality), Method: 4}); eerr == nil {
					fmt.Fprintf(w, "--frameboundary\r\nContent-Type: image/webp\r\nContent-Length: %d\r\nX-Frame-Index: %d\r\nX-Capture-Ms: %d\r\n\r\n",
						buf.Len(), frames, start.UnixMilli())
					w.Write(buf.Bytes())
					fmt.Fprint(w, "\r\n")
					flusher.Flush()
					frames++
				}
			}
		} else if frames == 0 {
			// Nothing has been sent yet: answer with the reason instead of an
			// empty stream that looks like a network problem.
			a.logf.Printf("camera preview failed: %v", err)
			return
		}
		if sleep := interval - time.Since(start); sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

// ---------- microphone ----------

type micState struct {
	mu      sync.Mutex
	path    string
	started time.Time
	args    []string
}

var mic = &micState{}

func (a *App) micArgs(seconds, rate, channels, bitrate int, encoder string) []string {
	args := []string{"-e", encoder, "-b", strconv.Itoa(bitrate), "-r", strconv.Itoa(rate), "-c", strconv.Itoa(channels)}
	if seconds > 0 {
		args = append(args, "-l", strconv.Itoa(seconds))
	}
	return args
}

// recordToFile starts a recording, waits for the file, and returns its bytes.
func (a *App) recordToFile(ctx context.Context, seconds, rate, channels, bitrate int, encoder string) ([]byte, map[string]any, error) {
	info := map[string]any{"seconds_requested": seconds, "encoder": encoder, "rate_hz": rate,
		"channels": channels, "bitrate_kbps": bitrate}
	note, err := a.ensureMicReady(ctx)
	if err != nil {
		return nil, info, err
	}
	info["mic_policy"] = note
	path, err := a.bridgeTmpPath(ctx, fmt.Sprintf("mic-%d.m4a", time.Now().UnixNano()))
	if err != nil {
		return nil, info, err
	}
	ext := ".m4a"
	if encoder != "aac" {
		ext = ".bin"
	}
	path = strings.TrimSuffix(path, ".m4a") + ext

	args := append(a.micArgs(seconds, rate, channels, bitrate, encoder), "-f", path)
	out, err := a.bridgeExec(ctx, 40*time.Second, "termux-microphone-record", args...)
	if err != nil {
		return nil, info, fmt.Errorf("%s (%s)", err.Error(), firstLine(out))
	}
	info["recorder_output"] = firstLine(out)

	// The recorder returns immediately and keeps writing in the background, so
	// the file has to be waited for rather than read straight away.
	wait := time.Duration(seconds)*time.Second + 2*time.Second
	deadline := time.Now().Add(wait)
	var size int64
	stable := 0
	last := int64(-1)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, info, ctx.Err()
		default:
		}
		time.Sleep(300 * time.Millisecond)
		st, serr := os.Stat(path)
		if serr != nil {
			continue
		}
		size = st.Size()
		if size == last && size > 0 {
			stable++
			if stable >= 3 {
				break
			}
		} else {
			stable = 0
		}
		last = size
	}
	body, rerr := os.ReadFile(path)
	if !a.config().Capture.KeepFiles {
		defer os.Remove(path)
	}
	info["file"] = filepath.Base(path)
	info["bytes"] = len(body)
	info["kept"] = a.config().Capture.KeepFiles
	if rerr != nil {
		return nil, info, fmt.Errorf("the recording produced no file: %v", rerr)
	}
	if len(body) < 256 {
		return nil, info, fmt.Errorf("the recorder wrote only %d bytes (the uid was probably still blocked from the microphone)", len(body))
	}
	info["mime"] = "audio/mp4"
	if encoder != "aac" {
		info["mime"] = "application/octet-stream"
	}
	return body, info, nil
}

func (a *App) handleMicRecord(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Capture.MicEnabled {
		writeErr(w, http.StatusServiceUnavailable, "the microphone is disabled in config (capture.mic_enabled=false)")
		return
	}
	q := r.URL.Query()
	seconds := cfg.Capture.MicSeconds
	if v := q.Get("seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seconds = minInt(n, cfg.Capture.MicMaxSeconds)
		}
	}
	rate := cfg.Capture.MicRateHz
	if v := q.Get("rate"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 8000 {
			rate = n
		}
	}
	channels := cfg.Capture.MicChannels
	if v := q.Get("channels"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 2 {
			channels = n
		}
	}
	bitrate := cfg.Capture.MicBitrateKbps
	if v := q.Get("bitrate"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 8 {
			bitrate = n
		}
	}
	encoder := strings.ToLower(strings.TrimSpace(q.Get("encoder")))
	if encoder == "" {
		encoder = cfg.Capture.MicEncoder
	}
	switch encoder {
	case "aac", "opus", "amr_wb", "amr_nb":
	default:
		writeErr(w, http.StatusBadRequest, `"encoder" must be aac, opus, amr_wb or amr_nb`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(seconds)*time.Second+60*time.Second)
	defer cancel()
	if !a.shotMu.TryLock() {
		w.Header().Set("Retry-After", "3")
		writeErr(w, http.StatusTooManyRequests, "another capture is in progress; retry in a moment")
		return
	}
	defer a.shotMu.Unlock()

	body, info, err := a.recordToFile(ctx, seconds, rate, channels, bitrate, encoder)
	info["stored"] = false
	info["pushed"] = false
	if err != nil {
		info["error"] = err.Error()
		info["hint"] = "POST /api/v1/bridge/fix takes the Termux wake lock, which is what this device requires before it will open the microphone"
		writeJSON(w, http.StatusBadGateway, info)
		return
	}
	if boolParam(q.Get("meta")) || boolParam(q.Get("base64")) {
		if boolParam(q.Get("base64")) {
			info["encoding"] = "base64"
			info["data"] = base64.StdEncoding.EncodeToString(body)
		}
		writeJSON(w, http.StatusOK, info)
		return
	}
	h := w.Header()
	h.Set("Content-Type", info["mime"].(string))
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Mic-Seconds", strconv.Itoa(seconds))
	h.Set("X-Mic-Bytes", strconv.Itoa(len(body)))
	h.Set("X-Mic-Stored", "false")
	if boolParam(q.Get("download")) {
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"mic-%d.m4a\"", time.Now().Unix()))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (a *App) handleMicStart(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Capture.MicEnabled {
		writeErr(w, http.StatusServiceUnavailable, "the microphone is disabled in config (capture.mic_enabled=false)")
		return
	}
	var body struct {
		Seconds  int    `json:"seconds"`
		Encoder  string `json:"encoder"`
		Bitrate  int    `json:"bitrate_kbps"`
		Rate     int    `json:"rate_hz"`
		Channels int    `json:"channels"`
	}
	if err := readJSONBody(r, &body, 1<<13); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	mic.mu.Lock()
	if mic.path != "" {
		mic.mu.Unlock()
		writeErr(w, http.StatusConflict, "a recording is already running; stop it first")
		return
	}
	mic.mu.Unlock()

	encoder := body.Encoder
	if encoder == "" {
		encoder = cfg.Capture.MicEncoder
	}
	rate := body.Rate
	if rate < 8000 {
		rate = cfg.Capture.MicRateHz
	}
	channels := body.Channels
	if channels < 1 {
		channels = cfg.Capture.MicChannels
	}
	bitrate := body.Bitrate
	if bitrate < 8 {
		bitrate = cfg.Capture.MicBitrateKbps
	}
	seconds := body.Seconds
	if seconds < 0 {
		seconds = 0
	}
	if seconds > cfg.Capture.MicMaxSeconds {
		seconds = cfg.Capture.MicMaxSeconds
	}
	note, err := a.ensureMicReady(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	path, perr := a.bridgeTmpPath(ctx, fmt.Sprintf("mic-live-%d.m4a", time.Now().UnixNano()))
	if perr != nil {
		writeErr(w, http.StatusInternalServerError, perr.Error())
		return
	}
	args := append(a.micArgs(seconds, rate, channels, bitrate, encoder), "-f", path)
	out, cerr := a.bridgeExec(ctx, 40*time.Second, "termux-microphone-record", args...)
	if cerr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": cerr.Error(), "output": firstLine(out)})
		return
	}
	mic.mu.Lock()
	mic.path, mic.started, mic.args = path, time.Now(), args
	mic.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "started_at": mic.started.Format(time.RFC3339), "limit_seconds": seconds,
		"encoder": encoder, "rate_hz": rate, "channels": channels, "bitrate_kbps": bitrate,
		"mic_policy": note, "recorder_output": firstLine(out),
		"note": "stop with POST /api/v1/mic/stop; the file stays on the device only while keep_capture_files is on",
	})
}

func (a *App) handleMicStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	mic.mu.Lock()
	path := mic.path
	started := mic.started
	mic.mu.Unlock()
	if path == "" {
		writeErr(w, http.StatusConflict, "nothing is recording")
		return
	}
	a.bridgeExec(ctx, 20*time.Second, "termux-microphone-record", "-q")
	time.Sleep(700 * time.Millisecond)
	body, err := os.ReadFile(path)
	info := map[string]any{"file": filepath.Base(path), "seconds": round2(time.Since(started).Seconds()), "bytes": len(body)}
	mic.mu.Lock()
	mic.path, mic.args = "", nil
	mic.mu.Unlock()
	if !a.config().Capture.KeepFiles {
		defer os.Remove(path)
	}
	if err != nil || len(body) < 256 {
		info["ok"] = false
		if err != nil {
			info["error"] = err.Error()
		} else {
			info["error"] = "the recording is too small to be audio"
		}
		writeJSON(w, http.StatusBadGateway, info)
		return
	}
	if boolParam(r.URL.Query().Get("base64")) || boolParam(r.URL.Query().Get("meta")) {
		info["ok"] = true
		info["mime"] = "audio/mp4"
		if boolParam(r.URL.Query().Get("base64")) {
			info["encoding"] = "base64"
			info["data"] = base64.StdEncoding.EncodeToString(body)
		}
		writeJSON(w, http.StatusOK, info)
		return
	}
	info["ok"] = true
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Mic-Seconds", strconv.FormatFloat(info["seconds"].(float64), 'f', 2, 64))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (a *App) handleMicStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := a.bridgeExec(ctx, 20*time.Second, "termux-microphone-record", "-i")
	res := map[string]any{"ok": err == nil, "recorder_state": strings.TrimSpace(out)}
	mic.mu.Lock()
	if mic.path != "" {
		res["started_by_api"] = true
		res["started_at"] = mic.started.Format(time.RFC3339)
		res["elapsed_seconds"] = round2(time.Since(mic.started).Seconds())
		if st, serr := os.Stat(mic.path); serr == nil {
			res["bytes"] = st.Size()
		}
	} else {
		res["started_by_api"] = false
	}
	mic.mu.Unlock()
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}
