package main

// Screenshots.
//
// A frame exists only while a request for one is being served: there is no
// background capture and nothing is written to the history database. This
// server has no upload path at all, so the response states "stored": false and
// a client does not have to guess where the bytes went.
//
// The capture itself is delegated to the platform's own /system/bin/screencap
// (the same tool adb and the power menu use). A window marked FLAG_SECURE makes
// SurfaceFlinger hand back a black frame; that is the platform protecting the
// content and this code does not try to get around it.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	gw "github.com/KarpelesLab/gowebp"
	"golang.org/x/image/draw"
)

const (
	screencapPath     = "/system/bin/screencap"
	shotDefaultFormat = "webp"
	shotDefaultQual   = 80
	shotMaxQual       = 100
	shotDefaultScale  = 1.0
	shotMinScale      = 0.1
	// A capture that has not answered in this long is not going to.
	shotTimeout = 20 * time.Second
)

// pngMagic is checked before handing bytes to the decoder: a build whose
// screencap ignores -p would otherwise produce a confusing decode error.
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

type shotOpts struct {
	format  string
	quality int
	scale   float64
	display string
	meta    bool // answer with JSON metadata instead of the image
	inline  bool // include the encoded image in that JSON as base64
	attach  bool // ask the browser to save the image instead of showing it
}

func boolParam(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func parseShotOpts(r *http.Request) (shotOpts, error) {
	q := r.URL.Query()
	o := shotOpts{
		format:  strings.ToLower(strings.TrimSpace(q.Get("format"))),
		quality: shotDefaultQual,
		scale:   shotDefaultScale,
		display: strings.TrimSpace(q.Get("display")),
	}
	if o.format == "" {
		o.format = shotDefaultFormat
	}
	if o.format == "jpg" {
		o.format = "jpeg"
	}
	switch o.format {
	case "webp", "jpeg", "png":
	default:
		return o, fmt.Errorf("unsupported format %q: use webp, jpeg or png", o.format)
	}
	if v := strings.TrimSpace(q.Get("quality")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > shotMaxQual {
			return o, fmt.Errorf("quality must be a whole number between 1 and %d", shotMaxQual)
		}
		o.quality = n
	}
	if v := strings.TrimSpace(q.Get("scale")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < shotMinScale || f > 1 {
			return o, fmt.Errorf("scale must be a number between %.1f and 1", shotMinScale)
		}
		o.scale = f
	}
	// base64 implies the metadata answer: the image has to be described somehow.
	o.inline = boolParam(q.Get("base64"))
	o.meta = o.inline || boolParam(q.Get("meta"))
	o.attach = boolParam(q.Get("download"))
	return o, nil
}

// capturePNG asks the platform for a frame. screencap prints a PNG to stdout
// when it is given no filename, which avoids a temporary file on the device.
func capturePNG(ctx context.Context, display string) ([]byte, error) {
	args := []string{"-p"}
	if display != "" {
		args = append([]string{"-d", display}, args...)
	}
	cmd := exec.CommandContext(ctx, screencapPath, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := firstLine(strings.TrimSpace(errb.String()))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("screencap failed: %s", msg)
	}
	if out.Len() < len(pngMagic) || !bytes.Equal(out.Bytes()[:len(pngMagic)], pngMagic) {
		return nil, fmt.Errorf("screencap returned %d bytes that are not a PNG", out.Len())
	}
	return out.Bytes(), nil
}

func shotMIME(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	default:
		return "image/webp"
	}
}

func (a *App) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	o, err := parseShotOpts(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// One capture at a time. screencap serialises inside SurfaceFlinger anyway,
	// so queueing requests here would only hold sockets open for no benefit.
	if !a.shotMu.TryLock() {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusTooManyRequests, "another screenshot is already being taken; retry in a moment")
		return
	}
	defer a.shotMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), shotTimeout)
	defer cancel()

	t0 := time.Now()
	raw, err := capturePNG(ctx, o.display)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, err = http.StatusGatewayTimeout,
				fmt.Errorf("screencap did not answer within %s", shotTimeout)
		}
		writeErr(w, code, err.Error())
		return
	}
	captureMS := time.Since(t0).Milliseconds()

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "decode screencap output: "+err.Error())
		return
	}
	sb := src.Bounds()
	a.logf.Printf("screenshot: captured %dx%d in %dms (%d bytes png)", sb.Dx(), sb.Dy(), captureMS, len(raw))

	img := src
	if o.scale < 1 {
		nw, nh := int(float64(sb.Dx())*o.scale), int(float64(sb.Dy())*o.scale)
		if nw < 16 {
			nw = 16
		}
		if nh < 16 {
			nh = 16
		}
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
		img = dst
	}

	t1 := time.Now()
	var buf bytes.Buffer
	switch o.format {
	case "png":
		if o.scale == 1 {
			buf.Write(raw) // already a PNG; re-encoding would only cost time
		} else {
			err = png.Encode(&buf, img)
		}
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: o.quality})
	default:
		err = gw.Encode(&buf, img, &gw.Options{Lossy: true, Quality: float32(o.quality), Method: 4})
	}
	encodeMS := time.Since(t1).Milliseconds()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode screenshot: "+err.Error())
		return
	}
	ib := img.Bounds()

	if o.meta {
		out := map[string]any{
			"ok":            true,
			"format":        o.format,
			"quality":       o.quality,
			"scale":         o.scale,
			"width":         ib.Dx(),
			"height":        ib.Dy(),
			"source_width":  sb.Dx(),
			"source_height": sb.Dy(),
			"source_bytes":  len(raw),
			"bytes":         buf.Len(),
			"capture_ms":    captureMS,
			"encode_ms":     encodeMS,
			"display":       orPrimary(o.display),
			// The guarantee, stated in the response rather than only in docs.
			"stored": false,
		}
		if o.quality == shotDefaultQual && o.format == "png" {
			out["quality"] = nil // no meaning for lossless png
		}
		if o.inline {
			out["encoding"] = "base64"
			out["mime"] = shotMIME(o.format)
			out["data"] = base64.StdEncoding.EncodeToString(buf.Bytes())
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	h := w.Header()
	h.Set("Content-Type", shotMIME(o.format))
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Screenshot-Format", o.format)
	h.Set("X-Screenshot-Quality", strconv.Itoa(o.quality))
	h.Set("X-Screenshot-Size", fmt.Sprintf("%dx%d", ib.Dx(), ib.Dy()))
	h.Set("X-Screenshot-Source-Bytes", strconv.Itoa(len(raw)))
	h.Set("X-Screenshot-Capture-Ms", strconv.FormatInt(captureMS, 10))
	h.Set("X-Screenshot-Encode-Ms", strconv.FormatInt(encodeMS, 10))
	h.Set("X-Screenshot-Stored", "false") // never written to the history database
	h.Set("Access-Control-Expose-Headers", "X-Screenshot-Format, X-Screenshot-Quality, X-Screenshot-Size, X-Screenshot-Capture-Ms, X-Screenshot-Encode-Ms")
	if o.attach {
		h.Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=\"screenshot-%d.%s\"", time.Now().Unix(), o.format))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

func orPrimary(s string) string {
	if s == "" {
		return "primary"
	}
	return s
}
