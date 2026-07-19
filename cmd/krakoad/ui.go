package main

// The audit UI: one self-contained page (inline CSS/JS, zero external
// assets) over the existing JSON endpoints. Runs table; per run a step
// sidebar with a reading pane that renders step inputs/results markdown-
// friendly (tickets and reviews are markdown — show them as documents, not
// escaped JSON). Raw events stay collapsed at the bottom.

const uiHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' rx='3' fill='%23161b22'/%3E%3Cpath d='M4 3v10M4 8l5-5M4 8l5 5' stroke='%2358a6ff' stroke-width='2' fill='none'/%3E%3C/svg%3E">
<title>krakoa</title>
<style>
  :root {
    --bg:#0e1116; --panel:#161b22; --panel2:#1c232c; --line:#232a33;
    --fg:#d5dce4; --dim:#8b96a3; --faint:#5a6572;
    --blue:#58a6ff; --amber:#d4a72c; --red:#f85149; --green:#3fb950; --purple:#bc8cff;
  }
  * { box-sizing:border-box; margin:0; }
  body { background:var(--bg); color:var(--fg);
         font:13px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
  code, .mono { font-family:ui-monospace,Menlo,monospace; font-size:12px; }
  a { color:var(--blue); text-decoration:none; cursor:pointer; }

  header { display:flex; align-items:center; gap:14px; padding:10px 20px;
           border-bottom:1px solid var(--line); position:sticky; top:0; background:var(--bg); z-index:5; }
  header .brand { font-weight:700; letter-spacing:3px; font-size:13px; }
  header .crumb { color:var(--dim); }
  header .right { margin-left:auto; color:var(--faint); font-size:12px; }
  .gatepill { background:var(--amber); color:#111; border-radius:10px; padding:1px 9px;
              font-weight:600; font-size:12px; }

  main { max-width:1180px; margin:0 auto; padding:18px 20px 60px; }

  .chip { display:inline-block; padding:1px 8px; border-radius:10px; font-size:11px; font-weight:600; }
  .c-running { background:rgba(88,166,255,.15); color:var(--blue); }
  .c-waiting { background:rgba(212,167,44,.15); color:var(--amber); }
  .c-gated   { background:rgba(212,167,44,.25); color:var(--amber); }
  .c-queued  { background:rgba(139,150,163,.15); color:var(--dim); }
  .c-done    { background:rgba(63,185,80,.15); color:var(--green); }
  .c-failed, .c-needs-attention, .c-canceled { background:rgba(248,81,73,.15); color:var(--red); }

  table.runs { width:100%; border-collapse:collapse; }
  .runs th { text-align:left; color:var(--faint); font-size:11px; text-transform:uppercase;
             letter-spacing:.8px; font-weight:600; padding:6px 10px; border-bottom:1px solid var(--line); }
  .runs td { padding:9px 10px; border-bottom:1px solid var(--line); }
  .runs tr.r:hover { background:var(--panel); cursor:pointer; }
  .runs .num { text-align:right; font-family:ui-monospace,Menlo,monospace; font-size:12px; }
  .dim { color:var(--dim); } .faint { color:var(--faint); }

  .gate { border:1px solid rgba(212,167,44,.5); background:rgba(212,167,44,.07);
          border-radius:6px; padding:10px 14px; margin:0 0 14px; }
  .gate.att { border-color:rgba(248,81,73,.6); background:rgba(248,81,73,.07); }
  .gate .q { margin-bottom:6px; }
  .gate code { background:var(--panel2); padding:2px 7px; border-radius:4px; user-select:all; }

  .runhead { display:flex; gap:26px; flex-wrap:wrap; align-items:baseline;
             padding:14px 16px; background:var(--panel); border:1px solid var(--line);
             border-radius:6px; margin-bottom:14px; }
  .runhead .kv .k { color:var(--faint); font-size:11px; text-transform:uppercase; letter-spacing:.8px; }
  .runhead .kv .v { font-size:15px; margin-top:2px; }

  /* sidebar + reading pane */
  .detail { display:grid; grid-template-columns:270px minmax(0,1fr); gap:16px; align-items:start; }
  @media (max-width:800px) { .detail { grid-template-columns:1fr; } }
  .side { border:1px solid var(--line); border-radius:6px; overflow:hidden; position:sticky; top:56px; }
  .snode { padding:9px 12px; border-bottom:1px solid var(--line); cursor:pointer;
           border-left:3px solid transparent; }
  .snode:last-child { border-bottom:none; }
  .snode:hover { background:var(--panel); }
  .snode.sel { background:var(--panel); border-left-color:var(--blue); }
  .snode .n { display:flex; justify-content:space-between; gap:8px; }
  .snode .n .st { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .snode .n .d { color:var(--faint); font-size:11px; font-family:ui-monospace,Menlo,monospace; white-space:nowrap; }
  .snode .sub { color:var(--faint); font-size:11px; margin-top:1px; }
  .dot { display:inline-block; width:7px; height:7px; border-radius:50%; margin-right:6px; vertical-align:1px; }
  .d-agent { background:var(--blue); } .d-wait { background:var(--amber); }
  .d-gate { background:var(--purple); } .d-park { background:var(--red); }
  .dot.live { animation:pulse 1.6s ease-in-out infinite; }
  @keyframes pulse { 0%,100% { opacity:1; } 50% { opacity:.25; } }
  @media (prefers-reduced-motion: reduce) { .dot.live { animation:none; } }

  .content { border:1px solid var(--line); border-radius:6px; background:var(--panel);
             padding:16px 20px; min-height:200px; }
  .content .meta { color:var(--dim); font-size:12px; margin-bottom:4px; }
  .content .meta .mono { user-select:all; }
  .content h2.title { font-size:15px; margin-bottom:6px; }
  .lbl { color:var(--faint); font-size:11px; text-transform:uppercase; letter-spacing:.8px;
         margin:14px 0 4px; }
  .kvline { margin:3px 0; } .kvline .lbl { display:inline; margin:0 6px 0 0; }
  pre { background:var(--bg); border:1px solid var(--line); border-radius:5px;
        padding:10px 12px; overflow-x:auto; font-size:12px; line-height:1.5;
        max-height:340px; overflow-y:auto; margin:4px 0; font-family:ui-monospace,Menlo,monospace; }
  .err { color:var(--red); white-space:pre-wrap; }

  /* rendered markdown */
  .prose { background:var(--bg); border:1px solid var(--line); border-radius:5px;
           padding:14px 18px; margin:4px 0; max-width:72ch; }
  .prose h1,.prose h2,.prose h3,.prose h4 { margin:12px 0 6px; line-height:1.3; }
  .prose h1 { font-size:16px; } .prose h2 { font-size:14px; color:var(--fg); }
  .prose h3,.prose h4 { font-size:13px; color:var(--dim); text-transform:uppercase; letter-spacing:.6px; }
  .prose h1:first-child,.prose h2:first-child { margin-top:0; }
  .prose p { margin:6px 0; }
  .prose ul,.prose ol { margin:6px 0 6px 22px; }
  .prose li { margin:2px 0; }
  .prose code { background:var(--panel2); padding:1px 5px; border-radius:3px; }
  .prose pre { margin:8px 0; } .prose pre code { background:none; padding:0; }
  .prose blockquote { border-left:3px solid var(--line); padding-left:10px; color:var(--dim); margin:6px 0; }
  .prose hr { border:none; border-top:1px solid var(--line); margin:10px 0; }
  .prose .todo { color:var(--dim); }

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
var runId = null, selKey = null, gates = [], lastDetail = null;

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
function ago(t) { return dur(Date.now() - new Date(t)); }
function clock(t) { return t ? new Date(t).toLocaleTimeString() : ''; }

/* ---- tiny markdown renderer (headings, lists, checkboxes, code, bold,
   links, quotes, hr) — enough for tickets and reviews ---- */
function mdInline(s) {
  s = esc(s);
  s = s.replace(/\x60([^\x60]+)\x60/g, '<code>$1</code>');
  s = s.replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
  s = s.replace(/\[([^\]]+)\]\((https?:[^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  s = s.replace(/(^|\s)(https?:\/\/[^\s<]+)/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>');
  return s;
}
function md(src) {
  var out = [], list = null, code = false, para = [];
  function flushP() { if (para.length) { out.push('<p>' + para.join('<br>') + '</p>'); para = []; } }
  function closeList() { if (list) { out.push('</' + list + '>'); list = null; } }
  String(src).split('\n').forEach(function(l) {
    if (/^\s*\x60\x60\x60/.test(l)) {
      flushP(); closeList();
      code = !code; out.push(code ? '<pre><code>' : '</code></pre>');
      return;
    }
    if (code) { out.push(esc(l) + '\n'); return; }
    var m;
    if ((m = l.match(/^(#{1,4})\s+(.*)/))) { flushP(); closeList(); var lv = m[1].length;
      out.push('<h' + lv + '>' + mdInline(m[2]) + '</h' + lv + '>'); return; }
    if (/^\s*(---+|\*\*\*+)\s*$/.test(l)) { flushP(); closeList(); out.push('<hr>'); return; }
    if ((m = l.match(/^\s*[-*]\s+\[( |x|X)\]\s+(.*)/))) { flushP();
      if (list !== 'ul') { closeList(); out.push('<ul>'); list = 'ul'; }
      var done = m[1] !== ' ';
      out.push('<li class="' + (done ? '' : 'todo') + '">' + (done ? '☑' : '☐') + ' ' + mdInline(m[2]) + '</li>'); return; }
    if ((m = l.match(/^\s*[-*]\s+(.*)/))) { flushP();
      if (list !== 'ul') { closeList(); out.push('<ul>'); list = 'ul'; }
      out.push('<li>' + mdInline(m[1]) + '</li>'); return; }
    if ((m = l.match(/^\s*\d+[.)]\s+(.*)/))) { flushP();
      if (list !== 'ol') { closeList(); out.push('<ol>'); list = 'ol'; }
      out.push('<li>' + mdInline(m[1]) + '</li>'); return; }
    if ((m = l.match(/^>\s?(.*)/))) { flushP(); closeList(); out.push('<blockquote>' + mdInline(m[1]) + '</blockquote>'); return; }
    if (/^\s*$/.test(l)) { flushP(); closeList(); return; }
    closeList(); para.push(mdInline(l));
  });
  flushP(); closeList();
  return '<div class="prose">' + out.join('') + '</div>';
}

function tick() {
  Promise.all([j('/v1/runs'), j('/v1/gates')]).then(function(res) {
    var runs = res[0] || []; gates = res[1] || [];
    document.getElementById('stat').textContent = 'updated ' + new Date().toLocaleTimeString();
    document.getElementById('gates-pill').innerHTML =
      gates.length ? '<span class="gatepill">' + gates.length + ' gate' + (gates.length > 1 ? 's' : '') + ' waiting</span>' : '';
    if (runId) {
      j('/v1/runs/' + runId).then(function(d) { lastDetail = d; renderDetail(d); });
    } else {
      renderList(runs);
    }
  }).catch(function() {
    document.getElementById('stat').textContent = 'daemon unreachable';
  });
}

function openRun(id) { runId = id; selKey = null; tick(); }
function home() { runId = null; selKey = null; tick(); }
function selectNode(k) { selKey = k; if (lastDetail) renderDetail(lastDetail); }

function renderList(runs) {
  document.getElementById('crumb').textContent = 'runs';
  var h = gates.map(function(g) { return gateBox(g, true); }).join('');
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
  runs.slice(0, 20).forEach(function(r) {
    j('/v1/runs/' + r.ID).then(function(d) {
      var el = document.getElementById('cost-' + r.ID);
      if (!el) return;
      el.textContent = usd((d.steps || []).reduce(function(a, s) { return a + (s.CostUSD || 0); }, 0));
    });
  });
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

/* Fold the event log into state visits: one sidebar node per stay. */
function segments(events) {
  var marks = {'run-admitted':'start','step-started':'agent','wait-armed':'wait','gate-opened':'gate','parked':'park'};
  var segs = [], cur = null;
  events.forEach(function(e) {
    var type = marks[e.Kind];
    if (!type || !e.State) return;
    var t = +new Date(e.At);
    if (cur && cur.state === e.State) {
      if (cur.type === 'start' && type !== 'start') cur.type = type;
      else if (type === 'park') cur.type = 'park';
      return;
    }
    if (cur) cur.end = t;
    cur = { state: e.State, type: type === 'start' ? 'agent' : type, start: t, end: null };
    segs.push(cur);
  });
  var done = events.some(function(e) { return e.Kind === 'run-finished'; });
  if (cur && !cur.end) cur.end = done && events.length ? +new Date(events[events.length - 1].At) : Date.now();
  if (cur) cur.live = !done;
  segs.forEach(function(s, i) { s.key = i + ':' + s.state; });
  return segs;
}

function stepFor(steps, seg) {
  var best = null;
  steps.forEach(function(s) {
    var t = +new Date(s.StartedAt);
    if (s.State === seg.state && t >= seg.start - 3000 && t <= seg.end + 1000) best = s;
  });
  return best;
}

function renderDetail(d) {
  var r = d.run, steps = d.steps || [], events = d.events || [];
  document.getElementById('crumb').innerHTML = '<a onclick="home()">runs</a> / <span class="mono">' + esc(r.ID) + '</span>';

  var agentOf = {};
  events.forEach(function(e) { if (e.Kind === 'step-started' && e.Data) agentOf[e.Data.step] = e.Data.agent; });
  var totalCost = steps.reduce(function(a, s) { return a + (s.CostUSD || 0); }, 0);
  var done = events.some(function(e) { return e.Kind === 'run-finished'; });
  var endT = done ? +new Date(events[events.length - 1].At) : Date.now();
  var myGates = gates.filter(function(g) { return g.RunID === r.ID; });
  var segs = segments(events);

  var h = '<div class="runhead">' +
    kv('status', '<span class="chip c-' + esc(r.Status) + '">' + esc(r.Status) + '</span>') +
    kv('now', esc(r.State) + ' <span class="dim">for ' + ago(r.UpdatedAt) + '</span>') +
    kv('duration', dur(endT - +new Date(r.CreatedAt))) +
    kv('cost', usd(totalCost) || '—') +
    kv('steps', String(steps.length)) +
    '</div>';
  h += myGates.map(function(g) { return gateBox(g, false); }).join('');

  // default selection: the current (last) node, following the run live
  var sel = null;
  segs.forEach(function(s) { if (s.key === selKey) sel = s; });
  if (!sel && segs.length) sel = segs[segs.length - 1];

  h += '<div class="detail"><div class="side">' + segs.map(function(s) {
    var st = stepFor(steps, s);
    var sub = s.type === 'agent' ? esc((st && agentOf[st.ID]) || '') : s.type === 'wait' ? 'waiting' : s.type === 'gate' ? 'gate' : 'needs attention';
    var outcome = st && st.Outcome ? ' → ' + esc(st.Outcome) : (s.type === 'agent' && s.live ? ' …' : '');
    return '<div class="snode' + (sel && s.key === sel.key ? ' sel' : '') + '" onclick="selectNode(\'' + esc(s.key) + '\')">' +
      '<div class="n"><span class="st"><span class="dot d-' + s.type + (s.live ? ' live' : '') + '"></span>' + esc(s.state) + outcome + '</span>' +
      '<span class="d">' + dur(s.end - s.start) + '</span></div>' +
      '<div class="sub">' + sub + (st && st.CostUSD ? ' · ' + usd(st.CostUSD) : '') + '</div></div>';
  }).join('') + '</div>';

  h += '<div class="content">' + (sel ? nodeContent(sel, steps, agentOf, events) : '<div class="empty">no activity yet</div>') + '</div></div>';

  h += '<details class="raw"><summary>raw events (' + events.length + ')</summary><table>' +
    events.map(function(e) {
      return '<tr><td class="dim">' + clock(e.At) + '</td><td>' + esc(e.Kind) + '</td><td>' + esc(e.State) + '</td>' +
        '<td class="d">' + esc(Object.entries(e.Data || {}).map(function(p) { return p[0] + '=' + JSON.stringify(p[1]); }).join(' ')) + '</td></tr>';
    }).join('') + '</table></details>';

  document.getElementById('main').innerHTML = h;
}

function kv(k, v) { return '<div class="kv"><div class="k">' + k + '</div><div class="v">' + v + '</div></div>'; }

/* One field of inputs/result: markdown-ish strings become documents,
   short scalars stay inline, structures stay JSON. */
function field(k, v) {
  if (v == null || v === '') return '';
  if (typeof v === 'string') {
    if (v.indexOf('\n') >= 0 || v.length > 100 || /^#/.test(v)) {
      return '<div class="lbl">' + esc(k) + '</div>' + md(v);
    }
    return '<div class="kvline"><span class="lbl">' + esc(k) + '</span> ' + mdInline(v) + '</div>';
  }
  if (typeof v === 'number' || typeof v === 'boolean') {
    return '<div class="kvline"><span class="lbl">' + esc(k) + '</span> ' + esc(String(v)) + '</div>';
  }
  return '<div class="lbl">' + esc(k) + '</div><pre>' + esc(JSON.stringify(v, null, 2)) + '</pre>';
}

function fields(obj, skip) {
  return Object.keys(obj || {}).filter(function(k) { return skip.indexOf(k) < 0; })
    .sort().map(function(k) { return field(k, obj[k]); }).join('');
}

function nodeContent(seg, steps, agentOf, events) {
  var st = stepFor(steps, seg);
  var h = '<h2 class="title">' + esc(seg.state) +
    (st && st.Outcome ? ' <span class="chip c-' + (st.Error ? 'failed' : 'done') + '">' + esc(st.Outcome) + '</span>' : '') + '</h2>';

  if (st) {
    h += '<div class="meta">' + esc(agentOf[st.ID] || '') + ' · attempt ' + st.Attempt +
      (st.CostUSD ? ' · ' + usd(st.CostUSD) : '') + ' · ' + clock(st.StartedAt) +
      (st.EndedAt ? ' → ' + clock(st.EndedAt) : ' (running)') + '</div>';
    if (st.SessionID) {
      h += '<div class="meta">session <span class="mono">' + esc(st.SessionID) + '</span>' +
        ' · resume: <span class="mono">claude --resume ' + esc(st.SessionID) + '</span></div>';
    }
    if (st.Error) h += '<div class="lbl">error</div><div class="err">' + esc(st.Error) + '</div>';
    var res = st.Result || {};
    var body = fields(res, ['outcome']);
    if (body) h += body;
    var inp = fields(st.Inputs || {}, []);
    if (inp) h += '<details><summary class="lbl" style="cursor:pointer">inputs</summary>' + inp + '</details>';
  } else if (seg.type === 'gate') {
    var ge = null;
    events.forEach(function(e) { if (e.Kind === 'gate-opened' && e.State === seg.state) ge = e; });
    var ans = null;
    events.forEach(function(e) { if (e.Kind === 'gate-answered' && e.State === seg.state) ans = e; });
    if (ge) h += field('question', (ge.Data || {}).payload);
    if (ans) h += '<div class="kvline"><span class="lbl">answered</span> ' + esc((ans.Data || {}).response) + ' <span class="faint">by ' + esc((ans.Data || {}).responder) + '</span></div>';
    else h += '<div class="dim">waiting for an answer — see the gate banner above.</div>';
  } else if (seg.type === 'wait') {
    var fired = null;
    events.forEach(function(e) { if (e.State === seg.state && (e.Kind === 'timer-fired' || e.Kind === 'signal-consumed')) fired = e; });
    h += '<div class="dim">' + (seg.live ? 'Waiting on external events (watcher sweeps every cadence; a timeout arm is the backstop).'
      : 'Wait ended' + (fired ? ': ' + esc(fired.Kind) : '') + '.') + '</div>';
    var evs = events.filter(function(e) { return e.State === seg.state; });
    h += '<div class="lbl">events in this span</div><pre>' + esc(evs.map(function(e) {
      return clock(e.At) + '  ' + e.Kind + '  ' + JSON.stringify(e.Data || {});
    }).join('\n')) + '</pre>';
  } else {
    var evs2 = events.filter(function(e) { return e.State === seg.state; });
    h += '<div class="lbl">events</div><pre>' + esc(evs2.map(function(e) {
      return clock(e.At) + '  ' + e.Kind + '  ' + JSON.stringify(e.Data || {});
    }).join('\n')) + '</pre>';
  }
  return h;
}

tick();
setInterval(tick, 4000);
</script>
</body>
</html>`
