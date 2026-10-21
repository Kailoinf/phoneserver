package main

// mcptools.go — the MCP tool table.
//
// One entry per REST capability. A tool is a thin, declarative description
// (name, one-line description, JSON Schema, HTTP target); mcp.go turns it into a
// real request against the same REST API the WebUI uses. Keeping the tools
// declarative matters: the MCP surface then cannot drift from the phone's actual
// capabilities, and a config gate (security.allow_shell, files.read_only, ...)
// still answers for itself — the agent sees the server's own error text.
//
// A tool is marked read-only (`.ro()`) only when it cannot change the phone's
// state. Anything that can mutate (input injection, config writes, uploads,
// shell, reboot, sending notifications) is a write tool, and write tools are
// hidden from tools/list when mcp.read_only=true. The default is "write" on
// purpose: forgetting `.ro()` hides a tool in read-only mode, which is
// recoverable, whereas forgetting to exclude a mutating tool is not.
//
// Descriptions are one Chinese sentence with an English gloss in parentheses,
// because a model often knows the English term for a capability even when the
// prompt is Chinese.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type mcpTool struct {
	name     string
	desc     string
	notes    string // text block prepended to every result (protocol caveats, mount hints)
	readOnly bool
	method   string // GET | POST | PUT
	path     string // may carry {share}/{path} template parameters
	kind     string // json | image | audio | download | upload

	// secArg names the argument holding a recording length; an audio tool can
	// legitimately run longer than control.max_timeout_ms, so the timeout is
	// sized from it (see mcpTimeout).
	secArg string

	props     map[string]any
	required  []string
	fixedQ    url.Values        // query parameters always sent
	fixedB    map[string]any    // body fields always sent (POST/PUT)
	rename    map[string]string // tool argument -> transport key
	queryArgs map[string]bool   // arguments that go to the query string even on POST
	pathArgs  []string          // arguments consumed into the path template
	schema    map[string]any
}

// ---------- builder ----------

type toolBuilder struct{ t *mcpTool }

func newTool(name, desc, method, path string) *toolBuilder {
	return &toolBuilder{t: &mcpTool{
		name: name, desc: desc, method: method, path: path, kind: "json",
		props: map[string]any{}, fixedQ: url.Values{}, fixedB: map[string]any{},
		rename: map[string]string{}, queryArgs: map[string]bool{},
	}}
}

func (b *toolBuilder) prop(name, typ, desc string) *toolBuilder {
	b.t.props[name] = map[string]any{"type": typ, "description": desc}
	return b
}

func (b *toolBuilder) str(n, d string) *toolBuilder { return b.prop(n, "string", d) }
func (b *toolBuilder) int(n, d string) *toolBuilder { return b.prop(n, "integer", d) }
func (b *toolBuilder) num(n, d string) *toolBuilder { return b.prop(n, "number", d) }
func (b *toolBuilder) bl(n, d string) *toolBuilder  { return b.prop(n, "boolean", d) }

func (b *toolBuilder) enum(n, d string, vals ...string) *toolBuilder {
	b.t.props[n] = map[string]any{"type": "string", "description": d, "enum": vals}
	return b
}

func (b *toolBuilder) arrStr(n, d string) *toolBuilder {
	b.t.props[n] = map[string]any{
		"type": "array", "items": map[string]any{"type": "string"}, "description": d,
	}
	return b
}

// arrPairs is the [x,y] point list that /api/v1/input/gesture takes.
func (b *toolBuilder) arrPairs(n, d string) *toolBuilder {
	b.t.props[n] = map[string]any{
		"type": "array", "description": d,
		"items": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
	}
	return b
}

// obj is a loosely-typed object (a config section, an extras bundle): the inner
// shape belongs to the phone's config, not to this schema.
func (b *toolBuilder) obj(n, d string) *toolBuilder {
	b.t.props[n] = map[string]any{"type": "object", "description": d, "additionalProperties": true}
	return b
}

func (b *toolBuilder) mapStr(n, d string) *toolBuilder {
	b.t.props[n] = map[string]any{
		"type": "object", "description": d,
		"additionalProperties": map[string]any{"type": "string"},
	}
	return b
}

func (b *toolBuilder) need(names ...string) *toolBuilder {
	b.t.required = append(b.t.required, names...)
	return b
}

func (b *toolBuilder) ro() *toolBuilder { b.t.readOnly = true; return b }

func (b *toolBuilder) q(k, v string) *toolBuilder        { b.t.fixedQ.Set(k, v); return b }
func (b *toolBuilder) body(k string, v any) *toolBuilder { b.t.fixedB[k] = v; return b }
func (b *toolBuilder) ren(from, to string) *toolBuilder  { b.t.rename[from] = to; return b }

