package main

// mcp.go — the MCP (Model Context Protocol) server: two transports, one tool
// table (mcptools.go).
//
// Why both transports: an agent that can start a process wants stdio
// (`phoneserver -mcp`); an agent that only gets an HTTP endpoint wants POST
// /mcp. Both funnel into the same dispatcher and the same tool table, so a tool
// behaves identically either way.
//
// Protocol shape (JSON-RPC 2.0, MCP revisions 2024-11-05 / 2025-03-26 /
// 2025-06-18):
//
//   - stdio: one JSON object per line on stdin, one response object per line on
//     stdout. stdout carries protocol messages only — every diagnostic goes to
//     stderr, because a stray print on stdout corrupts the stream for the
//     client. Lines are capped at 32MB (a base64 screenshot is the largest
//     thing that crosses this channel).
//   - HTTP: POST /mcp accepts a single request object or a batch array and
//     answers application/json; when the client's Accept asks for
//     text/event-stream, one `data: <json>` event is written and the response
//     ends (there is no server-initiated traffic). The endpoint is stateless:
//     the Mcp-Session-Id header is echoed, and minted when the client did not
//     send one, but no per-session state is kept — a session id is a courtesy
//     for clients that expect sticky sessions, not a contract.
//
// Tools are not a re-implementation of the API. Every call is an HTTP request
// to the same REST endpoints this server already serves (X-API-Key = the
// configured token), so the MCP surface cannot drift from the phone's real
// capabilities and cannot slip past a config gate: security.allow_shell keeps
// answering 403, and the agent gets that text verbatim.
//
// Safety: the server is a controller for the user's own phone. It defaults to
// loopback-only listening, the dangerous tools are behind config switches, and
// mcp.read_only=true hides every mutating tool from tools/list and refuses them
// in tools/call.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// mcpProtocolFallback is what we answer when the client asks for a revision
	// we do not know: claiming a newer revision we might not honour is worse
	// than saying which one we speak.
	mcpProtocolFallback = "2025-06-18"
	mcpServerName       = "phoneserver"

	// mcpMaxLineBytes caps a single stdio request line (32MB) — big enough for a
	// base64 screenshot, small enough that a runaway line cannot exhaust memory.
	mcpMaxLineBytes = 32 << 20
	// mcpMaxHTTPBody caps an HTTP request body the same way.
	mcpMaxHTTPBody = 32 << 20
	// mcpMaxResponseBytes caps what we read back from the phone's API.
	mcpMaxResponseBytes = 128 << 20
)

var mcpKnownProtocols = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// mcpInstructions is returned from initialize. The first paragraph is the part
// that matters for safety: this is the user's own phone, reached over loopback,
// with the destructive tools switched off unless the user turned them on.
const mcpInstructions = `这是用户自己手机上运行的 phoneserver 控制器。工具直接操作这台手机：遥测（电量/CPU/温度/网络…）、屏幕截图与串流、UI 自动化（点击/滑动/输入）、应用管理、设备控制（亮度/音量/手电/震动/TTS/红外）、通知与短信/通话/联系人、相机与麦克风、文件共享与 WebDAV、本地历史数据查询。

边界与安全：
- 服务默认只绑 loopback（127.0.0.1），也就是只有本机进程能调用；要暴露到局域网必须显式打开 security.allow_non_loopback_listen。
- 危险工具默认关闭：phone_shell 需要 security.allow_shell=true；phone_reboot / phone_shutdown 需要 security.allow_power_commands=true。没打开时服务端返回 403，错误原文会透传给你。
- 如果配置里 mcp.read_only=true，工具表只会给出只读工具，写工具（点击/输入/上传/改配置/重启…）会被拒绝。
- 相机与麦克风依赖 Termux:API 桥（Termux:API 应用 + 权限 + 唤醒锁）；报错时先调 phone_bridge_fix 再重试。

调用约定：
- 所有工具最终都打到同一份 REST API，非 2xx 时响应体里的 error 字段会作为 content 文本原样返回，并把 isError 置为 true。
- 图片工具返回 image 内容块（base64 + mimeType），音频工具返回 audio 内容块，并都附一条说明尺寸/质量/字节数的文本。
- 屏幕实时画面（GET /api/v1/stream/screen，annexb-len-prefix 的 H.264 Annex-B）是长连接，不能通过 tools/call 返回；请用 HTTP 客户端直接连接。`

