package main

// stream.go — the live screen: a real hardware H.264 encoder on the device,
// fanned out to any number of viewers.
//
// Why screenrecord and not a MediaProjection app: this process is a root
// daemon with no Activity and no UI, and a VirtualDisplay would cost an entire
// app package to host. The platform already ships /system/bin/screenrecord,
// which drives the hardware encoder and can hand the result back as bare H.264
// on stdout — no container, no muxer latency, no re-encode. That is exactly
// what a low-latency viewer wants.
//
// Shape of the stream. GET /api/v1/stream/screen answers 200 with a chunked
// body. The first chunk is one line of JSON followed by a newline:
//
//	{protocol, codec, avc, width, height, bitrate, fps_hint, size,
//	 started_at, display_woken, keep_display_on}
//
// The client uses it to configure its decoder before the first frame arrives.
// Every chunk after that is one access unit framed as a 4-byte big-endian
// length followed by the raw Annex-B bytes. With raw=1 the JSON header line is
// skipped and the Annex-B bytes are written straight through, so mpv/ffplay
// can read the URL directly.
//
// One (size, bitrate, display) triple is one screenrecord child process; every
// viewer of that triple is fed from the same process, so a second viewer costs
// no extra encoding. A viewer that falls behind is dropped rather than allowed
// to push back-pressure into the encoder: a stalled socket must never stall the
// capture. Dropped frames are counted and reported by /api/v1/stream/status.
//
// The webp fallback. GET /api/v1/stream/screen/mjpeg answers a
// multipart/x-mixed-replace body of webp frames rebuilt from the screencap
// pipeline (screenshot.go), throttled to stream.mjpeg_fps / mjpeg_max_width /
// mjpeg_quality. It is a fallback for a client that cannot decode H.264, not a
// smooth preview: each frame is a fresh capture, so it runs at a few frames a
// second at best.
//
// Measured on the test device (POCO F7 / Redmi Turbo 4 Pro, panel 720x1560):
//
//	screenrecord --output-format=h264 --size 720x1560 --bit-rate 3000000 \
//	    --time-limit 0 -
//
//	3.0 s of stdout = 904687 bytes -> ~2412 kbit/s, profile High, yuv420p,
//	level 50; NAL census over those 3 s = 288 slices(type 1) + 1 IDR(type 5)
//	+ 1 SPS(type 7) + 1 PPS(type 8), all in Annex-B. The SPS bytes are
//	64 00 32, so the codec string is avc1.640032. --time-limit 0 means "no
//	limit".
//
// The panel must be lit. screenrecord does not care about the keyguard, but
// with the display off or dozing it exits at once with
//
//	ERROR: INVALID_LAYER_STACK, please check your display state
//
// so a viewer that asks for wake=1 is first woken with input keyevent
// KEYCODE_WAKEUP. /api/v1/stream/display reports the current state, and it
// probes by really running screenrecord for one second into io.Discard so the
// answer reflects the device right now instead of a guess.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gw "github.com/KarpelesLab/gowebp"
	"golang.org/x/image/draw"
)

const (
	screenrecordPath = "/system/bin/screenrecord"

	// Per-viewer frame queue. Deliberately shallow: it bounds how far behind
	// the live edge a slow viewer may drift before frames are dropped.
	streamClientQueue = 8

	// How long a fresh viewer waits for the encoder's SPS before the header
	// line goes out without it. A healthy screenrecord answers in well under a
	// second; this only matters when something is wrong.
	streamStartTimeout = 6 * time.Second

	// The display probe records for this many seconds.
	screenProbeSeconds = 1

	// Multipart boundary used by the webp fallback stream.
	streamMjpegBoundary = "phoneserver-frame"
)

var (
	errStreamDisabled = errors.New("screen streaming is disabled in config (stream.enabled=false)")
	errTooManyClients = errors.New("too many screen streaming clients are already connected")
)

// ---------- Annex-B access-unit splitting (pure, unit-tested) ----------
//
// A raw H.264 byte stream is a sequence of NAL units, each introduced by a 3-
// or 4-byte start code. A decodable frame (an access unit) is a run of
// parameter-set / SEI NALs followed by a slice NAL (types 1 or 5). screenrecord
// emits the parameter sets once, at the very start, so the splitter holds every
// non-slice NAL back and prepends it to the next slice: that is what makes the
// first frame self-contained and the rest clean.
//
// Everything here is a pure function of the bytes fed in. Feeding the same
// stream in one call or in thousands must produce byte-identical frames — the
// reader thread calls Feed with whatever a single pipe read returned, so a NAL
// is routinely split across reads — and the unit test pins that down.

