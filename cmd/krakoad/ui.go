package main

// The audit UI: one self-contained page (inline CSS/JS, zero external
// assets) polling the existing JSON endpoints. Presentation only — the
// event spine is the data. Served on the loopback ingress; never published.

const uiHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>krakoa</title>
<style>
  :root { --bg:#111418; --panel:#1a1f26; --line:#2a323d; --fg:#d7dde5; --dim:#7d8794;
          --ok:#4cc38a; --warn:#e5b567; --bad:#e5484d; --acc:#6ca6e0; }
  * { box-sizing:border-box; margin:0; }
  body { background:var(--bg); color:var(--fg); font:13px/1.5 ui-monospace,Menlo,monospace; }
  header { padding:10px 16px; border-bottom:1px solid var(--line); display:flex; gap:12px; align-items:baseline; }
  header h1 { font-size:14px; letter-spacing:2px; }
  header span { color:var(--dim); font-size:11px; }
  main { display:grid; grid-template-columns:340px 1fr; min-height:calc(100vh - 41px); }
  #runs { border-right:1px solid var(--line); overflow-y:auto; }
  .run { padding:9px 14px; border-bottom:1px solid var(--line); cursor:pointer; }
  .run:hover, .run.sel { background:var(--panel); }
  .run .id { color:var(--acc); }
  .badge { padding:0 6px; border-radius:3px; font-size:11px; }
  .s-running { color:var(--acc); } .s-waiting { color:var(--warn); }
  .s-gated { color:var(--warn); } .s-done { color:var(--ok); }
  .s-failed, .s-needs-attention { color:var(--bad); font-weight:bold; }
  .s-queued, .s-canceled { color:var(--dim); }
  #detail { padding:14px 18px; overflow-y:auto; }
  h2 { font-size:13px; color:var(--dim); text-transform:uppercase; letter-spacing:1px; margin:16px 0 6px; }
  .gate { border:1px solid var(--warn); border-radius:4px; padding:8px 12px; margin:6px 0; }
  .gate.att { border-color:var(--bad); }
  .gate code { color:var(--acc); user-select:all; }
  .step { border:1px solid var(--line); border-radius:4px; padding:8px 12px; margin:6px 0; background:var(--panel); }
  .step .meta { color:var(--dim); font-size:12px; }
  .step .err { color:var(--bad); white-space:pre-wrap; }
  table { border-collapse:collapse; width:100%; }
  td { padding:2px 10px 2px 0; vertical-align:top; white-space:nowrap; }
  td.data { white-space:normal; color:var(--dim); word-break:break-all; }
  td.t { color:var(--dim); }
  .empty { color:var(--dim); padding:20px; }
</style>
</head>
<body>
<header><h1>KRAKOA</h1><span id="stat">connecting…</span></header>
<main>
  <div id="runs"></div>
  <div id="detail"><div class="empty">select a run</div></div>
</main>
<script>
let sel = null, gates = [];

const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const j = async p => (await fetch(p)).json();
const when = t => t ? new Date(t).toLocaleTimeString() : '';

async function tick() {
  try {
    const [runs, g] = await Promise.all([j('/v1/runs'), j('/v1/gates')]);
    gates = g || [];
    document.getElementById('stat').textContent =
      (runs?.length ?? 0) + ' runs · ' + gates.length + ' open gate(s) · ' + new Date().toLocaleTimeString();
    renderRuns(runs || []);
    if (sel) renderDetail(await j('/v1/runs/' + sel));
  } catch (e) {
    document.getElementById('stat').textContent = 'daemon unreachable: ' + e;
  }
}

function renderRuns(runs) {
  const el = document.getElementById('runs');
  el.innerHTML = runs.map(r => ` + "`" + `
    <div class="run ${r.ID===sel?'sel':''}" onclick="sel='${esc(r.ID)}';tick()">
      <div><span class="id">${esc(r.ID)}</span></div>
      <div>${esc(r.Workspace)}/${esc(r.Workflow)} · ${esc(r.State)}
        <span class="badge s-${esc(r.Status)}">${esc(r.Status)}</span></div>
    </div>` + "`" + `).join('') || '<div class="empty">no runs</div>';
}

function renderDetail(d) {
  const r = d.run, steps = d.steps || [], events = d.events || [];
  const agentOf = {};
  events.forEach(e => { if (e.Kind === 'step-started' && e.Data) agentOf[e.Data.step] = e.Data.agent; });
  const myGates = gates.filter(g => g.RunID === r.ID);

  let html = ` + "`" + `<div><span class="id" style="color:var(--acc)">${esc(r.ID)}</span>
    · state <b>${esc(r.State)}</b> <span class="badge s-${esc(r.Status)}">${esc(r.Status)}</span><br>
    <span class="meta" style="color:var(--dim)">def ${esc(r.DefHash)} · ws-git ${esc(r.WSVersion)} · started ${when(r.CreatedAt)}</span></div>` + "`" + `;

  if (myGates.length) {
    html += '<h2>open gates</h2>' + myGates.map(g => ` + "`" + `
      <div class="gate ${g.Payload && g.Payload.startsWith('needs attention') ? 'att':''}">
        <b>${esc(g.Kind)}</b> @ ${esc(g.State)}: ${esc(g.Payload)}<br>
        ${g.Options?.length ? 'options: ' + esc(g.Options.join(' | ')) + '<br>' : ''}
        answer: <code>krakoactl answer ${esc(g.ID)} &lt;response&gt;</code>
      </div>` + "`" + `).join('');
  }

  html += '<h2>steps</h2>' + (steps.map(s => ` + "`" + `
    <div class="step">
      <b>${esc(s.State)}</b> · ${esc(agentOf[s.ID] || s.Kind)} · attempt ${s.Attempt}
        → <b class="${s.Error?'err':''}">${esc(s.Outcome || (s.Error?'failed':'…'))}</b>
      <div class="meta">
        ${s.SessionID ? 'session ' + esc(s.SessionID) : ''}
        ${s.CostUSD ? ' · $' + s.CostUSD.toFixed(4) : ''}
        ${s.StartedAt ? ' · ' + when(s.StartedAt) : ''}
      </div>
      ${s.SessionPath ? '<div class="meta">' + esc(s.SessionPath) + '</div>' : ''}
      ${s.Error ? '<div class="err">' + esc(s.Error) + '</div>' : ''}
    </div>` + "`" + `).join('') || '<div class="empty">none yet</div>');

  html += '<h2>timeline</h2><table>' + events.map(e => ` + "`" + `
    <tr><td class="t">${when(e.At)}</td><td>${esc(e.Kind)}</td><td>${esc(e.State)}</td>
    <td class="data">${esc(Object.entries(e.Data||{}).map(([k,v])=>k+'='+JSON.stringify(v)).join(' '))}</td></tr>` + "`" + `).join('') + '</table>';

  document.getElementById('detail').innerHTML = html;
}

tick();
setInterval(tick, 3000);
</script>
</body>
</html>`