func (b *toolBuilder) qarg(names ...string) *toolBuilder {
	for _, n := range names {
		b.t.queryArgs[n] = true
	}
	return b
}

func (b *toolBuilder) pArg(names ...string) *toolBuilder {
	b.t.pathArgs = append(b.t.pathArgs, names...)
	return b
}

func (b *toolBuilder) as(kind string) *toolBuilder { b.t.kind = kind; return b }
func (b *toolBuilder) sec(arg string) *toolBuilder { b.t.secArg = arg; return b }
func (b *toolBuilder) note(s string) *toolBuilder  { b.t.notes = s; return b }

func (b *toolBuilder) done() *mcpTool {
	if b.t.required == nil {
		b.t.required = []string{}
	}
	b.t.schema = map[string]any{
		"type":                 "object",
		"properties":           b.t.props,
		"required":             b.t.required,
		"additionalProperties": false,
	}
	return b.t
}

// ---------- validation ----------

var schemaTypes = map[string]bool{
	"object": true, "array": true, "string": true,
	"integer": true, "number": true, "boolean": true, "null": true,
}

// validateToolSchema checks the shape an MCP client is entitled to assume. It is
// used by the unit test, and is cheap enough that a future wiring mistake shows
// up in CI rather than in an agent's tool list.
func validateToolSchema(t *mcpTool) error {
	if t.name == "" {
		return fmt.Errorf("tool with an empty name")
	}
	if t.desc == "" {
		return fmt.Errorf("%s: empty description", t.name)
	}
	typ, _ := t.schema["type"].(string)
	if typ != "object" {
		return fmt.Errorf("%s: inputSchema.type is %q, want \"object\"", t.name, typ)
	}
	props, ok := t.schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return fmt.Errorf("%s: inputSchema.properties is missing or empty", t.name)
	}
	if extra, ok := t.schema["additionalProperties"].(bool); !ok || extra {
		return fmt.Errorf("%s: inputSchema.additionalProperties must be false", t.name)
	}
	req, ok := t.schema["required"].([]string)
	if !ok {
		return fmt.Errorf("%s: inputSchema.required must be a string array", t.name)
	}
	for _, r := range req {
		if _, ok := props[r]; !ok {
			return fmt.Errorf("%s: required %q is not a declared property", t.name, r)
		}
	}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: property %q is not an object", t.name, name)
		}
		pt, _ := p["type"].(string)
		if !schemaTypes[pt] {
			return fmt.Errorf("%s: property %q has type %q", t.name, name, pt)
		}
		if e, ok := p["enum"].([]string); ok && len(e) == 0 {
			return fmt.Errorf("%s: property %q has an empty enum", t.name, name)
		}
	}
	if t.method != "GET" && t.method != "POST" && t.method != "PUT" {
		return fmt.Errorf("%s: unsupported method %q", t.name, t.method)
	}
	return nil
}

// ---------- lookup ----------

var mcpToolTable = buildMCPTools()

func allTools() []*mcpTool { return mcpToolTable }

var mcpToolIndex = func() map[string]*mcpTool {
	m := map[string]*mcpTool{}
	for _, t := range mcpToolTable {
		m[t.name] = t
	}
	return m
}()

func findTool(name string) (*mcpTool, bool) {
	t, ok := mcpToolIndex[name]
	return t, ok
}

// transportKey maps a tool argument onto the REST field name, when they differ.
func (t *mcpTool) transportKey(arg string) string {
	if k, ok := t.rename[arg]; ok {
		return k
	}
	return arg
}

// ---------- the table ----------
//
// Paths and body fields here are copied from the authoritative endpoint index in
// main.go's handleEndpoints and from the handlers themselves; nothing is guessed.