type nalSplitter struct {
	buf     []byte // bytes not yet formed into a complete NAL
	pending []byte // NALs held back for the access unit being assembled
}

// Feed adds bytes and returns every access unit that completed. An access unit
// that is still arriving stays buffered until its terminating slice is seen.
func (s *nalSplitter) Feed(p []byte) [][]byte {
	if len(p) > 0 {
		s.buf = append(s.buf, p...)
	}
	var out [][]byte
	for {
		sc, scw := nextStartCode(s.buf, 0)
		if sc < 0 {
			// No start code anywhere: only a possible start-code prefix can
			// still matter, so keep the last few bytes and drop the rest.
			if n := len(s.buf); n > 3 {
				s.buf = s.buf[n-3:]
			}
			break
		}
		if sc > 0 {
			// Trim bytes from before the first start code (idle zeros or
			// junk). copy handles the self-overlap; append would not.
			n := copy(s.buf, s.buf[sc:])
			s.buf = s.buf[:n]
		}
		next, _ := nextStartCode(s.buf, scw)
		if next < 0 {
			// The current NAL has not been terminated yet: wait for more.
			break
		}
		nal := append([]byte(nil), s.buf[:next]...)
		s.accumulate(nal, &out)
		n := copy(s.buf, s.buf[next:])
		s.buf = s.buf[:n]
	}
	return out
}

// Flush discards a trailing fragment: an access unit with no slice is not
// decodable, so handing it to a decoder would only produce an error. It exists
// so a whole-buffer split and a chunked split end in the same state.
func (s *nalSplitter) Flush() [][]byte {
	s.pending = nil
	s.buf = nil
	return nil
}

func (s *nalSplitter) accumulate(nal []byte, out *[][]byte) {
	t := nalHeaderType(nal)
	if t == 1 || t == 5 {
		// A slice seals the access unit: emit everything held so far plus it.
		s.pending = append(s.pending, nal...)
		*out = append(*out, s.pending)
		s.pending = nil // hand ownership to the caller; the next AU allocates
		return
	}
	// SPS/PPS/SEI/AUD/anything else belongs to the access unit that starts
	// with the next slice, so hold it back.
	s.pending = append(s.pending, nal...)
}

// splitAnnexB is the one-shot convenience used by tests: split a complete
// Annex-B buffer into access units.
func splitAnnexB(b []byte) [][]byte {
	var s nalSplitter
	return append(s.Feed(b), s.Flush()...)
}

// nextStartCode finds the first Annex-B start code in b at or after from and
// returns its offset and width (3 or 4). A 4-byte code 00 00 00 01 is reported
// at the first of its zeros so the exact bytes are preserved. Returns (-1, 0)
// when no complete start code is present.
func nextStartCode(b []byte, from int) (int, int) {
	if from < 0 {
		from = 0
	}
	for i := from; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if i > 0 && b[i-1] == 0 {
				return i - 1, 4
			}
			return i, 3
		}
	}
	return -1, 0
}

// nalHeaderType returns the nal_unit_type of an Annex-B NAL unit (the low 5
// bits of the byte after the start code), or 0 if b does not begin with a start
// code. 7=SPS, 8=PPS, 6=SEI, 5=IDR slice, 1=non-IDR slice.
func nalHeaderType(b []byte) byte {
	i := 0
	switch {
	case len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1:
		i = 4
	case len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1:
		i = 3
	default:
		return 0
	}
	if i >= len(b) {
		return 0
	}
	return b[i] & 0x1f
}