// ---------- sessions ----------

// mcpSessions is deliberately thin. The HTTP transport is stateless; the id
// exists so a client that expects a sticky Mcp-Session-Id gets one back and can
// keep sending it. Nothing else is keyed by it.
type mcpSessions struct {
	mu  sync.Mutex
	ids map[string]time.Time
}

func newMCPSessions() *mcpSessions { return &mcpSessions{ids: map[string]time.Time{}} }

func (s *mcpSessions) newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// A predictable id beats a missing header for a stateless endpoint.
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	id := hex.EncodeToString(b)
	s.touch(id)
	return id
}

func (s *mcpSessions) touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]time.Time{}
	}
	s.ids[id] = time.Now()
	if len(s.ids) <= 512 {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for k, t := range s.ids {
		if t.Before(cutoff) {
			delete(s.ids, k)
		}
	}
	// Still huge (someone is minting ids in a loop): the transport keeps no
	// state, so dropping the whole map costs nothing.
	if len(s.ids) > 8192 {
		s.ids = map[string]time.Time{}
	}
}

// ---------- JSON-RPC ----------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// contentBlock is an MCP tool result block: text, image or audio.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func textBlock(s string) contentBlock { return contentBlock{Type: "text", Text: s} }

func (r rpcRequest) idOrNull() json.RawMessage {
	if len(r.ID) == 0 {
		return json.RawMessage("null")
	}
	return r.ID
}

