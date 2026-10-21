package main

// dashboardHTML is served at "/" — a single-file, no-build dashboard that talks
// to the same REST API any other client would use. The token is kept in
// localStorage and sent as X-API-Key.
const dashboardHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Phone Data Server</title>
<style>
:root{--bg:#0b0e13;--card:#141922;--line:#232b38;--fg:#e6edf3;--dim:#8b98a9;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--acc:#58a6ff}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{padding:14px 18px;border-bottom:1px solid var(--line);display:flex;gap:14px;align-items:center;flex-wrap:wrap}
h1{font-size:15px;margin:0;font-weight:600}
.dim{color:var(--dim)}
main{display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:12px;padding:14px 18px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.card h2{margin:0 0 8px;font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:var(--dim);font-weight:600}
.row{display:flex;justify-content:space-between;gap:10px;padding:2px 0;border-bottom:1px dotted #1c2331}
.row:last-child{border:0}
.row span:last-child{text-align:right}
.big{font-size:22px;font-weight:600}
.bar{height:6px;background:#1c2331;border-radius:3px;overflow:hidden;margin-top:6px}
.bar>i{display:block;height:100%;background:var(--acc)}
.bars{display:grid;grid-template-columns:repeat(4,1fr);gap:4px;margin-top:8px}
.mini{background:#1c2331;border-radius:3px;height:34px;position:relative;overflow:hidden}
.mini>i{position:absolute;bottom:0;left:0;right:0;background:linear-gradient(180deg,#58a6ff,#1f6feb);transition:height .4s}
.mini>b{position:absolute;top:1px;left:0;right:0;text-align:center;font-size:9px;font-weight:400;color:var(--dim)}
input,button{background:#0f141c;color:var(--fg);border:1px solid var(--line);border-radius:6px;padding:6px 9px;font:inherit}
button{cursor:pointer}
button:hover{border-color:var(--acc)}
#err{color:var(--bad);white-space:pre-wrap}
.tag{padding:1px 6px;border-radius:99px;font-size:11px;border:1px solid var(--line)}
.ok{color:var(--ok)}.warn{color:var(--warn)}.bad{color:var(--bad)}
</style>
</head>
<body>
<header>
  <h1>Phone Data Server</h1>
  <span id="dev" class="dim">connecting…</span>
  <span class="dim" id="upd"></span>
  <span style="margin-left:auto"></span>
  <input id="tok" type="password" placeholder="api token" size="28">
  <button onclick="saveTok()">save</button>
  <button onclick="pushNow()">push now</button>
</header>
<main id="grid"></main>
<div style="padding:0 18px 14px">
  <div class="card">
    <h2>历史数据（本机 SQLite）</h2>
    <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:9px">
      <select id="hm"></select>
      <select id="hr">
        <option value="-1h">最近 1 小时</option>
        <option value="-6h" selected>最近 6 小时</option>
        <option value="-24h">最近 24 小时</option>
        <option value="-7d">最近 7 天</option>
        <option value="-30d">最近 30 天</option>
      </select>
      <button onclick="loadHistory()">刷新</button>
    </div>
    <canvas id="hc" style="width:100%;height:160px;display:block;background:#0f141c;border:1px solid var(--line);border-radius:8px"></canvas>
    <div id="hsum" class="dim" style="margin-top:7px"></div>
    <div id="hstat"></div>
  </div>
  <div class="card">
    <h2>截图</h2>
    <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:9px">
      <select id="sf"><option value="webp" selected>WebP</option><option value="jpeg">JPEG</option><option value="png">PNG</option></select>
      <select id="sq">
        <option value="60">质量 60 · 最小</option>
        <option value="80" selected>质量 80 · 推荐</option>
        <option value="90">质量 90 · 清晰</option>
      </select>
      <select id="ss"><option value="1" selected>原始尺寸</option><option value="0.5">缩小一半</option><option value="0.25">四分之一</option></select>
      <button onclick="shot()">截图</button>
      <button onclick="shotDownload()" id="sd" disabled>下载</button>
    </div>
    <div id="smeta" class="dim">按「截图」获取当前屏幕画面。只走 GET 接口，不写入历史数据库，也不参与云端推送。</div>
    <img id="simg" alt="屏幕截图" style="display:none;max-width:100%;margin-top:9px;border:1px solid var(--line);border-radius:8px">
  </div>
</div>
<footer style="padding:0 18px 24px"><div id="err"></div></footer>
<script>
const $=s=>document.querySelector(s);
function saveTok(){localStorage.setItem('ps_tok',$('#tok').value.trim());load();}
function tok(){return ($('#tok').value||localStorage.getItem('ps_tok')||'').trim();}
function hdr(){const t=tok();return t?{'X-API-Key':t}:{};}
async function api(p,opt){const r=await fetch(p,Object.assign({headers:hdr()},opt||{}));
  if(!r.ok)throw new Error(p+' -> '+r.status+' '+await r.text());return r.json();}
function fmtB(n){const u=['B','KB','MB','GB','TB'];let i=0;n=+n||0;while(n>=1024&&i<4){n/=1024;i++}return n.toFixed(n<10?2:1)+u[i];}
function fmtD(sec){if(sec<0||sec===undefined)return '-';const d=Math.floor(sec/86400),h=Math.floor(sec%86400/3600),m=Math.floor(sec%3600/60);return (d?d+'d ':'')+h+'h '+m+'m';}
function row(k,v,cls){return '<div class="row"><span class="dim">'+k+'</span><span class="'+(cls||'')+'">'+v+'</span></div>';}
function card(title,body){return '<div class="card"><h2>'+title+'</h2>'+body+'</div>';}
/* Kernel strings arrive in English; translate at render time so /api/v1 keeps
   a faithful copy of what the kernel actually reported. */
var ZH_STATUS={'Charging':'充电中','Discharging':'放电中','Full':'已充满','Not charging':'未充电','Unknown':'未知'};
var ZH_HEALTH={'Good':'良好','Overheat':'过热','Dead':'已损坏','Over voltage':'电压过高','Over current':'电流过大','Cold':'温度过低','Unspecified failure':'未指定故障','Unknown':'未知'};
var ZH_SELINUX={'Enforcing':'强制模式','Permissive':'宽容模式','disabled':'已关闭','unknown':'未知'};
var ZH_WAKE={'Awake':'亮屏','Dozing':'息屏显示','Asleep':'休眠','Unknown':'未知'};
function zh(map,v){return map[v]||v||'–';}

function render(s){
  const b=s.battery,c=s.cpu,m=s.memory,n=s.network,th=s.thermal,w=s.wifi,g=s.gpu,st=s.storage,sy=s.system,p=s.processes,pk=s.packages,d=s.device;
  const heat=(t)=>t>=70?'bad':t>=50?'warn':'ok';
  let h='';
  h+=card('设备', row('机型',d.market_name||d.model)+row('device_id',d.device_id)+row('系统版本',d.rom_incremental)+row('安卓版本',d.android_release+' / SDK '+d.android_sdk)+row('内核',(d.kernel||'').split(' ')[2]||'')+row('分区槽位',d.slot_suffix)+row('引导加载器',d.bootloader_unlocked?'已解锁':'已锁定',d.bootloader_unlocked?'warn':'ok'));
  h+=card('电量',
    '<div class="big '+heat(b.temp_c)+'">'+b.capacity_percent+'%</div>'+'<div class="dim">'+zh(ZH_STATUS,b.status)+' · '+zh(ZH_HEALTH,b.health)+'</div>'+
    '<div class="bar"><i style="width:'+b.capacity_percent+'%"></i></div>'+
    row('温度',b.temp_c+' °C',heat(b.temp_c))+row('循环次数',b.cycle_count)+row('健康度',b.health_percent+' %')+
    row('满充容量',b.charge_full_mah+' mAh')+row('设计容量',b.charge_full_design_mah+' mAh')+
    row('电流',b.current_now_a+' A')+row('电压',b.voltage_v+' V')+
    row('充到满',fmtD(b.time_to_full_seconds))+row('用到空',fmtD(b.time_to_empty_seconds)));
  h+=card('处理器',
    '<div class="big">'+c.usage_percent+'%</div>'+'<div class="dim">'+c.core_count+' 核 · '+((c.model||'').replace('Qualcomm Technologies, Inc ',''))+'</div>'+
    '<div class="bars">'+(c.per_core_percent||[]).map((v,i)=>'<div class="mini"><i style="height:'+Math.max(2,Math.min(100,v))+'%"></i><b>'+i+'</b></div>').join('')+'</div>'+
    row('负载',(c.load_avg||[]).join(' '))+row('开机时长',fmtD(c.uptime_seconds))+
    row('频率 MHz',(c.cores||[]).map(x=>Math.round(x.cur_khz/1000)).join('/')+' MHz')+
    row('调度器',(c.cores&&c.cores[0]&&c.cores[0].governor)||''));
  h+=card('内存', row('总量',fmtB(m.total_kb*1024))+row('可用',fmtB(m.available_kb*1024))+row('空闲',fmtB(m.free_kb*1024))+row('缓存',fmtB(m.cached_kb*1024))+row('交换分区',fmtB((m.swap_total_kb-m.swap_free_kb)*1024)+' / '+fmtB(m.swap_total_kb*1024))+'<div class="bar"><i style="width:'+m.used_percent+'%"></i></div>');
  h+=card('温度', '<div class="big '+heat(th.max_temp_c)+'">'+th.max_temp_c+' °C</div><div class="dim">'+th.hottest_zone+'</div>'+
    (th.zones||[]).slice(0,8).map(z=>row(z.type||z.name,z.temp_c+' °C',heat(z.temp_c))).join('')+
    row('温度区',(th.temperature_zone_count!=null?th.temperature_zone_count:th.zone_count)));
  h+=card('网络',
    row('实时下行',fmtB(n.total_rx_bytes_per_sec)+'/s')+row('实时上行',fmtB(n.total_tx_bytes_per_sec)+'/s')+
    row('累计下行',fmtB(n.total_rx_bytes))+row('累计上行',fmtB(n.total_tx_bytes))+
    (n.interfaces||[]).filter(i=>i.name!=='lo'&&i.rx_bytes>0).map(i=>row(i.name,(i.ipv4||[]).join(',')||i.mac,'')).join(''));
  h+=card('Wi-Fi',
    '<div class="big">'+(w.signal_percent||0)+'%</div><div class="dim">'+(w.ssid||(w.error?'读取失败':'未连接'))+'</div>'+
    row('bssid',w.bssid||'-')+row('ip',w.ipv4||'-')+row('rssi',(w.rssi_dbm||0)+' dBm',w.rssi_dbm>-60?'ok':w.rssi_dbm>-70?'warn':'bad')+
    row('协商速率',w.link_speed_mbps+' Mbps')+row('频率 MHz',w.frequency_mhz+' MHz · '+w.band+' ch'+(w.channel||'-'))+
    row('协议',w.standard||'-')+row('收 / 发包',w.tx_packets+' / '+w.rx_packets)+
    row('发送重试',w.tx_retries||0)+row('发送丢弃',w.tx_lost||0)+(w.error?row('错误',w.error,'bad'):''));
  h+=card('图形', '<div class="big">'+g.busy_percent+'%</div>'+row('GPU 频率',Math.round(g.current_hz/1e6)+' MHz')+row('最高频率',Math.round(g.max_hz/1e6)+' MHz')+row('温度',g.temp_c+' °C',heat(g.temp_c)));
  h+=card('存储',(st||[]).map(x=>row(x.mount,fmtB(x.free_bytes)+' free / '+fmtB(x.total_bytes)+' ('+x.used_percent+'% used)')).join(''));
  h+=card('系统', row('唤醒状态',zh(ZH_WAKE,s.power.wakefulness))+row('屏幕',s.display.screen_on?'亮':'灭')+
    row('亮度',s.display.brightness+' / '+s.display.brightness_max)+row('SELinux',zh(ZH_SELINUX,sy.selinux_mode))+
    row('进程数',p.count)+row('安装包',pk.total+' ('+pk.system+' sys / '+pk.user+' user)')+
    row('时区',sy.timezone)+row('开机时刻',sy.boot_time||''));
  h+=card('占用最高的进程',(p.top_by_rss||[]).slice(0,10).map(x=>row(x.name,fmtB(x.rss_kb*1024))).join(''));
  $('#grid').innerHTML=h;
  $('#dev').textContent=(d.market_name||d.model)+' · '+d.device_id;
  $('#upd').textContent='updated '+new Date().toLocaleTimeString();
}
async function load(){try{$('#err').textContent='';const s=await api('/api/v1/all');render(s);}catch(e){$('#err').textContent=String(e.message||e);}}
let shotURL=null,shotName='screenshot.webp';
async function shot(){
  const q=$('#sq').value,s=$('#ss').value,f=$('#sf').value;
  $('#smeta').textContent='正在截图并编码…'; $('#sd').disabled=true;
  try{
    const r=await fetch('/api/v1/screenshot?format='+f+'&quality='+q+'&scale='+s,{headers:hdr()});
    if(!r.ok){$('#smeta').textContent='截图失败：HTTP '+r.status+' '+await r.text();return;}
    const b=await r.blob();
    if(shotURL)URL.revokeObjectURL(shotURL);
    shotURL=URL.createObjectURL(b); shotName='screenshot.'+f;
    $('#simg').src=shotURL; $('#simg').style.display='block';
    $('#sd').disabled=false;
    const kb=n=>n<1024?n+' B':(n<1048576?(n/1024).toFixed(1)+' KB':(n/1048576).toFixed(2)+' MB');
    $('#smeta').textContent=r.headers.get('X-Screenshot-Size')+' · '+kb(b.size)+
      '（原始 PNG '+kb(Number(r.headers.get('X-Screenshot-Source-Bytes'))||0)+'）· 采集 '+
      (r.headers.get('X-Screenshot-Capture-Ms')||'?')+' ms · 编码 '+
      (r.headers.get('X-Screenshot-Encode-Ms')||'?')+' ms · 未入库 / 未推送';
  }catch(e){$('#smeta').textContent='截图失败：'+String(e.message||e);}
}
function shotDownload(){if(!shotURL)return;const a=document.createElement('a');a.href=shotURL;a.download=shotName;a.click();}
async function pushNow(){try{const r=await api('/api/v1/push',{method:'POST'});$('#err').textContent='push: '+(r.ok?('ok #'+r.http_status):(r.last_error||'failed'));}catch(e){$('#err').textContent=String(e.message||e);}}
$('#tok').value=localStorage.getItem('ps_tok')||'';

/* ---------------- history panel (SQLite-backed) ---------------- */
var HM=['battery_temp_c','battery_capacity_pct','battery_current_a','cpu_usage_pct','gpu_busy_pct','gpu_temp_c','mem_used_pct','max_temp_c','net_rx_bps','net_tx_bps','wifi_rssi_dbm','data_free_bytes','uptime_s','collect_ms'];
var HL={'battery_temp_c':'电池温度 °C','battery_capacity_pct':'电量 %','battery_current_a':'电池电流 A','cpu_usage_pct':'CPU 占用 %','gpu_busy_pct':'GPU 占用 %','gpu_temp_c':'GPU 温度 °C','mem_used_pct':'内存占用 %','max_temp_c':'最高温度 °C','net_rx_bps':'下行速率 B/s','net_tx_bps':'上行速率 B/s','wifi_rssi_dbm':'Wi-Fi 信号 dBm','data_free_bytes':'data 可用空间','uptime_s':'开机时长 s','collect_ms':'单次采集耗时 ms'};
function fillHM(list){var s=$('#hm'),cur=s.value,h='';list.forEach(function(m){h+='<option value="'+m+'">'+(HL[m]||m)+'</option>';});if(h)s.innerHTML=h;if(cur)s.value=cur;}
fillHM(HM);
function fv(v){if(v===null||v===undefined||v==='')return '–';var n=+v;if(!isFinite(n))return String(v);if(Math.abs(n)>=100000)return fmtB(n);return (Math.abs(n)>=100?Math.round(n):Math.round(n*100)/100).toString();}
function drawHChart(pts,idx){
  var c=$('#hc'),dpr=window.devicePixelRatio||1,w=c.clientWidth||600,h=160;
  c.width=Math.max(1,Math.round(w*dpr));c.height=Math.round(h*dpr);
  var g=c.getContext('2d');if(!g)return;g.setTransform(dpr,0,0,dpr,0,0);g.clearRect(0,0,w,h);
  var vals=[],t0=null,t1=null;
  for(var i=0;i<pts.length;i++){var ts=+pts[i][0],v=+pts[i][idx];if(!isFinite(ts)||!isFinite(v))continue;if(t0===null)t0=ts;t1=ts;vals.push([ts,v]);}
  g.font='11px monospace';
  if(vals.length<2){g.fillStyle='#8b98a9';g.fillText(vals.length?'只有一个采样点，再等一个周期':'这个时间窗内没有数据',10,22);return;}
  var vmin=Infinity,vmax=-Infinity;
  for(i=0;i<vals.length;i++){if(vals[i][1]<vmin)vmin=vals[i][1];if(vals[i][1]>vmax)vmax=vals[i][1];}
  if(vmax-vmin<1e-9)vmax=vmin+1;
  var pad=26,span=Math.max(1,t1-t0);
  var X=function(ts){return pad+(ts-t0)/span*(w-pad*2);};
  var Y=function(v){return h-pad-(v-vmin)/(vmax-vmin)*(h-pad*2);};
  g.strokeStyle='rgba(139,152,169,.22)';g.lineWidth=1;
  for(var k=0;k<=3;k++){var gy=pad+k*(h-pad*2)/3;g.beginPath();g.moveTo(pad,gy);g.lineTo(w-pad,gy);g.stroke();}
  g.beginPath();g.moveTo(X(vals[0][0]),Y(vals[0][1]));
  for(i=1;i<vals.length;i++)g.lineTo(X(vals[i][0]),Y(vals[i][1]));
  g.strokeStyle='#58a6ff';g.lineWidth=1.7;g.stroke();
  g.lineTo(X(vals[vals.length-1][0]),h-pad);g.lineTo(X(vals[0][0]),h-pad);g.closePath();
  g.fillStyle='rgba(88,166,255,.15)';g.fill();
  g.fillStyle='#8b98a9';
  g.fillText(fv(vmax),2,pad+4);g.fillText(fv(vmin),2,h-pad+4);
  g.fillText(new Date(vals[0][0]*1000).toLocaleTimeString(),pad,h-5);
  g.fillText(new Date(vals[vals.length-1][0]*1000).toLocaleTimeString(),w-pad-58,h-5);
  g.beginPath();g.arc(X(vals[vals.length-1][0]),Y(vals[vals.length-1][1]),2.6,0,Math.PI*2);g.fillStyle='#58a6ff';g.fill();
}
async function loadHistory(){
  var m=$('#hm').value||'battery_temp_c',r=$('#hr').value||'-6h';
  try{
    $('#err').textContent='';
    var j=await api('/api/v1/history?metrics='+encodeURIComponent(m)+'&since='+encodeURIComponent(r)+'&limit=320&stats=1');
    var idx=(j.columns||[]).indexOf(m);
    drawHChart(j.points||[],idx<0?0:idx);
    var s=j.stats||{};
    $('#hsum').textContent=(j.count||0)+' 个点 · '+(HL[m]||m)+' · 最小 '+fv((s.min||{})[m])+' · 平均 '+fv((s.avg||{})[m])+' · 最大 '+fv((s.max||{})[m])+' · 最新 '+fv((s.last||{})[m]);
  }catch(e){$('#hsum').textContent=String(e.message||e);
    if(String(e.message||e).indexOf('503')>=0)$('#hsum').textContent='本地历史未启用（history.enabled=false）';return;}
  try{
    var t=await api('/api/v1/history/stats'),st=t.store||{},co=t.cost_estimate||{};
    var srcs=st.logs_by_source?Object.keys(st.logs_by_source).map(function(k){return k+'×'+st.logs_by_source[k];}).join('  '):'–';
    $('#hstat').innerHTML=
      row('采样间隔',(t.interval_seconds||'?')+' 秒（完整文档 '+(t.full_seconds||'?')+' 秒）')+
      row('样本 / 快照 / 日志',(st.samples||0)+' / '+(st.snapshots||0)+' / '+(st.logs||0))+
      row('数据库',fmtB(st.db_bytes)+'  <span class="dim">'+(st.path||'')+'</span>')+
      row('保留策略','样本 '+(st.retention_days||0)+' 天 · 快照 '+(st.snapshot_retention_days||0)+' 天 · 日志 '+(st.log_retention_days||0)+' 天 · 上限 '+(st.max_db_mb||0)+' MB')+
      row('快照占用（精确）',st.snapshot_bytes_per_day?fmtB(st.snapshot_bytes_per_day)+' / 天 × '+(st.snapshot_retention_days||0)+' 天保留':'–')+
      row('实测增长',st.bytes_per_day?fmtB(st.bytes_per_day)+' / 天（主库增量）':(st.growth_note||'–'))+
      row('预计填满上限',st.est_days_to_cap?Math.round(st.est_days_to_cap)+' 天':'–')+
      row('采集开销',(co.collections_per_day||0)+' 次/天 · 约 '+(co.cpu_seconds_per_day||0)+' 秒 CPU · '+(co.avg_cpu_percent_of_core||0)+'% 单核')+
      row('循环耗时',(t.timing&&t.timing.ticks_measured)?('间隔 '+(t.timing.avg_gap_ms||0)+'ms · 采集 '+(t.timing.avg_collect_ms||0)+'ms · 写库 '+(t.timing.avg_write_ms||0)+'ms（均值）'):'（还没跑满一个周期）')+
      row('快照压缩比',st.compression_ratio?st.compression_ratio+'×':'–')+
      row('日志来源',srcs)+row('丢弃的日志行',st.dropped_log_lines||0)+
      row('上次写入错误',t.last_error||'无');
  }catch(e){$('#hstat').innerHTML='';}
}
api('/api/v1/history/metrics').then(function(j){
  if(j&&j.metrics)fillHM(HM.filter(function(m){return j.metrics.indexOf(m)>=0;}).concat(j.metrics.filter(function(m){return HM.indexOf(m)<0;})));
}).catch(function(){});
$('#hm').onchange=loadHistory;$('#hr').onchange=loadHistory;

load();setInterval(load,3000);
loadHistory();setInterval(function(){if(!document.hidden)loadHistory();},30000);
</script>
</body>
</html>
`