// spsProfileLevel reads profile_idc, the constraint-flags byte and level_idc
// from the first SPS NAL (type 7) in an Annex-B buffer. Those are the RBSP's
// first three bytes, which is exactly what an avc1.PPCCLL string is built from.
func spsProfileLevel(b []byte) (profile, constraints, level byte, ok bool) {
	off := 0
	for {
		sc, scw := nextStartCode(b, off)
		if sc < 0 {
			return 0, 0, 0, false
		}
		i := sc + scw
		if i >= len(b) {
			return 0, 0, 0, false
		}
		if b[i]&0x1f == 7 {
			if i+3 >= len(b) {
				return 0, 0, 0, false
			}
			return b[i+1], b[i+2], b[i+3], true
		}
		off = i + 1
	}
}

// avcCodecString builds the RFC 6381 codec string a browser needs to configure
// its decoder: avc1. + profile/constraints/level in hex.
func avcCodecString(profile, constraints, level byte) string {
	return fmt.Sprintf("avc1.%02X%02X%02X", profile, constraints, level)
}

// writeFrame writes one access unit as a 4-byte big-endian length followed by
// its Annex-B bytes — the framing the len-prefix protocol specifies.
func writeFrame(w io.Writer, frame []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(frame)
	return err
}

// parseStreamSize accepts "720x1560" and rejects anything screenrecord could
// not be asked for. It rounds nothing: the size that goes to screenrecord is
// the size the client gets.
func parseStreamSize(s string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(s)), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("size must be WxH, e.g. 720x1560 (got %q)", s)
	}
	w, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if e1 != nil || e2 != nil || w < 128 || h < 128 || w > 4096 || h > 4096 {
		return 0, 0, fmt.Errorf("size must be WxH with each side between 128 and 4096 (got %q)", s)
	}
	return w, h, nil
}

func streamKey(size string, bitrate int, display string) string {
	return size + "|" + strconv.Itoa(bitrate) + "|" + display
}

func roundTo(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

// ---------- viewers ----------

// streamClient is one HTTP viewer attached to an encoder. Its queue is separate
// from every other viewer's, so one slow socket cannot affect the rest.
type streamClient struct {
	q       chan []byte
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64
}

// offer enqueues a frame without ever blocking. When the queue is full the
// oldest frame is discarded and the newest enqueued, so a viewer that has
// fallen behind catches up to the live edge instead of replaying stale video.
func (c *streamClient) offer(frame []byte) {
	select {
	case c.q <- frame:
		return
	default:
	}
	c.dropped.Add(1)
	select {
	case <-c.q:
	default:
	}
	select {
	case c.q <- frame:
	default:
	}
}

// close signals the serving handler to return. The queue channel is never
// closed: the encoder may still be mid-send when a viewer leaves, and closing a
// channel a sender has already selected on would panic.
func (c *streamClient) close() {
	c.once.Do(func() { close(c.done) })
}

// ---------- encoder ----------

// streamEncoder owns one screenrecord child process and fans its frames out.
type streamEncoder struct {
	mgr     *streamManager
	key     string
	size    string // "720x1560"
	width   int
	height  int
	bitrate int
	display string
	fpsHint int

	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	stdout  io.ReadCloser
	stderr  *bytes.Buffer
	started time.Time

	splitter nalSplitter

	mu      sync.Mutex
	clients map[*streamClient]struct{}
	closed  bool
	lastErr string
	idle    *time.Timer

	spsReady chan struct{}
	spsOnce  sync.Once
	avcVal   atomic.Value // string

	frames atomic.Int64
	bytes  atomic.Int64
}

func (e *streamEncoder) avc() string {
	if v, ok := e.avcVal.Load().(string); ok {
		return v
	}
	return ""
}

func (e *streamEncoder) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

func (e *streamEncoder) failureReason() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastErr == "" {
		return "screenrecord exited"
	}
	return e.lastErr
}

func (e *streamEncoder) setErr(msg string) {
	e.mu.Lock()
	e.lastErr = msg
	e.mu.Unlock()
}

// addClient attaches a viewer and cancels any pending idle shutdown.
func (e *streamEncoder) addClient() *streamClient {
	c := &streamClient{q: make(chan []byte, streamClientQueue), done: make(chan struct{})}
	e.mu.Lock()
	e.clients[c] = struct{}{}
	if e.idle != nil {
		e.idle.Stop()
		e.idle = nil
	}
	e.mu.Unlock()
	return c
}