func resultResponse(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, code int, msg string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// ---------- server ----------

type mcpServer struct {
	baseURL string // REST base the tools call, e.g. http://127.0.0.1:8080
	cfgFn   func() Config
	logf    func(string, ...any)
}

func (s *mcpServer) log(format string, a ...any) {
	if s.logf != nil {
		s.logf(format, a...)
	}
}

var mcpHTTPClient = &http.Client{}

// mcpSelfBaseURL is where a tool call goes when no explicit URL was given: the
// loopback address the server itself bound, so the MCP layer talks to this very
// process and not to some other host.
func mcpSelfBaseURL(cfg Config) string {
	host, port := splitListen(cfg.effectiveListen())
	if port == "" {
		port = "8080"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	return "http://" + host + ":" + port
}

func trimFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// decodeJSON decodes a params blob with UseNumber, so an integer argument keeps
// its exact value all the way into the request body (a millisecond timestamp
// reconstructed as float64 would lose precision).
func decodeJSON(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(dst)
}

// ---------- dispatch ----------

type initParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      map[string]any `json:"clientInfo"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type listParams struct {
	Cursor string `json:"cursor"`
}

// process handles one payload: a single request object or a batch array.
// It reports whether the payload was a batch (the HTTP layer then answers with
// an array) and skips notifications entirely, which is what the transport spec
// requires — a notification gets no response object.
func (s *mcpServer) process(payload []byte) (resps []*rpcResponse, batch bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return []*rpcResponse{errorResponse(nil, -32700, "Parse error: empty request body")}, false
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return []*rpcResponse{errorResponse(nil, -32700, "Parse error: "+err.Error())}, true
		}
		if len(items) == 0 {
			return []*rpcResponse{errorResponse(nil, -32600, "Invalid Request: empty batch")}, true
		}
		out := make([]*rpcResponse, 0, len(items))
		for _, item := range items {
			if r, ok := s.handleMessage(item); ok {
				out = append(out, r)
			}
		}
		return out, true
	}
	r, ok := s.handleMessage(trimmed)
	if !ok {
		return nil, false
	}
	return []*rpcResponse{r}, false
}

func (s *mcpServer) handleMessage(raw json.RawMessage) (*rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, -32700, "Parse error: "+err.Error()), true
	}
	// A message without an id is a notification: per JSON-RPC it gets no answer.
	// An explicit "id": null is still a request, and is answered with id null.
	notification := len(req.ID) == 0

	switch {
	case req.Method == "initialize":
		return s.handleInitialize(req, notification)
	case req.Method == "ping":
		if notification {
			return nil, false
		}
		return resultResponse(req.idOrNull(), map[string]any{}), true
	case req.Method == "tools/list":
		if notification {
			return nil, false
		}
		var p listParams
		if err := decodeJSON(req.Params, &p); err != nil {
			return errorResponse(req.idOrNull(), -32602, "Invalid params: "+err.Error()), true
		}
		return resultResponse(req.idOrNull(), map[string]any{
			"tools":        toolListJSON(s.readOnly()),
			"read_only":    s.readOnly(),
			"tool_count":   len(toolListJSON(s.readOnly())),
			"server":       mcpServerName,
			"instructions": "read_only=true 时只列出不会改变手机状态的工具（when true, only tools that cannot change the phone's state are listed）",
		}), true
	case req.Method == "tools/call":
		if notification {
			return nil, false
		}
		return s.handleToolsCall(req)
	case strings.HasPrefix(req.Method, "notifications/"):
		// initialized / cancelled / progress: no answer, ever.
		if notification {
			return nil, false
		}
		return resultResponse(req.idOrNull(), map[string]any{}), true
	case strings.HasPrefix(req.Method, "$/"):
		// Reserved extension namespace; ignoring it is the spec's own advice.
		if notification {
			return nil, false
		}
		return resultResponse(req.idOrNull(), map[string]any{}), true
	default:
		if notification {
			return nil, false
		}
		return errorResponse(req.idOrNull(), -32601, "Method not found: "+req.Method), true
	}
}

func (s *mcpServer) readOnly() bool { return s.cfgFn().MCP.ReadOnly }

func (s *mcpServer) handleInitialize(req rpcRequest, notification bool) (*rpcResponse, bool) {
	if notification {
		return nil, false
	}
	var p initParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return errorResponse(req.idOrNull(), -32602, "Invalid params: "+err.Error()), true
	}
	proto := strings.TrimSpace(p.ProtocolVersion)
	if !mcpKnownProtocols[proto] {
		// Echo the client's revision when we speak it, otherwise name our newest
		// supported one and let the client decide what to do.
		proto = mcpProtocolFallback
	}
	return resultResponse(req.idOrNull(), map[string]any{
		"protocolVersion": proto,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo":   map[string]any{"name": mcpServerName, "version": version},
		"instructions": mcpInstructions,
	}), true
}

func (s *mcpServer) handleToolsCall(req rpcRequest) (*rpcResponse, bool) {
	var p callParams
	if err := decodeJSON(req.Params, &p); err != nil {
		return errorResponse(req.idOrNull(), -32602, "Invalid params: "+err.Error()), true
	}
	if strings.TrimSpace(p.Name) == "" {
		return errorResponse(req.idOrNull(), -32602, `Invalid params: "name" is required`), true
	}
	args := map[string]any{}
	if len(bytes.TrimSpace(p.Arguments)) > 0 {
		if err := decodeJSON(p.Arguments, &args); err != nil {
			return errorResponse(req.idOrNull(), -32602, "Invalid params: arguments must be an object: "+err.Error()), true
		}
		if args == nil {
			args = map[string]any{}
		}
	}

	tool, ok := findTool(p.Name)
	if !ok {
		return errorResponse(req.idOrNull(), -32602, "Invalid params: unknown tool "+p.Name), true
	}
	if s.readOnly() && !tool.readOnly {
		return resultResponse(req.idOrNull(), map[string]any{
			"content": []contentBlock{textBlock(fmt.Sprintf(
				"工具 %s 会改变手机状态，但 mcp.read_only=true，所以被拒绝（%s is a write tool; this server runs with mcp.read_only=true, so the call was refused. Set mcp.read_only=false to allow it.）. 只读工具可以用 tools/list 查看。",
				tool.name, tool.name))},
			"isError": true,
		}), true
	}

	blocks, isErr := s.invoke(context.Background(), tool, args)
	return resultResponse(req.idOrNull(), map[string]any{
		"content": blocks,
		"isError": isErr,
	}), true
}

// ---------- tool invocation ----------

func (s *mcpServer) invoke(ctx context.Context, t *mcpTool, args map[string]any) ([]contentBlock, bool) {
	out := make([]contentBlock, 0, 3)
	if t.notes != "" {
		out = append(out, textBlock(t.notes))
	}

	req, err := s.buildToolRequest(ctx, t, args)
	if err != nil {
		// A bad argument is the caller's fault, but it belongs in the tool
		// result so the model can read and fix it rather than guess.
		return append(out, textBlock("参数有误，请求未发出: "+err.Error())), true
	}
	ctx2, cancel := context.WithTimeout(ctx, s.timeoutFor(t, args))
	defer cancel()

	resp, err := mcpHTTPClient.Do(req.WithContext(ctx2))
	if err != nil {
		return append(out, textBlock("调用手机 API 失败（"+s.baseURL+"）: "+err.Error())), true
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxResponseBytes))
	if err != nil {
		return append(out, textBlock("读取手机 API 响应失败: "+err.Error())), true
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The server's own error text is the useful part — it names the config
		// switch (security.allow_shell, files.read_only, ...) or the missing
		// bridge permission. Pass it through unedited.
		return append(out, textBlock(apiErrorText(resp.StatusCode, data))), true
	}

	switch t.kind {
	case "image", "audio":
		blocks, err := mediaBlocks(t.kind, data)
		if err != nil {
			return append(out, textBlock("解析 "+t.name+" 的响应失败: "+err.Error())), true
		}
		return append(out, blocks...), false
	case "download":
		blocks, isErr := downloadBlocks(t, args, resp.Header.Get("Content-Type"), data)
		return append(out, blocks...), isErr
	default:
		return append(out, textBlock(mcpPrettyJSON(data))), false
	}
}

// timeoutFor picks the per-call timeout. control.max_timeout_ms is the default,
// but an audio recording is legitimately longer than that (up to
// capture.mic_max_seconds), so its own length sizes the deadline — otherwise a
// 300-second recording would be cut off by a 120-second client timeout.
func (s *mcpServer) timeoutFor(t *mcpTool, args map[string]any) time.Duration {
	cfg := s.cfgFn()
	ms := cfg.Control.MaxTimeoutMS
	if ms <= 0 {
		ms = 120000
	}
	d := time.Duration(ms) * time.Millisecond
	if t.kind != "audio" || t.secArg == "" {
		return d
	}
	secs := 0
	if v, ok := args[t.secArg]; ok {
		if n, err := strconv.Atoi(mcpStrVal(v)); err == nil {
			secs = n
		}
	}
	if secs <= 0 {
		secs = cfg.Capture.MicSeconds
	}
	if cfg.Capture.MicMaxSeconds > 0 && secs > cfg.Capture.MicMaxSeconds {
		secs = cfg.Capture.MicMaxSeconds
	}
	if need := time.Duration(secs+90) * time.Second; need > d {
		d = need
	}
	return d
}

func (s *mcpServer) buildToolRequest(ctx context.Context, t *mcpTool, args map[string]any) (*http.Request, error) {
	work := make(map[string]any, len(args))
	for k, v := range args {
		work[k] = v
	}

	path := t.path
	for _, name := range t.pathArgs {
		v, ok := work[name]
		if !ok {
			return nil, fmt.Errorf("缺少路径参数 %q", name)
		}
		path = strings.ReplaceAll(path, "{"+name+"}", escapePathValue(mcpStrVal(v)))
		delete(work, name)
	}

	q := url.Values{}
	for k, vs := range t.fixedQ {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	body := map[string]any{}
	for k, v := range t.fixedB {
		body[k] = v
	}

	// File upload is the one tool whose payload is the raw bytes, not JSON.
	var rawBody []byte
	rawContentType := ""
	if t.kind == "upload" {
		switch {
		case work["data_base64"] != nil:
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(mcpStrVal(work["data_base64"])))
			if err != nil {
				return nil, fmt.Errorf("data_base64 不是合法的 base64: %w", err)
			}
			rawBody = b
		case work["text"] != nil:
			rawBody = []byte(mcpStrVal(work["text"]))
		}
		delete(work, "data_base64")
		delete(work, "text")
		rawContentType = "application/octet-stream"
	}

	for name, v := range work {
		key := t.transportKey(name)
		if t.method == "GET" || t.queryArgs[name] {
			q.Set(key, mcpStrVal(v))
		} else {
			body[key] = v
		}
	}
	if t.kind == "image" || t.kind == "audio" {
		// Ask the API for metadata plus the bytes inline: that is exactly the
		// shape an MCP image/audio content block needs.
		q.Set("meta", "1")
		q.Set("base64", "1")
	}

	target := s.baseURL + path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}

	var reader io.Reader
	if rawBody != nil {
		reader = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, t.method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	if rawContentType != "" {
		req.Header.Set("Content-Type", rawContentType)
	}
	if len(body) > 0 && t.kind != "upload" {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码请求体失败: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.ContentLength = int64(len(b))
		req.Header.Set("Content-Type", "application/json")
	}
	// The token is what makes this an authenticated call; an empty token means
	// the server has auth disabled, so send nothing.
	if tok := s.cfgFn().APIToken; tok != "" {
		req.Header.Set("X-API-Key", tok)
	}
	return req, nil
}

// escapePathValue turns a share or relative path into safe URL path segments.
// Splitting on "/" first is what keeps a nested path working while still
// escaping every segment (so ".." or a space cannot smuggle anything through).
func escapePathValue(v string) string {
	parts := strings.Split(strings.Trim(v, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, url.PathEscape(p))
	}
	return strings.Join(out, "/")
}

// apiErrorText extracts the server's own error message. When the body carries
// the API's standard {"error": "..."} document, that string is returned exactly
// as written — it is the sentence that names the missing config switch.
func apiErrorText(code int, body []byte) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(body), &obj) == nil {
		for _, key := range []string{"error", "message"} {
			if raw, ok := obj[key]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 4096 {
		s = s[:4096] + "…"
	}
	if s == "" {
		return fmt.Sprintf("HTTP %d 且响应体为空（empty body）", code)
	}
	return fmt.Sprintf("HTTP %d: %s", code, s)
}

// mediaBlocks turns the API's meta+base64 JSON into MCP content blocks: the
// media itself, plus a text line so a model without image input still learns
// the dimensions, quality and size.
func mediaBlocks(kind string, raw []byte) ([]contentBlock, error) {
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %w", err)
	}
	data, _ := meta["data"].(string)
	if data == "" {
		// meta-only answer (or an error document that still came back 200):
		// hand the JSON through rather than inventing media that is not there.
		return []contentBlock{textBlock(mcpPrettyJSON(raw))}, nil
	}
	mime, _ := meta["mime"].(string)
	blockType, fallbackMime := "image", "image/webp"
	if kind == "audio" {
		blockType, fallbackMime = "audio", "audio/mp4"
	}
	if mime == "" {
		mime = fallbackMime
	}
	return []contentBlock{
		{Type: blockType, Data: data, MimeType: mime},
		textBlock(describeMedia(kind, meta, mime)),
	}, nil
}

func describeMedia(kind string, meta map[string]any, mime string) string {
	field := func(key string) string {
		v, ok := meta[key]
		if !ok || v == nil {
			return ""
		}
		switch n := v.(type) {
		case string:
			return n
		case json.Number:
			return n.String()
		case float64:
			return trimFloat(n)
		case bool:
			if n {
				return "true"
			}
			return "false"
		default:
			return fmt.Sprintf("%v", n)
		}
	}
	var b strings.Builder
	if kind == "audio" {
		b.WriteString("音频内容块已内联（audio block inlined）")
		if s := field("seconds"); s != "" {
			fmt.Fprintf(&b, "；时长 %s 秒", s)
		}
	} else {
		w, h := field("width"), field("height")
		if w != "" && h != "" {
			fmt.Fprintf(&b, "图片内容块已内联（image block inlined）：%sx%s 像素", w, h)
		} else {
			b.WriteString("图片内容块已内联（image block inlined）")
		}
		if f := field("format"); f != "" {
			fmt.Fprintf(&b, "，格式 %s", f)
		}
		if q := field("quality"); q != "" && q != "null" {
			fmt.Fprintf(&b, "，质量 %s", q)
		}
		if sw := field("source_width"); sw != "" && field("source_height") != "" {
			fmt.Fprintf(&b, "，源分辨率 %sx%s", sw, field("source_height"))
		}
	}
	fmt.Fprintf(&b, "，mime %s", mime)
	if n := field("bytes"); n != "" {
		fmt.Fprintf(&b, "，%s 字节", n)
	}
	b.WriteString("。内容不入库、不推送（never stored, never pushed）。")
	return b.String()
}

// downloadBlocks answers the file-download tool. It chooses the block type from
// the file's own content type, and says so rather than silently base64-ing a
// binary into a text block the caller cannot use.
func downloadBlocks(t *mcpTool, args map[string]any, contentType string, data []byte) ([]contentBlock, bool) {
	ct := strings.TrimSpace(strings.ToLower(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if v, ok := args["max_bytes"]; ok {
		if max, err := strconv.Atoi(mcpStrVal(v)); err == nil && max > 0 && len(data) > max {
			return []contentBlock{textBlock(fmt.Sprintf(
				"文件共 %d 字节，超过 max_bytes=%d，未返回内容；请提高 max_bytes（file is %d bytes, over the cap）",
				len(data), max, len(data)))}, true
		}
	}
	forceB64 := false
	if v, ok := args["base64"]; ok {
		forceB64 = mcpStrVal(v) == "true"
	}
	switch {
	case ct == "":
		ct = "application/octet-stream"
	}
	if strings.HasPrefix(ct, "image/") && !forceB64 {
		return []contentBlock{
			{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: ct},
			textBlock(fmt.Sprintf("图片文件已内联：%d 字节，Content-Type %s（image inlined as base64）", len(data), ct)),
		}, false
	}
	if !forceB64 && (strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml")) {
		return []contentBlock{textBlock(string(data))}, false
	}
	return []contentBlock{textBlock(fmt.Sprintf(
		"文件 %d 字节，Content-Type %s，以下为 base64（binary payload as base64）:\n%s",
		len(data), ct, base64.StdEncoding.EncodeToString(data)))}, false
}

// toolListJSON is what tools/list returns. In read-only mode the mutating tools
// are simply absent — an agent cannot call what it cannot see.
func toolListJSON(readOnly bool) []map[string]any {
	out := make([]map[string]any, 0, len(mcpToolTable))
	for _, t := range mcpToolTable {
		if readOnly && !t.readOnly {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.desc,
			"inputSchema": t.schema,
		})
	}
	return out
}

// ---------- stdio transport ----------

var errLineTooLong = errors.New("request line exceeds the size limit")

// readLimitedLine reads one newline-terminated line, refusing to buffer more
// than max bytes. A line over the cap is dropped whole (and reported), so a bad
// line cannot desynchronise the stream by leaving half of itself for the next
// read.
func readLimitedLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// runMCPStdio is the `phoneserver -mcp` entry point: a JSON-RPC server on stdio.
// It returns the process exit code.
func runMCPStdio(cfgPath string, cfg Config, apiURL string) int {
	base := strings.TrimSpace(apiURL)
	if base == "" {
		base = mcpSelfBaseURL(cfg)
	}
	base = strings.TrimRight(base, "/")

	srv := &mcpServer{
		baseURL: base,
		cfgFn:   func() Config { return cfg },
		logf: func(format string, a ...any) {
			// stderr only: stdout is the protocol channel.
			fmt.Fprintf(os.Stderr, "phoneserver mcp: "+format+"\n", a...)
		},
	}
	srv.log("stdio transport ready: api=%s config=%s tools=%d protocol=%s",
		base, cfgPath, len(mcpToolTable), mcpProtocolFallback)

	in := bufio.NewReaderSize(os.Stdin, 256<<10)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	for {
		line, err := readLimitedLine(in, mcpMaxLineBytes)
		if errors.Is(err, errLineTooLong) {
			srv.log("stdin: dropping a request line larger than %d bytes", mcpMaxLineBytes)
			if werr := enc.Encode(errorResponse(nil, -32700,
				fmt.Sprintf("Parse error: the request line exceeds the %d byte limit", mcpMaxLineBytes))); werr != nil {
				srv.log("stdout write failed: %v", werr)
				return 2
			}
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				srv.log("stdin closed; exiting")
				return 0
			}
			srv.log("stdin read failed: %v", err)
			return 2
		}

		if len(bytes.TrimSpace(line)) > 0 {
			resps, batch := srv.process(line)
			if batch {
				if werr := enc.Encode(resps); werr != nil {
					srv.log("stdout write failed: %v", werr)
					return 2
				}
			} else {
				for _, r := range resps {
					if werr := enc.Encode(r); werr != nil {
						srv.log("stdout write failed: %v", werr)
						return 2
					}
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				srv.log("stdin closed; exiting")
				return 0
			}
			srv.log("stdin read failed: %v", err)
			return 2
		}
	}
}

// ---------- HTTP transport ----------

// handleMCPHTTP is the POST /mcp endpoint. main.go registers it without an
// outer guard, so the token check happens here.
func (a *App) handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(errorResponse(nil, -32600,
			"Invalid Request: this MCP endpoint is stateless — it only accepts POST with a JSON-RPC 2.0 document (single object or batch array). A GET has nothing to return here."))
		return
	}
	if !a.tokenOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="phoneserver"`)
		writeErr(w, http.StatusUnauthorized,
			"missing or invalid api token (send X-API-Key, Authorization: *** or ?key=)")
		return
	}
	if a.mcpSessions == nil {
		a.mcpSessions = newMCPSessions()
	}
	sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
	if sid == "" {
		sid = a.mcpSessions.newID()
	} else {
		a.mcpSessions.touch(sid)
	}
	w.Header().Set("Mcp-Session-Id", sid)
	w.Header().Set("X-Phoneserver-Version", version)

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxHTTPBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			errorResponse(nil, -32700, "Parse error: reading the request body failed: "+err.Error()))
		return
	}

	cfg := a.config()
	srv := &mcpServer{
		baseURL: mcpSelfBaseURL(cfg),
		cfgFn:   a.config,
		logf: func(format string, args ...any) {
			if a.logf != nil {
				a.logf.Printf(format, args...)
			}
		},
	}
	resps, batch := srv.process(body)

	// A batch or single notification has no response object at all; answer 202
	// so the client's HTTP call still completes.
	if len(resps) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var payload any
	if batch {
		payload = resps
	} else {
		payload = resps[0]
	}
	out, merr := json.Marshal(payload)
	if merr != nil {
		writeErr(w, http.StatusInternalServerError, "marshal MCP response: "+merr.Error())
		return
	}

	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		// One event, then the stream ends: this endpoint has no server-initiated
		// traffic, so holding the connection open would only mislead a client.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", out)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(append(out, '\n'))
}
