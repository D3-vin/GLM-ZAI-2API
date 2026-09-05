// dashboard.go — embedded HTML dashboard (GitHub-dark style, Viktor layout).
package app

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>GLM-ZAI-2API</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0d1117; color: #c9d1d9; min-height: 100vh; padding: 24px; }
.header { display: flex; align-items: center; gap: 12px; margin-bottom: 24px; }
.header h1 { font-size: 20px; font-weight: 600; color: #e6edf3; }
.header .sub { font-size: 13px; color: #8b949e; }
.dot { width: 10px; height: 10px; border-radius: 50%; background: #56d364; box-shadow: 0 0 8px #56d364; animation: pulse 2s infinite; }
@keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.5} }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 16px; }
.card { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 18px; }
.card-title { font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: .05em; color: #8b949e; margin-bottom: 14px; }
.row { display: flex; justify-content: space-between; align-items: center; padding: 6px 0; border-bottom: 1px solid #21262d; }
.row:last-child { border-bottom: none; }
.row-label { font-size: 13px; color: #8b949e; }
.row-value { font-size: 13px; color: #e6edf3; font-weight: 500; }
.badge { padding: 2px 8px; border-radius: 12px; font-size: 11px; font-weight: 600; cursor: pointer; transition: opacity .15s; user-select: none; }
.badge:hover { opacity: .75; }
.badge-green { background: #1a3a1a; color: #56d364; border: 1px solid #238636; }
.badge-red   { background: #3a1a1a; color: #f85149; border: 1px solid #da3633; }
.badge-gray  { background: #21262d; color: #8b949e; border: 1px solid #30363d; }
.badge-blue  { background: #1a2a3a; color: #79c0ff; border: 1px solid #1f6feb; }
.endpoint { font-family: monospace; font-size: 12px; color: #56d364; }
.code-block { background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 12px; font-family: monospace; font-size: 12px; color: #c9d1d9; white-space: pre-wrap; overflow-x: auto; }
.method { display: inline-block; padding: 2px 6px; border-radius: 4px; font-size: 10px; font-weight: 600; margin-right: 6px; }
.method-post { background: #1a2a3a; color: #79c0ff; border: 1px solid #1f6feb; }
.method-get  { background: #1a3a1a; color: #56d364; border: 1px solid #238636; }
.path { font-family: monospace; color: #e6edf3; font-size: 13px; }
.desc { font-size: 12px; color: #8b949e; margin-top: 2px; }
.loading { color: #484f58; font-style: italic; font-size: 13px; }
.ep-row { padding: 8px 0; border-bottom: 1px solid #21262d; }
.ep-row:last-child { border-bottom: none; }
</style>
</head>
<body>

<div class="header">
  <div class="dot" id="statusDot"></div>
  <div>
    <h1>GLM-ZAI-2API</h1>
    <div class="sub" id="modeSub">OpenAI-compatible proxy · loading…</div>
  </div>
</div>

<div class="grid">

  <!-- Status -->
  <div class="card">
    <div class="card-title">Status</div>
    <div class="row"><span class="row-label">Proxy</span><span class="badge badge-green" id="proxyBadge">running</span></div>
    <div class="row"><span class="row-label">Port</span><span class="row-value" id="portVal">—</span></div>
    <div class="row"><span class="row-label">Endpoint</span><span class="endpoint" id="endpointVal">—</span></div>
    <div class="row"><span class="row-label">Token DB</span><span class="row-value" id="tokenCountVal"><span class="loading">loading…</span></span></div>
    <div class="row"><span class="row-label">User</span><span class="row-value" id="userVal">—</span></div>
    <div class="row"><span class="row-label">FE Version</span><span class="row-value" id="feVerVal" style="font-size:12px">—</span></div>
    <div class="row"><span class="row-label">Sessions</span><span class="row-value" id="sessionsVal">0</span></div>
  </div>

  <!-- Features -->
  <div class="card">
    <div class="card-title">Features <span style="font-size:10px;font-weight:400;color:#484f58;text-transform:none">(per-model, click to toggle)</span></div>
    <div class="row"><span class="row-label">Model</span><select id="featModelSel" onchange="onFeatModelChange(this.value)" style="background:#0d1117;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:2px 8px;font-size:12px;max-width:230px"></select></div>
    <div class="row"><span class="row-label">Web Search</span><span class="badge badge-gray" id="featSearch" onclick="toggleFeat('web_search',this)">OFF</span></div>
    <div class="row"><span class="row-label">Thinking</span><span class="badge badge-gray" id="featThink" onclick="toggleFeat('enable_thinking',this)">OFF</span></div>
    <div class="row"><span class="row-label">Preview Mode</span><span class="badge badge-gray" id="featPreview" onclick="toggleFeat('preview_mode',this)">OFF</span></div>
    <div class="row"><span class="row-label">Persist History</span><span class="badge badge-gray" id="featPersist" onclick="toggleFeat('persist_history',this)">OFF</span></div>
  </div>

  <!-- Models catalog -->
  <div class="card">
    <div class="card-title">Models <span style="font-size:10px;font-weight:400;color:#484f58;text-transform:none">(live Z.AI catalog · cutoff glm-4.7)</span></div>
    <div class="row"><span class="row-label">Catalog size</span><span class="row-value"><span id="modelsCount">—</span> models</span></div>
    <div class="ep-row"><span class="path" id="modelsList" style="font-size:11px;line-height:1.7;word-break:break-all">—</span></div>
    <button class="badge badge-blue" id="modelsBtn" style="margin-top:10px;width:100%">Show capabilities JSON</button>
    <div class="code-block" id="modelsJSON" style="display:none;margin-top:10px;max-height:320px;overflow-y:auto"></div>
  </div>

  <!-- API Endpoints -->
  <div class="card" style="grid-column: 1 / -1;">
    <div class="card-title">API Endpoints</div>
    <div class="ep-row">
      <span class="method method-post">POST</span>
      <span class="path">/v1/chat/completions</span>
      <div class="desc">OpenAI-compatible chat (stream / non-stream, tool calling)</div>
    </div>
    <div class="ep-row">
      <span class="method method-get">GET</span>
      <span class="path">/v1/models</span>
      <div class="desc">Available model list</div>
    </div>
    <div class="ep-row">
      <span class="method method-post">POST</span>
      <span class="path">/features</span>
      <div class="desc">Per-model features: web_search, enable_thinking, preview_mode (image_generation always off)</div>
    </div>
    <div class="ep-row">
      <span class="method method-post">POST</span>
      <span class="path">/prompt</span>
      <div class="desc">Legacy: single text prompt</div>
    </div>
    <div class="ep-row">
      <span class="method method-post">POST</span>
      <span class="path">/admin/session/clear</span>
      <div class="desc">Clear all session history</div>
    </div>
    <div class="ep-row">
      <span class="method method-get">GET</span>
      <span class="path">/status</span>
      <div class="desc">Z.AI session status (public)</div>
    </div>
    <div class="ep-row">
      <span class="method method-get">GET</span>
      <span class="path">/admin/health</span>
      <div class="desc">Health check (public)</div>
    </div>
  </div>

  <!-- Test -->
  <div class="card" style="grid-column: 1 / -1;">
    <div class="card-title">Test the OpenAI Endpoint</div>
    <div class="code-block"># Non-streaming
curl -X POST http://__HOST__/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer __AUTH_TOKEN__" \
  -d '{"model":"glm-4.7","messages":[{"role":"user","content":"Hello!"}],"stream":false}'

# Streaming
curl -N -X POST http://__HOST__/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer __AUTH_TOKEN__" \
  -d '{"model":"glm-4.7","stream":true,"messages":[{"role":"user","content":"Say hi"}]}'</div>
  </div>

</div>

<script>
function setFeature(id, on) {
  var el = document.getElementById(id);
  if (!el) return;
  el.className = 'badge ' + (on ? 'badge-green' : 'badge-gray');
  el.textContent = on ? 'ON' : 'OFF';
}

async function refresh() {
  try {
    var res = await fetch('/status');
    var d = await res.json();

    // Header
    document.getElementById('modeSub').textContent = 'OpenAI-compatible proxy · ' + (d.mode || 'direct');
    var dot = document.getElementById('statusDot');
    var proxyBadge = document.getElementById('proxyBadge');
    if (d.connected) {
      dot.style.background = '#56d364';
      dot.style.boxShadow = '0 0 8px #56d364';
      proxyBadge.className = 'badge badge-green';
      proxyBadge.textContent = 'running';
    } else {
      dot.style.background = '#f85149';
      dot.style.boxShadow = '0 0 8px #f85149';
      proxyBadge.className = 'badge badge-red';
      proxyBadge.textContent = 'offline';
    }

    // Status
    document.getElementById('portVal').textContent = d.port || '—';
    document.getElementById('endpointVal').textContent = 'http://127.0.0.1:' + (d.port || '5082') + '/v1';
    document.getElementById('tokenCountVal').textContent = (d.tokenCount != null ? d.tokenCount + ' tokens' : '—');
    var userTxt = d.userName || '—';
    if (d.accountPoolSize > 1) {
      userTxt += ' · account ' + ((d.accountPoolIndex >= 0 ? d.accountPoolIndex : 0) + 1) + '/' + d.accountPoolSize;
    }
    document.getElementById('userVal').textContent = userTxt;
    document.getElementById('feVerVal').textContent = d.feVersion || '—';
    document.getElementById('sessionsVal').textContent = d.activeSessions || 0;

    // Persist history is bridge-global — sync from /status.
    // Per-model feature badges come from fetchFeatureState().
    setFeature('featPersist', (d.features || {}).persistHistory);
  } catch(e) { console.error('dashboard refresh error:', e); }
}

// Selected model for the Features card. Starts on the cutoff default and is
// re-pointed by the Models catalog as soon as it loads.
var featModel = 'glm-4.7';

function onFeatModelChange(m) {
  featModel = m;
  fetchFeatureState();
}

async function fetchFeatureState() {
  try {
    var res = await fetch('/features?model=' + encodeURIComponent(featModel), {
      headers: {'Authorization':'Bearer __AUTH_TOKEN__'}
    });
    if (!res.ok) return;
    var d = await res.json();
    var f = d.features || {};
    setFeature('featSearch', f.web_search || f.auto_web_search);
    setFeature('featThink', f.enable_thinking);
    setFeature('featPreview', f.preview_mode);
  } catch(e) { console.error('feature state error:', e); }
}

function toggleFeat(key, el) {
  var isOn = el.textContent === 'ON';
  var body = { model: featModel }; body[key] = !isOn;
  fetch('/features', {
    method: 'POST',
    headers: {'Content-Type':'application/json','Authorization':'Bearer __AUTH_TOKEN__'},
    body: JSON.stringify(body)
  }).then(function(r){return r.json()}).then(function(d){
    if(!d.success) return;
    // The response carries the authoritatively resolved per-model features
    var f = d.features || {};
    setFeature('featSearch', f.web_search || f.auto_web_search);
    setFeature('featThink', f.enable_thinking);
    setFeature('featPreview', f.preview_mode);
    setFeature('featPersist', d.persistHistory);
  }).catch(function(e){console.error('toggle error:',e)});
}

refresh();
setInterval(refresh, 5000);
fetchFeatureState();

// Models catalog — /admin/models (same auth as the API)
function refreshModels() {
  fetch('/admin/models', {
    headers: {'Authorization':'Bearer __AUTH_TOKEN__'}
  }).then(function(r){return r.json()}).then(function(d){
    if (!d || !d.models) return;
    document.getElementById('modelsCount').textContent = d.count;
    document.getElementById('modelsList').textContent = d.models.map(function(m){return m.id}).join(' · ');
    var pre = document.getElementById('modelsJSON');
    pre.textContent = JSON.stringify(d, null, 2);
    // Feed the Features card selector from the same catalog
    var sel = document.getElementById('featModelSel');
    if (sel && d.models.length) {
      var cur = sel.value || featModel;
      sel.innerHTML = d.models.map(function(m){ return '<option value="'+m.id+'">'+m.id+'</option>'; }).join('');
      if ([].some.call(sel.options, function(o){ return o.value === cur; })) sel.value = cur;
      featModel = sel.value;
      fetchFeatureState();
    }
  }).catch(function(e){console.error('models refresh error:',e)});
}
document.getElementById('modelsBtn').addEventListener('click', function(){
  var pre = document.getElementById('modelsJSON');
  pre.style.display = pre.style.display === 'none' ? 'block' : 'none';
});
refreshModels();
setInterval(refreshModels, 60000);
</script>
</body>
</html>`