// removeClient detaches a viewer; when the last one leaves it arms the idle
// timer that eventually reaps the child process.
func (e *streamEncoder) removeClient(c *streamClient) {
	c.close()
	idleS := e.mgr.app.config().Stream.IdleTimeoutS
	e.mu.Lock()
	delete(e.clients, c)
	if len(e.clients) == 0 && !e.closed {
		d := time.Duration(idleS) * time.Second
		if d <= 0 {
			d = 20 * time.Second
		}
		e.idle = time.AfterFunc(d, func() {
			e.close(fmt.Sprintf("idle for %s with no viewers", d))
		})
	}
	e.mu.Unlock()
}

func (e *streamEncoder) closeClients() {
	e.mu.Lock()
	cs := make([]*streamClient, 0, len(e.clients))
	for c := range e.clients {
		cs = append(cs, c)
	}
	e.clients = map[*streamClient]struct{}{}
	e.mu.Unlock()
	for _, c := range cs {
		c.close()
	}
}

// close stops the encoder: cancel the child's context (CommandContext kills it),
// then wait for the reader goroutine, which reaps the process and closes every
// viewer. Calling it twice is harmless.
func (e *streamEncoder) close(reason string) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	if e.idle != nil {
		e.idle.Stop()
		e.idle = nil
	}
	e.mu.Unlock()

	e.mgr.app.logf.Printf("stream: encoder %s (%dx%d @ %d bps) closing: %s", e.key, e.width, e.height, e.bitrate, reason)
	e.cancel()
	<-e.done
}

// publish hands one finished access unit to every viewer. The frame slice is
// read-only from here on, so all viewers can share it without copying.
func (e *streamEncoder) publish(frame []byte) {
	if e.avc() == "" {
		if p, c, l, ok := spsProfileLevel(frame); ok {
			e.avcVal.Store(avcCodecString(p, c, l))
			e.spsOnce.Do(func() { close(e.spsReady) })
		}
	}
	e.frames.Add(1)
	e.bytes.Add(int64(len(frame)))

	e.mu.Lock()
	cs := make([]*streamClient, 0, len(e.clients))
	for c := range e.clients {
		cs = append(cs, c)
	}
	e.mu.Unlock()
	for _, c := range cs {
		c.offer(frame)
	}
}

