package main

// The audit UI: one self-contained page (inline CSS/JS, zero external
// assets) over the existing JSON endpoints. Langfuse-shaped: a runs table,
// and per run a state-visit waterfall with a detail panel per span. Raw
// events stay collapsed — the page answers "where is it, what does it need
// from me, what did it cost", not "here is the database".

const uiHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>krakoa</title>
<style>
  :root {
    --bg:#0e1116; --panel:#161b22; --panel2:#1c232c; --line:#232a33;
    --fg:#d5dce4; --dim:#8b96a3; --faint:#5a6572;
    --blue:#58a6ff; --amber:#d4a72c; --red:#f85149; --green:#3fb950; --purple:#bc8cff;
  }
  * { box-sizing:border-box; margin:0; }
  html,body { height:100%; }
  body { background:var(--bg); color:var(--fg);
         font:13px/1.45 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
  code, .mono { font-family:ui-monospace,Menlo,monospace; font-size:12px; }
  a { color:var(--blue); text-decoration:none; cursor:pointer; }

  header { display:flex; align-items:center; gap:14px; padding:10px 20px;
           border-bottom:1px solid var(--line); position:sticky; top:0; background:var(--bg); z-index:5; }
  header .brand { font-weight:700; letter-spacing:3px; font-size:13px; }
  header .crumb { color:var(--dim); }
  header .right { margin-left:auto; color:var(--faint); font-size:12px; }
  .gatepill { background:var(--amber); color:#111; border-radius:10px; padding:1px 9px;
              font-weight:600; font-size:12px; }

  main { max-width:1060px; margin:0 auto; padding:18px 20px 60px; }

  /* status chips */
  .chip { display:inline-block; padding:1px 8px; border-radius:10px; font-size:11px; font-weight:600; }
  .c-running { background:rgba(88,166,255,.15); color:var(--blue); }
  .c-waiting { background:rgba(212,167,44,.15); color:var(--amber); }
  .c-gated   { background:rgba(212,167,44,.25); color:var(--amber); }
  .c-queued  { background:rgba(139,150,163,.15); color:var(--dim); }
  .c-done    { background:rgba(63,185,80,.15); color:var(--green); }
  .c-failed, .c-needs-attention, .c-canceled { background:rgba(248,81,73,.15); color:var(--red); }

  /* runs table */
  table.runs { width:100%; border-collapse:collapse; }
  .runs th { text-align:left; color:var(--faint); font-size:11px; text-transform:uppercase;
             letter-spacing:.8px; font-weight:600; padding:6px 10px; border-bottom:1px solid var(--line); }
  .runs td { padding:9px 10px; border-bottom:1px solid var(--line); }
  .runs tr.r:hover { background:var(--panel); cursor:pointer; }
  .runs .num { text-align:right; font-family:ui-monospace,Menlo,monospace; font-size:12px; }
  .dim { color:var(--dim); } .faint { color:var(--faint); }

  /* gate banner */
  .gate { border:1px solid rgba(212,167,44,.5); background:rgba(212,167,44,.07);
          border-radius:6px; padding:10px 14px; margin:0 0 14px; }
  .gate.att { border-color:rgba(248,81,73,.6); background:rgba(248,81,73,.07); }
  .gate .q { margin-bottom:6px; }
  .gate code { background:var(--panel2); padding:2px 7px; border-radius:4px; user-select:all; }

  /* run header card */
  .runhead { display:flex; gap:26px; flex-wrap:wrap; align-items:baseline;
             padding:14px 16px; background:var(--panel); border:1px solid var(--line);
             border-radius:6px; margin-bottom:14px; }
  .runhead .kv .k { color:var(--faint); font-size:11px; text-transform:uppercase; letter-spacing:.8px; }
  .runhead .kv .v { font-size:15px; margin-top:2px; }

  /* waterfall */
  .wf { border:1px solid var(--line); border-radius:6px; overflow:hidden; }
  .row { display:grid; grid-template-columns:170px 1fr 130px; align-items:center;
         padding:7px 12px; border-bottom:1px solid var(--line); cursor:pointer; }
  .row:last-child { border-bottom:none; }
  .row:hover, .row.sel { background:var(--panel); }
  .row .name { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .row .sub { color:var(--faint); font-size:11px; }
  .track { position:relative; height:16px; margin:0 10px; }
  .bar { position:absolute; top:3px; height:10px; border-radius:3px; min-width:6px; opacity:.9; }
  .b-agent { background:var(--blue); } .b-wait { background:var(--amber); }
  .b-gate { background:var(--purple); } .b-park { background:var(--red); }
  .bar.live { animation:pulse 1.6s ease-in-out infinite; }
  @keyframes pulse { 0%,100% { opacity:.95; } 50% { opacity:.35; } }
  @media (prefers-reduced-motion: reduce) { .bar.live { animation:none; } }
  .row .right { text-align:right; font-family:ui-monospace,Menlo,monospace; font-size:11px; color:var(--dim); }

  /* span detail */
  .spandetail { margin-top:14px; border:1px solid var(--line); border-radius:6px;
                background:var(--panel); padding:14px 16px; }
  .spandetail h3 { font-size:13px; margin-bottom:8px; }
  .spandetail .meta { color:var(--dim); font-size:12px; margin-bottom:10px; }
  .spandetail .meta .mono { user-select:all; }
  pre { background:var(--bg); border:1px solid var(--line); border-radius:5px;
        padding:10px 12px; overflow-x:auto; font-size:12px; line-height:1.5;
        max-height:280px; overflow-y:auto; margin:6px 0 10px; }
  .lbl { color:var(--faint); font-size:11px; text-transform:uppercase; letter-spacing:.8px; }
  .err { color:var(--red); }

  details.raw { margin-top:16px; }
  details.raw summary { color:var(--faint); cursor:pointer; font-size:12px; }
  details.raw table { border-collapse:collapse; width:100%; margin-top:8px; }
  details.raw td { padding:2px 10px 2px 0; vertical-align:top; font-size:12px;
                   font-family:ui-monospace,Menlo,monospace; }
  details.raw td.d { color:var(--faint); word-break:break-all; white-space:normal; }
  .empty { color:var(--faint); padding:26px; text-align:center; }
</style>
</head>
<body>
<header>
  <span class="brand">KRAKOA</span>
  <span class="crumb" id="crumb"></span>
  <span id="gates-pill"></span>
  <span class="right" id="stat"></span>
</header>
<main id="main"></main>
<script>
'use strict';
var runId = null, selSpan = null, gates = [], lastDetail = null;

function esc(s) { return String(s == null ? '' : s).replace(/[&<>"]/g, function(c) {
  return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function j(p) { return fetch(p).then(function(r){ return r.json(); }); }
function dur(ms) {
  if (ms < 0 || isNaN(ms)) return '';
  var s = Math.round(ms/1000);
  if (s < 90) return s + 's';
  var m = Math.round(s/60); if (m < 90) return m + 'm';
  var h = Math.floor(m/60); return h + 'h ' + (m - h*60) + 'm';
}
function usd(x) { return x ? '$' + x.toFixed(x < 0.1 ? 4 : 2) : ''; }
function ago(t) { return dur(Date.now() - new Date(t)) }
function clock(t) { return t ? new Date(t).toLocaleTimeString() : ''; }

function tick() {
  Promise.all([j('/v1/runs'), j('/v1/gates')]).then(function(res) {
    var runs = res[0] || []; gates = res[1] || [];
    document.getElementById('stat').textContent = 'updated ' + new Date().toLocaleTimeString();
    document.getElementById('gates-pill').innerHTML =
      gates.length ? '<span class="gatepill">' + gates.length + ' gate' + (gates.length>1?'s':'') + ' waiting</span>' : '';
    if (runId) {
      j('/v1/runs/' + runId).then(function(d) { lastDetail = d; renderDetail(d); });
    } else {
      renderList(runs);
    }
  }).catch(function(e) {
    document.getElementById('stat').textContent = 'daemon unreachable';
  });
}

function openRun(id) { runId = id; selSpan = null; tick(); }
function home() { runId = null; selSpan = null; tick(); }

function renderList(runs) {
  document.getElementById('crumb').textContent = 'runs';
  var h = '';
  if (gates.length) {
    h += gates.map(function(g) { return gateBox(g, true); }).join('');
  }
  if (!runs.length) {
    h += '<div class="empty">no runs yet</div>';
  } else {
    h += '<table class="runs"><tr><th>run</th><th>status</th><th>now</th><th style="text-align:right">cost</th><th style="text-align:right">age</th></tr>' +
      runs.map(function(r) {
        return '<tr class="r" onclick="openRun(\'' + esc(r.ID) + '\')">' +
          '<td><span class="mono">' + esc(r.ID) + '</span><br><span class="faint">' + esc(r.Workspace) + ' / ' + esc(r.Workflow) + '</span></td>' +
          '<td><span class="chip c-' + esc(r.Status) + '">' + esc(r.Status) + '</span></td>' +
          '<td>' + esc(r.State) + '<br><span class="faint">for ' + ago(r.UpdatedAt) + '</span></td>' +
          '<td class="num" id="cost-' + esc(r.ID) + '"></td>' +
          '<td class="num dim">' + ago(r.CreatedAt) + '</td></tr>';
      }).join('') + '</table>';
  }
  document.getElementById('main').innerHTML = h;
}

function gateBox(g, showRun) {
  var att = (g.Payload || '').indexOf('needs attention') === 0;
  return '<div class="gate' + (att ? ' att' : '') + '">' +
    '<div class="q"><b>' + (att ? 'needs attention' : esc(g.Kind)) + '</b>' +
    (showRun ? ' · <a onclick="openRun(\'' + esc(g.RunID) + '\')" class="mono">' + esc(g.RunID) + '</a>' : '') +
    ' · ' + esc(g.Payload) +
    (g.Options && g.Options.length ? ' <span class="dim">[' + esc(g.Options.join(' / ')) + ']</span>' : '') + '</div>' +
    'answer: <code>krakoactl answer ' + esc(g.ID) + ' &lt;response&gt;</code></div>';
}

// Fold the event log into state visits: one waterfall row per stay in a state.
function segments(events) {
  var marks = {'run-admitted':'start','step-started':'agent','wait-armed':'wait','gate-opened':'gate','parked':'park'};
  var segs = [], cur = null;
  events.forEach(function(e) {
    var type = marks[e.Kind];
    if (!type || !e.State) return;
    var t = +new Date(e.At);
    if (cur && cur.state === e.State) {   // same visit: refine its type
      if (cur.type === 'start' || (type === 'park')) cur.type = (type === 'start') ? cur.type : type;
      if (cur.type === 'park' && type === 'gate') return;
      if (type === 'agent') cur.attempts++;
      return;
    }
    if (cur) cur.end = t;
    cur = { state: e.State, type: type === 'start' ? 'agent' : type, start: t, end: null, attempts: type === 'agent' ? 1 : 0 };
    segs.push(cur);
  });
  var last = events.length ? events[events.length-1] : null;
  var done = events.some(function(e) { return e.Kind === 'run-finished'; });
  if (cur && !cur.end) cur.end = done && last ? +new Date(last.At) : Date.now();
  if (cur) cur.live = !done;
  return segs;
}

function renderDetail(d) {
  var r = d.run, steps = d.steps || [], events = d.events || [];
  document.getElementById('crumb').innerHTML = '<a onclick="home()">runs</a> / <span class="mono">' + esc(r.ID) + '</span>';

  var agentOf = {};
  events.forEach(function(e) { if (e.Kind === 'step-started' && e.Data) agentOf[e.Data.step] = e.Data.agent; });
  var totalCost = steps.reduce(function(a, s) { return a + (s.CostUSD || 0); }, 0);
  var started = +new Date(r.CreatedAt);
  var done = events.some(function(e) { return e.Kind === 'run-finished'; });
  var endT = done ? +new Date(events[events.length-1].At) : Date.now();
  var myGates = gates.filter(function(g) { return g.RunID === r.ID; });

  var h = '<div class="runhead">' +
    kv('status', '<span class="chip c-' + esc(r.Status) + '">' + esc(r.Status) + '</span>') +
    kv('now', esc(r.State) + ' <span class="dim">for ' + ago(r.UpdatedAt) + '</span>') +
    kv('duration', dur(endT - started)) +
    kv('cost', usd(totalCost) || '—') +
    kv('steps', String(steps.length)) +
    '</div>';

  h += myGates.map(function(g) { return gateBox(g, false); }).join('');

  var segs = segments(events);
  var t0 = segs.length ? segs[0].start : started;
  var span = Math.max(endT - t0, 1);
  h += '<div class="wf">' + segs.map(function(s, i) {
    var st = stepFor(steps, s);
    var left = (s.start - t0) / span * 100, width = (s.end - s.start) / span * 100;
    var sub = s.type === 'agent' ? (st ? esc(agentOf[st.ID] || '') + (st.Attempt > 1 ? ' · attempt ' + st.Attempt : '') : '')
            : s.type === 'wait' ? 'waiting' : s.type === 'gate' ? 'gate' : 'needs attention';
    var right = dur(s.end - s.start) + (st && st.CostUSD ? ' · ' + usd(st.CostUSD) : '');
    var outcome = st && st.Outcome ? ' → ' + esc(st.Outcome) : '';
    return '<div class="row' + (selSpan === i ? ' sel' : '') + '" onclick="selSpan = (selSpan===' + i + ' ? null : ' + i + '); renderDetail(lastDetail)">' +
      '<div class="name">' + esc(s.state) + outcome + '<div class="sub">' + sub + '</div></div>' +
      '<div class="track"><div class="bar b-' + s.type + (s.live && i === segs.length-1 ? ' live' : '') + '" style="left:' + left + '%;width:' + Math.max(width, 1) + '%"></div></div>' +
      '<div class="right">' + right + '</div></div>';
  }).join('') + '</div>';

  if (selSpan != null && segs[selSpan]) h += spanDetail(segs[selSpan], steps, agentOf, events);

  h += '<details class="raw"><summary>raw events (' + events.length + ')</summary><table>' +
    events.map(function(e) {
      return '<tr><td class="dim">' + clock(e.At) + '</td><td>' + esc(e.Kind) + '</td><td>' + esc(e.State) + '</td>' +
        '<td class="d">' + esc(Object.entries(e.Data || {}).map(function(p){ return p[0] + '=' + JSON.stringify(p[1]); }).join(' ')) + '</td></tr>';
    }).join('') + '</table></details>';

  document.getElementById('main').innerHTML = h;
}

function kv(k, v) { return '<div class="kv"><div class="k">' + k + '</div><div class="v">' + v + '</div></div>'; }

function stepFor(steps, seg) {
  var best = null;
  steps.forEach(function(s) {
    var t = +new Date(s.StartedAt);
    if (s.State === seg.state && t >= seg.start - 3000 && t <= seg.end + 1000) best = s;
  });
  return best;
}

function spanDetail(seg, steps, agentOf, events) {
  var st = stepFor(steps, seg);
  var h = '<div class="spandetail"><h3>' + esc(seg.state) + '</h3>';
  if (st) {
    h += '<div class="meta">' + esc(agentOf[st.ID] || '') + ' · attempt ' + st.Attempt +
      (st.CostUSD ? ' · ' + usd(st.CostUSD) : '') +
      (st.SessionID ? ' · session <span class="mono">' + esc(st.SessionID) + '</span>' : '') + '</div>';
    if (st.SessionPath) h += '<div class="meta">resume: <span class="mono">claude --resume ' + esc(st.SessionID) + '</span> · transcript <span class="mono">' + esc(st.SessionPath) + '</span></div>';
    if (st.Error) h += '<div class="err">' + esc(st.Error) + '</div>';
    if (st.Inputs && Object.keys(st.Inputs).length) h += '<div class="lbl">inputs</div><pre>' + esc(JSON.stringify(st.Inputs, null, 2)) + '</pre>';
    if (st.Result && Object.keys(st.Result).length) h += '<div class="lbl">result</div><pre>' + esc(JSON.stringify(st.Result, null, 2)) + '</pre>';
  } else {
    var evs = events.filter(function(e) { return e.State === seg.state; });
    h += '<div class="lbl">events in this span</div><pre>' + esc(evs.map(function(e) {
      return clock(e.At) + '  ' + e.Kind + '  ' + JSON.stringify(e.Data || {});
    }).join('\n')) + '</pre>';
  }
  return h + '</div>';
}

tick();
setInterval(tick, 4000);
</script>
</body>
</html>`