func buildMCPTools() []*mcpTool {
	tools := []*mcpTool{
		// ----- telemetry -----
		newTool("phone_get_all",
			"一次性读取全部遥测段落（every telemetry section in one document）",
			"GET", "/api/v1/all").ro().done(),
		newTool("phone_device_info",
			"读取设备身份、ROM、内核与 build 信息（device identity, ROM, kernel, build）",
			"GET", "/api/v1/device").ro().done(),
		newTool("phone_battery",
			"读取电量、健康度、循环次数、电流与温度（battery charge, health, cycles, currents）",
			"GET", "/api/v1/battery").ro().done(),
		newTool("phone_cpu",
			"读取每核心频率、使用率、调频器与负载（per-core frequency, usage, governor, loadavg）",
			"GET", "/api/v1/cpu").ro().done(),
		newTool("phone_gpu",
			"读取 GPU 占用率、频率与温度（GPU busy %, clock, temperature）",
			"GET", "/api/v1/gpu").ro().done(),
		newTool("phone_memory",
			"读取内存与交换区用量（memory and swap usage from /proc/meminfo）",
			"GET", "/api/v1/memory").ro().done(),
		newTool("phone_storage",
			"读取各挂载点的容量与剩余空间（statvfs per configured mount）",
			"GET", "/api/v1/storage").ro().done(),
		newTool("phone_thermal",
			"读取全部温度区，单位摄氏度（all thermal zones in Celsius）",
			"GET", "/api/v1/thermal").ro().done(),
		newTool("phone_network",
			"读取网络接口、计数器与实时速率（interfaces, counters, live rates）",
			"GET", "/api/v1/network").ro().done(),
		newTool("phone_wifi",
			"读取 SSID、信号强度、链路速率与频段（SSID, RSSI, link speed, frequency）",
			"GET", "/api/v1/network/wifi").ro().done(),
		newTool("phone_processes",
			"读取进程总数与内存占用最高的进程（process count and top processes by RSS）",
			"GET", "/api/v1/processes").ro().done(),
		newTool("phone_packages",
			"读取已安装应用数量（installed package counts, system vs data）",
			"GET", "/api/v1/packages").ro().done(),
		newTool("phone_props",
			"读取完整 getprop 属性表，1000+ 键（full getprop dump）",
			"GET", "/api/v1/props").ro().done(),
		newTool("phone_system",
			"读取开机时长、内核、SELinux 与负载（uptime, kernel, selinux, loadavg）",
			"GET", "/api/v1/system").ro().done(),

		// ----- screen -----
		newTool("phone_screenshot",
			"截取当前屏幕并返回图片块（current screen as an image block）",
			"GET", "/api/v1/screenshot").ro().as("image").
			enum("format", "图片格式，默认 webp（image format）", "webp", "jpeg", "png").
			int("quality", "1-100，默认 80（lossy quality）").
			num("scale", "0.1-1.0 缩放比例，默认 1（downscale factor）").
			str("display", "内屏编号，默认主屏（display id, e.g. 0 or 4630947042512727040）").
			note("返回一个 webp 图片块 + 一条文本说明尺寸/质量/字节数。截图只在本次请求内存在，不入库、不推送。FLAG_SECURE 窗口截出来是黑帧。").
			done(),
		newTool("phone_screen_stream_info",
			"查询屏幕串流状态并说明长连接协议（stream status + H.264 Annex-B framing）",
			"GET", "/api/v1/stream/status").ro().
			note("实时画面走 GET /api/v1/stream/screen?size=720x1560&bitrate=3000000&wake=1，响应是 chunked 的 H.264 Annex-B（annexb-len-prefix：每个 NAL 前缀 4 字节大端长度，没有 00 00 00 01 起始码）。这是长连接，tools/call 只能返回一次性结果，所以请用 HTTP 客户端（curl -N / ffmpeg）直接连接，不要用本工具拉流；MJPEG 回退在 /api/v1/stream/screen/mjpeg。").
			done(),
		newTool("phone_screen_display_state",
			"查询屏幕是否点亮、screenrecord 当前是否可用（panel awake, screenrecord availability）",
			"GET", "/api/v1/stream/display").ro().done(),

		// ----- input injection -----
		newTool("phone_tap",
			"点击屏幕坐标（tap at absolute screen coordinates）",
			"POST", "/api/v1/input/tap").
			int("x", "横坐标（x pixel）").int("y", "纵坐标（y pixel）").
			int("display", "可选，内屏编号（display id）").
			need("x", "y").done(),
		newTool("phone_swipe",
			"从一点滑动到另一点（swipe from (x1,y1) to (x2,y2)）",
			"POST", "/api/v1/input/swipe").
			int("x1", "起点横坐标（start x）").int("y1", "起点纵坐标（start y）").
			int("x2", "终点横坐标（end x）").int("y2", "终点纵坐标（end y）").
			int("duration_ms", "滑动时长毫秒，默认 300（duration in ms）").
			need("x1", "y1", "x2", "y2").done(),
		newTool("phone_input_text",
			"向焦点输入框输入文本（type text into the focused field）",
			"POST", "/api/v1/input/text").
			str("text", "要输入的文本（text to type）").need("text").
			note("纯 ASCII 走 /system/bin/input text；非 ASCII（例如中文）走剪贴板 + KEYCODE_PASTE(279)，需要 Termux:API 桥可用，否则返回 503 并说明。先确认目标输入框已获得焦点。").
			done(),
		newTool("phone_key",
			"发送一个按键事件（send one key event by name or code）",
			"POST", "/api/v1/input/key").
			str("key", "按键名，例如 HOME、BACK、ENTER、KEYCODE_VOLUME_UP（key name）").
			int("code", "或用数字键码，例如 66 = ENTER（numeric keycode）").
			bl("longpress", "长按（hold the key down）").
			int("duration_ms", "长按时长毫秒（hold duration）").
			done(),
		newTool("phone_input_keys",
			"连续发送多个按键（send several key events in one call）",
			"POST", "/api/v1/input/keys").
			arrStr("keys", "按键名列表，例如 [\"HOME\",\"BACK\"]（key names）").
			bl("longpress", "长按（hold each key）").
			int("duration_ms", "长按时长毫秒（hold duration）").
			done(),
		newTool("phone_input_gesture",
			"沿多点路径划动（chained swipes along a point path）",
			"POST", "/api/v1/input/gesture").
			arrPairs("points", "坐标点列表，例如 [[100,200],[300,400]]（point path）").
			int("duration_ms", "总时长毫秒，默认 400（total duration in ms）").
			int("steps", "每段中间的 MOVE 事件数（intermediate move events per segment）").
			need("points").done(),

		// ----- UI automation -----
		newTool("phone_ui_dump",
			"导出当前 UI 层级（uiautomator hierarchy: text, bounds, centre）",
			"GET", "/api/v1/ui/dump").
			bl("all", "true 时返回全部节点而不是只返回可交互节点（return every node）").
			int("max_age_ms", "接受多旧的缓存 dump，毫秒（accept a cached dump up to this old）").
			done(),
		newTool("phone_ui_find",
			"按文本/描述/资源 id/类名查找控件（find elements by text/desc/id/class）",
			"GET", "/api/v1/ui/find").
			str("text", "控件文本（element text）").
			str("desc", "content-desc（content description）").
			str("resource_id", "资源 id，例如 com.x:id/btn（view id）").
			str("class", "类名，例如 android.widget.Button（class name）").
			bl("contains", "子串匹配而不是全等（substring match）").
			bl("clickable", "只匹配可点击元素（clickable only）").
			bl("first", "只返回第一条匹配（only the first match）").
			done(),
		newTool("phone_ui_click",
			"查找控件并点击其中心（find an element and tap its centre）",
			"POST", "/api/v1/ui/click").
			str("text", "控件文本（element text）").
			str("desc", "content-desc（content description）").
			str("resource_id", "资源 id（view id）").
			str("class", "类名（class name）").
			bl("contains", "子串匹配（substring match）").
			int("index", "匹配到多个时取第几个，从 0 开始（which match to tap）").
			bl("long_press", "长按（long press）").
			int("duration_ms", "长按时长毫秒，默认 600（long-press duration）").
			done(),

		// ----- apps -----
		newTool("phone_apps",
			"列出已安装且可启动的应用（installed packages with launchable activities）",
			"GET", "/api/v1/apps").
			str("grep", "按包名子串过滤（package name substring filter）").
			int("limit", "最多返回条数（max entries）").
			str("package", "顺带解析该包的启动 Activity（also resolve this package's launch activity）").
			done(),
		newTool("phone_app_launch",
			"启动应用或指定 Activity（launch a package or an explicit activity）",
			"POST", "/api/v1/apps/launch").
			str("package", "包名，例如 com.android.settings（package name）").
			str("activity", "可选，完整 Activity 名 a.B（explicit activity）").
			str("action", "可选，Intent action（intent action）").
			str("data", "可选，Intent data URI（intent data）").
			mapStr("extras", "可选，Intent extras（string map）").
			int("wait_ms", "启动后等待毫秒（wait after launch）").
			bl("force_stop_first", "先强停再启动（force-stop before launch）").
			need("package").done(),
		newTool("phone_app_stop",
			"停止应用（stop a package, optionally kill its processes）",
			"POST", "/api/v1/apps/stop").
			str("package", "包名（package name）").need("package").
			bl("kill", "连同进程一起杀（also kill processes）").
			done(),
		newTool("phone_current_app",
			"读取前台应用与 Activity（foreground package + activity）",
			"GET", "/api/v1/apps/current").ro().done(),
		newTool("phone_apps_activity",
			"读取 Activity/任务栈摘要（activity and task stack summary）",
			"GET", "/api/v1/apps/activity").ro().done(),

		// ----- device control -----
		newTool("phone_screen_power",
			"开关或唤醒/休眠屏幕（screen on/off/toggle/wake/sleep）",
			"POST", "/api/v1/control/screen").
			enum("action", "on=唤醒，off=休眠，toggle=取反，默认 toggle（screen action）",
				"on", "off", "toggle", "wake", "sleep").
			note("唤醒屏幕不会解锁手机；有锁屏时应用级自动化仍会被挡住。").
			done(),
		newTool("phone_volume",
			"调节音量（volume up/down/mute on the active stream）",
			"POST", "/api/v1/control/volume").
			enum("action", "up=加，down=减，mute=静音（volume action）", "up", "down", "mute", "toggle_mute").
			need("action").
			note("音量键作用于当前活动音频流；读取各流的具体音量用 phone_audio。").
			done(),
		newTool("phone_brightness",
			"设置屏幕亮度（set screen brightness by absolute value or percent）",
			"POST", "/api/v1/control/brightness").
			int("value", "面板原始范围内的绝对值（absolute brightness in the panel's range）").
			int("percent", "按百分比设置，0-100（set by percent）").
			bl("auto", "交给系统自动亮度（hand control back to auto-brightness）").
			done(),
		newTool("phone_rotation",
			"设置屏幕旋转（set rotation 0-3 or auto）",
			"POST", "/api/v1/control/rotation").
			str("value", "0-3 或 \"auto\"（rotation: 0..3 or auto）").
			bl("lock", "锁定当前方向（lock the orientation）").
			need("value").done(),
		newTool("phone_torch",
			"开关手电筒（torch on/off）",
			"POST", "/api/v1/control/torch").
			bl("on", "true 打开，false 关闭（torch state）").need("on").done(),
		newTool("phone_vibrate",
			"震动指定毫秒（vibrate for N milliseconds）",
			"POST", "/api/v1/control/vibrate").
			int("ms", "震动时长毫秒（duration in ms）").need("ms").done(),
		newTool("phone_toast",
			"在屏幕上显示一条短提示（show a toast message）",
			"POST", "/api/v1/control/toast").
			str("text", "提示文本（toast text）").need("text").
			bl("short", "短提示（short duration）").
			str("position", "位置，例如 center/bottom（gravity）").
			done(),
		newTool("phone_clipboard_get",
			"读取当前剪贴板内容（read the system clipboard）",
			"POST", "/api/v1/control/clipboard").ro().
			body("action", "get").
			note("读剪贴板需要一个可聚焦的输入法状态；Android 10+ 只有前台应用能读，失败时服务端会说明。").
			done(),
		newTool("phone_clipboard_set",
			"写入剪贴板（put text on the system clipboard）",
			"POST", "/api/v1/control/clipboard").
			str("text", "要写入的文本（text to place on the clipboard）").need("text").
			body("action", "set").done(),
		newTool("phone_tts",
			"用系统 TTS 朗读文本（speak text through the system TTS engine）",
			"POST", "/api/v1/control/tts").
			str("text", "要朗读的文本（text to speak）").need("text").
			num("rate", "语速倍数（speech rate multiplier）").
			num("pitch", "音调倍数（pitch multiplier）").
			str("lang", "语言标签，例如 zh-CN（BCP-47 language tag）").
			done(),
		newTool("phone_ir_send",
			"通过红外发射器发送红外码（transmit an IR pattern via the blaster）",
			"POST", "/api/v1/control/ir").
			int("freq_hz", "载波频率，例如 38000（carrier frequency in Hz）").
			str("pattern", "逗号分隔的 on/off 微秒序列，例如 \"9000,4500,560,560\"（durations in microseconds）").
			int("repeat", "重复次数，-1 表示一直发（repeat count, -1 = forever）").
			bl("frequencies", "只列出设备支持的红外频率（list supported frequencies only）").
			done(),
		newTool("phone_media",
			"控制媒体播放（media play/pause/stop/next/prev/info）",
			"POST", "/api/v1/control/media").
			enum("action", "播放控制动作（playback action）",
				"play", "pause", "stop", "next", "prev", "info").
			str("file", "播放指定文件（play this file instead）").
			need("action").done(),
		newTool("phone_wifi_toggle",
			"开关 Wi-Fi（turn Wi-Fi on/off, or switch SSID）",
			"POST", "/api/v1/control/wifi").
			bl("on", "true 打开，false 关闭（Wi-Fi state）").
			str("ssid", "可选，连接到指定 SSID（connect to this SSID）").
			done(),
		newTool("phone_sensors",
			"列出传感器或读取样本（list sensors, or read samples from one）",
			"GET", "/api/v1/control/sensors").
			str("sensor", "传感器名，留空则列出全部（sensor name; omit to list）").
			int("n", "读取样本数，最多 50（how many samples to read）").
			done(),
		newTool("phone_location",
			"读取最后一次定位（last known location via the bridge）",
			"GET", "/api/v1/control/location").
			str("provider", "定位来源，例如 gps/network（location provider）").
			str("request", "单次还是持续，例如 once/last（request type）").
			done(),
		newTool("phone_audio",
			"读取音频流、音量与静音状态（audio streams, volumes, mute state）",
			"GET", "/api/v1/control/audio").ro().done(),

		// ----- notifications & messages -----
		newTool("phone_notifications",
			"读取通知栏内容（posted notifications: title, text, package, when）",
			"GET", "/api/v1/notifications").
			bl("include_ongoing", "包含常驻通知，例如播放器（include ongoing notifications）").
			int("limit", "最多返回条数（max entries）").
			str("grep", "在标题/正文/包名/渠道里做子串过滤（substring filter）").
			str("package", "只看某个包的通知（filter by package）").
			int("since_ms", "只看这个毫秒时间戳之后的通知（only notifications newer than this）").
			bl("raw", "返回解析前的原始 dumpsys 文本（return the raw dumpsys text）").
			done(),
		newTool("phone_notification_post",
			"以手机身份发一条通知（post a notification from the phone）",
			"POST", "/api/v1/notifications/post").
			str("text", "通知正文（notification body）").need("text").
			str("title", "通知标题（title）").
			str("id", "通知 tag，默认 phoneserver（notification tag）").
			str("channel", "通知渠道（notification channel）").
			enum("style", "展开样式（collapse style）", "bigtext", "inbox").
			done(),
		newTool("phone_sms",
			"读取短信（read SMS messages from the device）",
			"GET", "/api/v1/messages/list").ro().q("source", "sms").
			int("limit", "最多返回条数，默认 50（max rows）").
			str("where", "可选的 provider where 子句（raw where clause）").
			done(),
		newTool("phone_calls",
			"读取通话记录（read the call log）",
			"GET", "/api/v1/messages/list").ro().q("source", "calls").
			int("limit", "最多返回条数，默认 50（max rows）").
			str("where", "可选的 provider where 子句（raw where clause）").
			done(),
		newTool("phone_contacts",
			"读取联系人（read contacts）",
			"GET", "/api/v1/messages/list").ro().q("source", "contacts").
			int("limit", "最多返回条数，默认 50（max rows）").
			str("where", "可选的 provider where 子句（raw where clause）").
			done(),
		newTool("phone_messages_search",
			"在短信/通话/联系人里搜索（search device messages for a term）",
			"GET", "/api/v1/messages/search").
			str("q", "搜索词（search term）").need("q").
			enum("source", "搜索哪一类，默认 sms（which source）", "sms", "calls", "contacts").
			int("limit", "最多返回条数，默认 50（max rows）").
			done(),

		// ----- camera & microphone -----
		newTool("phone_camera_photo",
			"拍照并返回图片块（take a photo and return it as an image block）",
			"GET", "/api/v1/camera/photo").ro().as("image").
			int("camera", "相机编号，0 通常是后置（camera id）").
			enum("format", "输出格式（output format）", "raw", "jpeg", "webp", "png").
			int("quality", "1-100 有损质量（lossy quality）").
			int("maxwidth", "最大宽度像素，0 表示原始分辨率（max width, 0 = sensor size）").
			note("需要 Termux:API 桥可用（相机权限 + 唤醒锁）；失败时返回体里的 hint 会给出 bridge/fix。返回一个图片块 + 一条文本说明尺寸/质量/字节数，照片不入库、不推送。").
			done(),
		newTool("phone_camera_info",
			"读取相机能力（camera capabilities via the bridge）",
			"GET", "/api/v1/camera/info").ro().done(),
		newTool("phone_mic_record",
			"录制若干秒音频并返回音频块（record N seconds and return an audio block）",
			"GET", "/api/v1/mic/record").ro().as("audio").sec("seconds").
			int("seconds", "录制秒数，上限 300（record length in seconds）").
			enum("encoder", "编码器，默认 aac（audio codec）", "aac", "opus", "amr_wb", "amr_nb").
			int("bitrate", "比特率 kbps（bitrate in kbps）").
			int("rate", "采样率 Hz（sample rate）").
			int("channels", "声道数 1-2（channels）").
			note("需要 Termux:API 桥与唤醒锁（否则麦克风打不开）。返回一个音频块（audio/mp4）+ 一条文本说明秒数/字节数，录音不入库、不推送。").
			done(),
		newTool("phone_mic_status",
			"读取当前录音状态（current microphone recording state）",
			"GET", "/api/v1/mic/status").ro().done(),
		newTool("phone_mic_start",
			"开始后台录音（start a background recording）",
			"POST", "/api/v1/mic/start").
			int("seconds", "0 表示手动停止（0 = stop manually）").
			str("encoder", "编码器，默认 aac（audio codec）").
			int("bitrate_kbps", "比特率 kbps（bitrate）").
			int("rate_hz", "采样率 Hz（sample rate）").
			int("channels", "声道数（channels）").
			done(),
		newTool("phone_mic_stop",
			"停止录音并取回文件（stop the recording and return the file）",
			"POST", "/api/v1/mic/stop").done(),

		// ----- files & WebDAV -----
		newTool("phone_files_shares",
			"列出已配置的文件共享名（the configured file shares）",
			"GET", "/files/").ro().
			note("共享是一个白名单：URL 里永远只有共享名，没有绝对路径，所以不存在目录穿越。下载/上传请用 phone_files_list、phone_file_download、phone_file_upload，路径写成 <share>/<相对路径>。").
			done(),
		newTool("phone_files_list",
			"以 JSON 列出共享里的目录（list a directory inside a share as JSON）",
			"GET", "/files/{share}/{path}").ro().q("json", "1").
			pArg("share", "path").
			str("share", "共享名，例如 sdcard（share name）").need("share").
			str("path", "共享内的相对路径，留空看根目录（relative path inside the share）").
			done(),
		newTool("phone_file_download",
			"从共享下载文件（download a file from a share）",
			"GET", "/files/{share}/{path}").ro().as("download").
			pArg("share", "path").
			str("share", "共享名，例如 sdcard（share name）").need("share").
			str("path", "共享内的相对路径（relative file path）").need("path").
			bl("base64", "强制以 base64 文本返回（force a base64 text answer）").
			int("max_bytes", "允许返回的最大字节数，超出则不返回内容（cap the payload）").
			done(),
		newTool("phone_file_upload",
			"上传或覆盖共享里的文件（upload or replace a file in a share）",
			"PUT", "/files/{share}/{path}").as("upload").
			pArg("share", "path").
			str("share", "共享名（share name）").need("share").
			str("path", "共享内的相对路径（relative file path）").need("path").
			str("text", "文件内容（UTF-8 文本，与 data_base64 二选一）").
			str("data_base64", "文件内容（base64 编码的字节，与 text 二选一）").
			bl("mkdir", "自动创建缺失的父目录（create missing parent directories）").
			note("整段内容作为一个请求体发送；大文件请用 WebDAV（phone_webdav_info 给出地址）。files.read_only=true 时服务端会拒绝上传。").
			done(),
		newTool("phone_rename_or_move",
			"重命名、移动或复制共享内的文件（rename/move/copy inside a share）",
			"POST", "/files/{share}/{path}").qarg("op", "dest").
			pArg("share", "path").
			str("share", "共享名（share name）").need("share").
			str("path", "源相对路径（source path）").need("path").
			str("dest", "目标，写成 <share>/<path>（destination as share/path）").need("dest").
			enum("op", "操作类型，默认 move（what to do）", "move", "copy").
			done(),
		newTool("phone_webdav_info",
			"给出可挂载的 WebDAV 地址与认证说明（mountable WebDAV URL and Basic auth note）",
			"GET", "/api/v1/health").ro().
			note("把 <base>/dav/<share>/ 挂成网络驱动器（Windows 映射网络驱动器 / macOS Finder 连接服务器 / rclone、ES 文件浏览器等）。认证是 HTTP Basic：用户名随便填，密码用 config.json 里的 api_token；无 token 时服务不校验认证。WebDAV 提供 DAV: 1,2、PROPFIND/MKCOL/PUT/MOVE/COPY/DELETE；LOCK/UNLOCK 会被回应但不真正生效。下面的 file_shares 是可用共享名。").
			done(),

		// ----- bridge, config, history -----
		newTool("phone_bridge_status",
			"检查 Termux:API 桥是否可用以及被哪条策略挡住（is the bridge usable, and which policy blocks camera/mic）",
			"GET", "/api/v1/bridge/status").ro().done(),
		newTool("phone_bridge_fix",
			"修桥：授予权限、取唤醒锁、把 uid 提到前台（grant permissions, take the wake lock, foreground the uid）",
			"POST", "/api/v1/bridge/fix").
			note("相机/麦克风报错时先调这个，再重试原来的工具。").
			done(),
		newTool("phone_config_get",
			"读取当前配置，token 已打码（current config, token redacted）",
			"GET", "/api/v1/config").ro().done(),
		newTool("phone_config_set",
			"更新配置字段，只需给出要改的段落（update config fields; a partial document is merged）",
			"PUT", "/api/v1/config").
			str("listen", "监听地址 host:port，改动需要重启（listen address; restart required）").
			str("api_token", "API 令牌（api token）").
			str("device_id", "设备标识（device id）").
			str("log_dir", "日志目录，改动需要重启（log directory）").
			bl("log_requests", "是否记录每个请求（log every request）").
			int("getprop_ttl_seconds", "getprop 缓存秒数（getprop TTL）").
			int("wifi_ttl_seconds", "Wi-Fi 状态缓存秒数（wifi TTL）").
			int("power_ttl_seconds", "电源状态缓存秒数（power TTL）").
			int("packages_ttl_seconds", "包列表缓存秒数（packages TTL）").
			int("procs_ttl_seconds", "进程列表缓存秒数（processes TTL）").
			arrStr("storage_mounts", "要统计的挂载点（mounts to report）").
			obj("security", "安全开关：allow_non_loopback_listen / allow_shell / allow_power_commands / shell_timeout_ms / shell_max_output_bytes（security switches）").
			obj("control", "控制开关：enabled / default_timeout_ms / max_timeout_ms（control switches）").
			obj("capture", "相机与麦克风参数（camera and microphone settings）").
			obj("stream", "屏幕串流参数（screen streaming settings）").
			obj("notify", "通知读取参数（notification settings）").
			obj("mcp", "MCP 开关：http / read_only / Path（MCP transport switches）").
			obj("bridge", "Termux:API 桥参数（bridge settings）").
			obj("files", "文件共享：enabled / shares / read_only / allow_delete / max_upload_mb / http_files / webdav（file service settings）").
			obj("history", "历史库参数（history database settings）").
			note("只发送要修改的字段即可（PUT 会把给出的字段合并进现有配置）。mcp.read_only=true 时本工具会被拒绝。非 loopback 的 listen 还要求 security.allow_non_loopback_listen=true。").
			done(),
		newTool("phone_history_query",
			"查询存储的历史采样（query stored samples: metrics/since/until/limit）",
			"GET", "/api/v1/history").ro().
			arrStr("metrics", "要查询的指标列，例如 [\"battery_capacity_pct\",\"cpu_usage_pct\"]（metric columns）").
			str("since", "起始时间，接受 -6h / -7d / unix 秒 / RFC3339（start time）").
			str("until", "结束时间（end time）").
			int("limit", "最多返回点数，默认 720（max points）").
			bl("stats", "附带每列的统计（include per-column stats）").
			done(),
		newTool("phone_history_metrics",
			"列出可查询的指标列（the queryable metric columns）",
			"GET", "/api/v1/history/metrics").ro().done(),
		newTool("phone_history_logs",
			"查询存储的日志行（query stored log lines: grep/level/since）",
			"GET", "/api/v1/history/logs").ro().
			str("grep", "子串过滤（substring filter）").
			str("level", "日志级别（log level）").
			str("since", "起始时间（start time）").
			str("until", "结束时间（end time）").
			int("limit", "最多返回行数，默认 200（max lines）").
			done(),
		newTool("phone_history_snapshot",
			"读取某一时刻的完整快照（the full stored document at or near a timestamp）",
			"GET", "/api/v1/history/snapshot").ro().
			str("ts", "时间戳，默认最新（timestamp; default latest）").
			str("section", "只取某一节，例如 battery（only one section）").
			done(),
		newTool("phone_history_stats",
			"读取历史库大小、行数与增长速率（db size, row counts, growth rate）",
			"GET", "/api/v1/history/stats").ro().done(),
		newTool("phone_history_quality",
			"读取数据质量报告：覆盖率、缺口、被拒的值（data-quality report: coverage, gaps, rejected values）",
			"GET", "/api/v1/history/quality").ro().done(),
		newTool("phone_history_gaps",
			"列出采样缺口（sampling gaps outside the expected cadence）",
			"GET", "/api/v1/history/gaps").ro().done(),
		newTool("phone_history_prune",
			"立即执行历史保留策略（apply the history retention policy now）",
			"POST", "/api/v1/history/prune").done(),

		// ----- dangerous: gated by config -----
		newTool("phone_shell",
			"以 root 执行一条 shell 命令（run one root shell command）",
			"POST", "/api/v1/shell").
			str("cmd", "要执行的命令（command line）").need("cmd").
			int("timeout_ms", "超时毫秒（timeout in ms）").
			int("max_output_bytes", "输出上限字节（output cap）").
			note("需要 security.allow_shell=true，否则服务端返回 403 并说明原因。它以 root 身份运行，能改动这台手机的任何东西——只有用户明确要求时才用。").
			done(),
		newTool("phone_reboot",
			"重启手机（reboot the phone）",
			"POST", "/api/v1/device/reboot").
			enum("mode", "重启模式，留空为正常重启（reboot mode）",
				"normal", "recovery", "bootloader", "sideload", "edl").
			note("需要 security.allow_power_commands=true，否则服务端返回 403（错误文本里会说明该开关）。这是破坏性操作，只有用户明确要求时才用。").
			done(),
		newTool("phone_shutdown",
			"关闭手机电源（power the phone off）",
			"POST", "/api/v1/device/shutdown").
			note("需要 security.allow_power_commands=true，否则 403。执行后本服务随之停止。").
			done(),
	}

	return tools
}

// mcpPrettyJSON renders a tool's answer for a text content block. It keeps the
// server's own bytes when they are already JSON, and falls back to the raw text.
func mcpPrettyJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func mcpSchemaJSON(t *mcpTool) map[string]any { return t.schema }

// mcpStrVal formats an argument for a query string.
func mcpStrVal(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	case float64:
		return trimFloat(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, mcpStrVal(e))
		}
		return strings.Join(parts, ",")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}