// run reads the encoder's stdout, splits it into access units and fans them
// out. It is the only owner of the pipe and the only caller of cmd.Wait, so a
// killed or self-terminated process always leaves the goroutine and no zombie.
func (e *streamEncoder) run() {
	defer close(e.done)
	buf := make([]byte, 32<<10)
	var readErr error
	for {
		n, err := e.stdout.Read(buf)
		if n > 0 {
			for _, frame := range e.splitter.Feed(buf[:n]) {
				e.publish(frame)
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	// Waiting here (after reads have drained) is what reaps the child.
	werr := e.cmd.Wait()
	e.stdout.Close()

	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if !closed {
		msg := ""
		if s := strings.TrimSpace(e.stderr.String()); s != "" {
			msg = firstLine(s)
		} else if werr != nil {
			msg = werr.Error()
		} else if readErr != nil && readErr != io.EOF {
			msg = readErr.Error()
		}
		if msg == "" {
			msg = "screenrecord ended the stream"
		}
		e.setErr(msg)
		e.mgr.app.logf.Printf("stream: encoder %s (%dx%d) stopped: %s", e.key, e.width, e.height, msg)
	}
	e.closeClients()
	e.mgr.remove(e)
}

// ---------- manager ----------

type streamManager struct {
	app *App

	mu       sync.Mutex
	encoders map[string]*streamEncoder
	clients  int

	// keep_display_on state. Protected by mu so the 0<->1 viewer transition
	// and the enable/restore commands cannot interleave.
	stayApplied  bool
	stayOriginal string
	stayHadValue bool
}

func newStreamManager(a *App) *streamManager {
	return &streamManager{app: a, encoders: map[string]*streamEncoder{}}
}

func (m *streamManager) clientCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clients
}

func (m *streamManager) stayOnInEffect() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stayApplied
}

// subscribe attaches a viewer, starting the encoder if this is the first one.
// The (size, bitrate, display) triple selects which child process serves it.
func (m *streamManager) subscribe(size string, bitrate int, display string) (*streamEncoder, *streamClient, error) {
	cfg := m.app.config()
	if !cfg.Stream.Enabled {
		return nil, nil, errStreamDisabled
	}
	m.mu.Lock()
	if m.clients >= cfg.Stream.MaxClients {
		n := m.clients
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("%w (%d of %d in use)", errTooManyClients, n, cfg.Stream.MaxClients)
	}
	key := streamKey(size, bitrate, display)
	e := m.encoders[key]
	if e == nil || e.isClosed() {
		ne, err := m.start(key, size, bitrate, display)
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		e = ne
		m.encoders[key] = e
	}
	c := e.addClient()
	m.clients++
	if m.clients == 1 {
		m.applyStayOn(cfg)
	}
	m.mu.Unlock()
	return e, c, nil
}

// release detaches a viewer. When the last viewer anywhere leaves, the
// keep_display_on override is put back exactly as it was found.
func (m *streamManager) release(e *streamEncoder, c *streamClient) {
	m.mu.Lock()
	if m.clients > 0 {
		m.clients--
	}
	if m.clients == 0 {
		m.restoreStayOn()
	}
	m.mu.Unlock()
	e.removeClient(c)
}

func (m *streamManager) remove(e *streamEncoder) {
	m.mu.Lock()
	if cur, ok := m.encoders[e.key]; ok && cur == e {
		delete(m.encoders, e.key)
	}
	m.mu.Unlock()
}

// start spawns the screenrecord child. --time-limit 0 means record until we
// kill it; the process's context is that kill switch.
func (m *streamManager) start(key, size string, bitrate int, display string) (*streamEncoder, error) {
	sw, sh, err := parseStreamSize(size)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"--output-format=h264",
		"--size", fmt.Sprintf("%dx%d", sw, sh),
		"--bit-rate", strconv.Itoa(bitrate),
		"--time-limit", "0",
	}
	if display != "" {
		args = append(args, "--display-id", display)
	}
	args = append(args, "-") // write the raw H.264 to stdout, no file
	cmd := exec.CommandContext(ctx, screenrecordPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("screenrecord stdout pipe: %w", err)
	}
	errb := &bytes.Buffer{}
	cmd.Stderr = errb
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("screenrecord would not start: %s", err)
	}
	e := &streamEncoder{
		mgr:      m,
		key:      key,
		size:     size,
		width:    sw,
		height:   sh,
		bitrate:  bitrate,
		display:  display,
		fpsHint:  m.app.config().Stream.DefaultFPS,
		cmd:      cmd,
		cancel:   cancel,
		done:     make(chan struct{}),
		stdout:   stdout,
		stderr:   errb,
		started:  time.Now(),
		clients:  map[*streamClient]struct{}{},
		spsReady: make(chan struct{}),
	}
	m.app.logf.Printf("stream: started encoder %s (%dx%d @ %d bps)", key, sw, sh, bitrate)
	go e.run()
	return e, nil
}

// applyStayOn holds the panel awake for the duration a viewer is connected. It
// remembers the previous stay_on_while_plugged_in so it can be restored exactly.
func (m *streamManager) applyStayOn(cfg Config) {
	if !cfg.Stream.KeepDisplayOn || m.stayApplied {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	prev, err := m.app.runCmd(ctx, 8*time.Second, binPath("settings"), "get", "global", "stay_on_while_plugged_in")
	prev = strings.TrimSpace(prev)
	if err != nil {
		prev = ""
	}
	if _, err := m.app.runCmd(ctx, 8*time.Second, binPath("svc"), "power", "stayon", "true"); err != nil {
		m.app.logf.Printf("stream: keep_display_on could not enable stayon: %v", err)
		return
	}
	m.stayOriginal = prev
	m.stayHadValue = prev != "" && prev != "null"
	m.stayApplied = true
	m.app.logf.Printf("stream: keep_display_on -> svc power stayon true (previous %q)", m.stayOriginal)
}

// restoreStayOn puts the setting back and says so; a no-op when it was never
// changed.
func (m *streamManager) restoreStayOn() {
	if !m.stayApplied {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if m.stayHadValue {
		if _, err := m.app.runCmd(ctx, 8*time.Second, binPath("settings"), "put", "global", "stay_on_while_plugged_in", m.stayOriginal); err != nil {
			m.app.logf.Printf("stream: keep_display_on restore failed: %v", err)
		} else {
			m.app.logf.Printf("stream: keep_display_on restored to %q", m.stayOriginal)
		}
	} else {
		if _, err := m.app.runCmd(ctx, 8*time.Second, binPath("settings"), "delete", "global", "stay_on_while_plugged_in"); err != nil {
			m.app.logf.Printf("stream: keep_display_on restore (delete) failed: %v", err)
		} else {
			m.app.logf.Printf("stream: keep_display_on restored (setting was unset)")
		}
	}
	m.stayApplied = false
}

// stopAll is called on shutdown: kill every encoder, disconnect every viewer
// and put the display-wake setting back.
func (m *streamManager) stopAll(reason string) {
	m.mu.Lock()
	encs := make([]*streamEncoder, 0, len(m.encoders))
	for _, e := range m.encoders {
		encs = append(encs, e)
	}
	m.encoders = map[string]*streamEncoder{}
	m.clients = 0
	m.restoreStayOn()
	m.mu.Unlock()

	for _, e := range encs {
		e.close(reason)
	}
}

func (m *streamManager) snapshot() []map[string]any {
	m.mu.Lock()
	encs := make([]*streamEncoder, 0, len(m.encoders))
	for _, e := range m.encoders {
		encs = append(encs, e)
	}
	m.mu.Unlock()
	out := make([]map[string]any, 0, len(encs))
	for _, e := range encs {
		out = append(out, e.status())
	}
	return out
}

func (e *streamEncoder) status() map[string]any {
	f := e.frames.Load()
	b := e.bytes.Load()
	up := time.Since(e.started).Seconds()
	fps, kbps := 0.0, 0.0
	if up > 0 {
		fps = float64(f) / up
		kbps = float64(b) * 8 / 1000 / up
	}
	var dropped int64
	e.mu.Lock()
	n := len(e.clients)
	for c := range e.clients {
		dropped += c.dropped.Load()
	}
	lastErr := e.lastErr
	e.mu.Unlock()
	return map[string]any{
		"size":          e.size,
		"width":         e.width,
		"height":        e.height,
		"bitrate":       e.bitrate,
		"clients":       n,
		"frames":        f,
		"bytes":         b,
		"fps_measured":  roundTo(fps, 2),
		"kbps_measured": roundTo(kbps, 1),
		"uptime_s":      roundTo(up, 2),
		"last_error":    lastErr,
		"dropped":       dropped,
		"avc":           e.avc(),
		"display":       orPrimary(e.display),
		"started_at":    e.started.Format(time.RFC3339),
	}
}

// ---------- HTTP ----------

// streamHeader is the first line of the len-prefix stream: everything a decoder
// needs before the first frame. Written as a struct so the field order is fixed.
type streamHeader struct {
	Protocol      string `json:"protocol"`
	Codec         string `json:"codec"`
	AVC           string `json:"avc"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Bitrate       int    `json:"bitrate"`
	FPSHint       int    `json:"fps_hint"`
	Size          string `json:"size"`
	StartedAt     string `json:"started_at"`
	DisplayWoken  bool   `json:"display_woken"`
	KeepDisplayOn bool   `json:"keep_display_on"`
}

// wakeDisplay turns the panel on and gives SurfaceFlinger a moment to rebuild
// the layer stack; screenrecord refuses to start in either the "off" or the
// immediate post-wake state.
func (a *App) wakeDisplay(ctx context.Context) bool {
	wctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := a.runCmd(wctx, 8*time.Second, binPath("input"), "keyevent", "KEYCODE_WAKEUP"); err != nil {
		a.logf.Printf("stream: wake display failed: %v", err)
		return false
	}
	time.Sleep(400 * time.Millisecond)
	return true
}

func (a *App) handleScreenStream(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Stream.Enabled {
		writeErr(w, http.StatusServiceUnavailable, errStreamDisabled.Error())
		return
	}
	q := r.URL.Query()

	size := strings.TrimSpace(q.Get("size"))
	if size == "" {
		size = cfg.Stream.DefaultSize
	}
	sw, sh, err := parseStreamSize(size)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	size = fmt.Sprintf("%dx%d", sw, sh)

	bitrate := cfg.Stream.DefaultBitrate
	if v := strings.TrimSpace(q.Get("bitrate")); v != "" {
		n, cerr := strconv.Atoi(v)
		if cerr != nil || n < 100000 || n > 100000000 {
			writeErr(w, http.StatusBadRequest, "bitrate must be a whole number between 100000 and 100000000 bits per second")
			return
		}
		bitrate = n
	}
	display := strings.TrimSpace(q.Get("display"))
	wake := cfg.Stream.WakeDisplay
	if v := strings.TrimSpace(q.Get("wake")); v != "" {
		wake = boolParam(v)
	}
	raw := boolParam(q.Get("raw"))

	// Fail before subscribing if the response cannot be streamed.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "this connection cannot be streamed to (the response writer cannot flush)")
		return
	}

	displayWoken := false
	if wake && !a.collectDisplay().ScreenOn {
		displayWoken = a.wakeDisplay(r.Context())
	}

	enc, client, err := a.streams.subscribe(size, bitrate, display)
	if err != nil {
		switch {
		case errors.Is(err, errTooManyClients):
			w.Header().Set("Retry-After", "2")
			writeErr(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, errStreamDisabled):
			writeErr(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeErr(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	defer a.streams.release(enc, client)

	// Wait for the encoder's SPS so the header can carry the real codec string.
	// A second viewer of an existing encoder already has it.
	avc := enc.avc()
	if avc == "" {
		select {
		case <-enc.spsReady:
			avc = enc.avc()
		case <-enc.done:
			writeErr(w, http.StatusBadGateway, "screen encoder stopped before it produced a frame: "+enc.failureReason())
			return
		case <-r.Context().Done():
			return
		case <-time.After(streamStartTimeout):
			avc = enc.avc()
		}
	}

	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // a reverse proxy must not buffer a live stream
	h.Set("Access-Control-Expose-Headers", "X-Stream-Protocol")
	if raw {
		h.Set("Content-Type", "video/h264")
		h.Set("X-Stream-Protocol", "annexb-raw")
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("X-Stream-Protocol", "annexb-len-prefix")
	}
	w.WriteHeader(http.StatusOK)

	if !raw {
		line, merr := json.Marshal(streamHeader{
			Protocol:      "annexb-len-prefix",
			Codec:         "h264",
			AVC:           avc,
			Width:         enc.width,
			Height:        enc.height,
			Bitrate:       enc.bitrate,
			FPSHint:       enc.fpsHint,
			Size:          enc.size,
			StartedAt:     enc.started.Format(time.RFC3339),
			DisplayWoken:  displayWoken,
			KeepDisplayOn: a.streams.stayOnInEffect(),
		})
		if merr != nil {
			return
		}
		if _, werr := w.Write(append(line, '\n')); werr != nil {
			return
		}
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-client.done:
			return
		case <-enc.done:
			return
		case frame := <-client.q:
			if len(frame) == 0 {
				continue
			}
			if raw {
				if _, werr := w.Write(frame); werr != nil {
					return
				}
			} else if werr := writeFrame(w, frame); werr != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleScreenMjpeg answers the webp fallback: a fresh screencap per frame,
// scaled and lossy-encoded, delivered as multipart/x-mixed-replace. It is not a
// video path — it exists for clients that cannot decode H.264.
func (a *App) handleScreenMjpeg(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	if !cfg.Stream.Enabled {
		writeErr(w, http.StatusServiceUnavailable, errStreamDisabled.Error())
		return
	}
	q := r.URL.Query()
	fps := cfg.Stream.MjpegFPS
	if v := strings.TrimSpace(q.Get("fps")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 30 {
			fps = int(f)
			if fps < 1 {
				fps = 1
			}
		}
	}
	maxW := cfg.Stream.MjpegMaxWidth
	if v := strings.TrimSpace(q.Get("width")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 64 && n <= 4096 {
			maxW = n
		}
	}
	quality := cfg.Stream.MjpegQuality
	if v := strings.TrimSpace(q.Get("quality")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			quality = n
		}
	}
	display := strings.TrimSpace(q.Get("display"))
	wake := cfg.Stream.WakeDisplay
	if v := strings.TrimSpace(q.Get("wake")); v != "" {
		wake = boolParam(v)
	}
	if wake && !a.collectDisplay().ScreenOn {
		a.wakeDisplay(r.Context())
	}

	h := w.Header()
	h.Set("Content-Type", "multipart/x-mixed-replace; boundary="+streamMjpegBoundary)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Stream-Protocol", "mjpeg-webp")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	tick := time.NewTicker(time.Second / time.Duration(fps))
	defer tick.Stop()
	index := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		frame, derr := a.mjpegFrame(r.Context(), display, maxW, quality)
		if derr != nil {
			if index == 0 {
				// Nothing sent yet: the caller would otherwise see an empty
				// stream that looks like a network failure.
				a.logf.Printf("stream mjpeg: first frame failed: %v", derr)
				return
			}
			continue
		}
		if _, werr := fmt.Fprintf(w, "--%s\r\nContent-Type: image/webp\r\nContent-Length: %d\r\nX-Frame-Index: %d\r\n\r\n",
			streamMjpegBoundary, len(frame), index); werr != nil {
			return
		}
		if _, werr := w.Write(frame); werr != nil {
			return
		}
		if _, werr := io.WriteString(w, "\r\n"); werr != nil {
			return
		}
		flusher.Flush()
		index++
	}
}

// mjpegFrame captures one screen and returns it as webp, scaled down to maxW.
// The screencap mutex is shared with the still-screenshot endpoint so the two
// cannot both hammer SurfaceFlinger at once.
func (a *App) mjpegFrame(ctx context.Context, display string, maxW, quality int) ([]byte, error) {
	if !a.shotMu.TryLock() {
		return nil, errors.New("screenshot capture busy")
	}
	defer a.shotMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, shotTimeout)
	defer cancel()
	raw, err := capturePNG(cctx, display)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	img := src
	if sb := src.Bounds(); maxW > 0 && sb.Dx() > maxW {
		nw := maxW
		nh := sb.Dy() * maxW / sb.Dx()
		if nh < 2 {
			nh = 2
		}
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
		img = dst
	}
	var buf bytes.Buffer
	if err := gw.Encode(&buf, img, &gw.Options{Lossy: true, Quality: float32(quality), Method: 4}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *App) handleStreamStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	writeJSON(w, http.StatusOK, map[string]any{
		"implemented":          true,
		"enabled":              cfg.Stream.Enabled,
		"clients":              a.streams.clientCount(),
		"max_clients":          cfg.Stream.MaxClients,
		"idle_timeout_seconds": cfg.Stream.IdleTimeoutS,
		"keep_display_on":      a.streams.stayOnInEffect(),
		"encoders":             a.streams.snapshot(),
		"display":              a.collectDisplay(),
	})
}

// handleStreamDisplay reports the panel state and whether screenrecord will run
// right now. The probe is a real one-second recording into io.Discard: an
// answer that is actually exercised beats one inferred from a property.
func (a *App) handleStreamDisplay(w http.ResponseWriter, r *http.Request) {
	d := a.collectDisplay()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	probeOK, probeErr := a.probeScreenrecord(ctx)

	note := ""
	switch {
	case !d.ScreenOn:
		note = "the panel is off; screenrecord refuses to start in this state (INVALID_LAYER_STACK) until it is woken with input keyevent KEYCODE_WAKEUP"
	case probeOK:
		note = "the display is on and screenrecord accepted a one-second trial recording"
	default:
		note = "the display reports as on but screenrecord still refused the trial recording; see probe_error"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"screen_on":      d.ScreenOn,
		"wakefulness":    d.Wakefulness,
		"probe_ok":       probeOK,
		"probe_error":    probeErr,
		"note":           note,
		"brightness":     d.Brightness,
		"brightness_max": d.BrightnessMax,
		"refresh_hint":   d.RefreshHint,
	})
}

// probeScreenrecord runs screenrecord for one second into io.Discard and
// reports the outcome verbatim, including any stderr text.
func (a *App) probeScreenrecord(ctx context.Context) (bool, string) {
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, screenrecordPath,
		"--output-format=h264", "--time-limit", strconv.Itoa(screenProbeSeconds), "-")
	var errb bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &errb
	err := cmd.Run()
	msg := strings.TrimSpace(errb.String())
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return false, firstLine(msg)
	}
	if msg != "" {
		// It ran, but said something: pass that through rather than hiding it.
		return true, firstLine(msg)
	}
	return true, ""
}
