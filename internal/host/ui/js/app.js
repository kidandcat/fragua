// Fragua observer UI: watch the agent work, step in when it matters.

import { runScript, getJSON, getSVG, cancelOp, save, connectEvents } from './api.js';
import { Canvas } from './canvas.js';
import {
  buildLayerPanel, syncLayerPanel, renderInspector, renderChecks,
  renderStatus, Log,
} from './panels.js';

const $ = (id) => document.getElementById(id);
const REFRESH_MIN_MS = 700;

const ui = {
  log: new Log($('log')),
  view: 'board',
  summary: null,
  state: null,
  stateStale: true,
  checks: { drc: null, erc: null },
  errorsOnly: false,
  op: null,
  opTimer: 0,
  layerOrder: [],
  history: [],
  histIdx: -1,
};

const canvas = new Canvas($('board'), {
  onZoom: (k) => { $('zoom-label').textContent = `${Math.round(k * 100)}%`; },
  onHover: showTooltip,
  onSelect: (sel) => showInspector(sel),
  onMove: (ref, x, y) => issue(`move ${ref} ${x.toFixed(2)} ${y.toFixed(2)}`),
  onMarker: (id) => {
    const v = allViolations().find((x) => x.id === id);
    if (v) pickViolation(v);
  },
});

// ── talking to the host ─────────────────────────────────────────

// issue runs a script line the UI built itself, echoing it first so the
// human learns the verb behind the gesture.
async function issue(line) {
  ui.log.command(line);
  const r = await runScript(line);
  ui.log.results(r.text);
  $('st-last').textContent = line.split('\n')[0];
  ui.stateStale = true;
  scheduleRefresh(0);
  return r;
}

let refreshTimer = 0;
let refreshing = false;
let lastRefresh = 0;

function scheduleRefresh(delay) {
  clearTimeout(refreshTimer);
  const wait = Math.max(delay || 0, REFRESH_MIN_MS - (Date.now() - lastRefresh));
  refreshTimer = setTimeout(refresh, Math.max(0, wait));
}

async function refresh() {
  // A long op holds the project's write lock, so /screenshot and /summary
  // would block behind it. The progress bar carries the news instead; the
  // canvas catches up the moment the op ends.
  if (ui.op || refreshing) return;
  refreshing = true;
  lastRefresh = Date.now();
  try {
    const [svg, summary] = await Promise.all([
      getSVG(ui.view === 'board' ? '/screenshot' : '/schematic'),
      getJSON('/summary'),
    ]);
    canvas.setSVG(svg);
    if (ui.view === 'board') {
      ui.layerOrder = buildLayerPanel($('layers'), $('overlays'), canvas, null);
      canvas.setMarkers(allViolations());
    }
    applySummary(summary);
    setConn('live');
  } catch (e) {
    ui.log.sys(String(e));
    setConn('down');
  } finally {
    refreshing = false;
  }
}

function applySummary(s) {
  ui.summary = s;
  $('project-name').textContent = s.name || 'untitled';
  const path = $('project-path');
  path.textContent = s.path || 'in memory — not saved to disk';
  path.title = s.path || '';
  renderStatus(
    { outline: $('st-outline'), layers: $('st-layers'), parts: $('st-parts'), nets: $('st-nets'), drc: $('st-drc') },
    s,
    ui.checks
  );
}

async function ensureState() {
  if (ui.state && !ui.stateStale) return ui.state;
  ui.state = await getJSON('/state');
  ui.stateStale = false;
  return ui.state;
}

// ── inspector ───────────────────────────────────────────────────

async function showInspector(sel) {
  const el = $('inspector');
  if (sel && (sel.kind === 'pad' || sel.kind === 'trace' || sel.kind === 'via' || sel.kind === 'copper')) {
    canvas.highlightNet(sel.net || null);
  }
  const state = await ensureState().catch(() => null);
  let part = null;
  if (sel && sel.kind === 'footprint') {
    const fp = findFP(state, sel.ref);
    if (fp && fp.key) part = await getJSON(`/part?key=${encodeURIComponent(fp.key)}`).catch(() => null);
  }
  showPane('inspect');
  renderInspector(el, sel, {
    state,
    part,
    onNet: (net) => canvas.highlightNet(net),
    onScript: (line) => issue(line),
  });
}

function findFP(state, ref) {
  const fps = ((state || {}).board || {}).footprints || {};
  for (const id of Object.keys(fps)) if (fps[id] && fps[id].reference === ref) return fps[id];
  return null;
}

function showTooltip(hit, ev) {
  const tip = $('tooltip');
  if (!hit || !ev) {
    tip.hidden = true;
    return;
  }
  const text =
    hit.kind === 'pad' ? `${hit.ref}.${hit.padName} · ${hit.net || 'no net'}` :
    hit.kind === 'footprint' ? hit.ref :
    hit.kind === 'marker' ? 'violation' :
    `${hit.kind}${hit.net ? ' · ' + hit.net : ''}${hit.layer ? ' · ' + hit.layer : ''}`;
  tip.textContent = text;
  const r = $('canvas').getBoundingClientRect();
  tip.style.left = `${Math.min(ev.clientX - r.left + 14, r.width - 200)}px`;
  tip.style.top = `${ev.clientY - r.top + 16}px`;
  tip.hidden = false;
}

// ── checks ──────────────────────────────────────────────────────

function allViolations() {
  return [...(ui.checks.drc ? ui.checks.drc.violations : []), ...(ui.checks.erc ? ui.checks.erc.violations : [])];
}

async function runCheck(kind) {
  const btn = kind === 'drc' ? $('btn-run-drc') : $('btn-run-erc');
  btn.disabled = true;
  try {
    ui.checks[kind] = await getJSON('/' + kind);
    ui.log.command(kind);
    ui.log.results(`ok ${kind}: ${ui.checks[kind].summary}`);
  } catch (e) {
    ui.log.sys(String(e));
  } finally {
    btn.disabled = false;
  }
  paintChecks();
  canvas.setMarkers(allViolations());
  applySummary(ui.summary || {});
}

// invalidateChecks drops the last DRC/ERC run: the board moved, so the
// findings and their markers are about a board that no longer exists.
function invalidateChecks() {
  if (!ui.checks.drc && !ui.checks.erc) return;
  ui.checks = { drc: null, erc: null };
  canvas.setMarkers([]);
  paintChecks();
  if (ui.summary) applySummary(ui.summary);
}

function paintChecks() {
  renderChecks($('checks'), [ui.checks.drc, ui.checks.erc], {
    errorsOnly: ui.errorsOnly,
    onPick: pickViolation,
  });
  const errs = (ui.checks.drc ? ui.checks.drc.errors : 0) + (ui.checks.erc ? ui.checks.erc.errors : 0);
  const warns = (ui.checks.drc ? ui.checks.drc.warnings : 0) + (ui.checks.erc ? ui.checks.erc.warnings : 0);
  const badge = $('check-badge');
  badge.hidden = !ui.checks.drc && !ui.checks.erc;
  badge.textContent = errs || warns || 0;
  badge.className = 'badge' + (errs ? '' : warns ? ' is-warn' : ' is-clean');
}

async function pickViolation(v) {
  // A board violation means nothing on the schematic sheet: come back first.
  if (ui.view !== 'board') await showView('board');
  showPane('checks');
  if (v.x_mm != null) canvas.focusMarker(v);
  if (v.net) canvas.highlightNet(v.net);
  renderInspector($('inspector'), { kind: 'violation', violation: v }, {
    state: ui.state,
    onNet: (net) => canvas.highlightNet(net),
    onScript: (line) => issue(line),
  });
}

// ── progress ────────────────────────────────────────────────────

function startOp(op) {
  ui.op = { op, started: Date.now(), done: 0, total: 0, detail: '' };
  $('progress').hidden = false;
  $('progress-op').textContent = op;
  $('progress-detail').textContent = '';
  $('progress-fill').classList.add('indeterminate');
  $('progress-fill').style.width = '';
  $('btn-cancel').disabled = false;
  clearInterval(ui.opTimer);
  ui.opTimer = setInterval(tickOp, 100);
}

function tickOp() {
  if (!ui.op) return;
  const s = (Date.now() - ui.op.started) / 1000;
  $('progress-elapsed').textContent = `${s.toFixed(1)} s`;
}

function updateOp(ev) {
  if (!ui.op) startOp(ev.op || 'working');
  ui.op.done = ev.done || 0;
  ui.op.total = ev.total || 0;
  // done is omitted from the event when it is zero, so read it back off the
  // op we just normalised rather than the raw frame.
  $('progress-detail').textContent = ui.op.total
    ? `${ui.op.done}/${ui.op.total}${ev.detail ? ' · ' + ev.detail : ''}`
    : ev.detail || '';
  const fill = $('progress-fill');
  if (ev.total > 0) {
    fill.classList.remove('indeterminate');
    fill.style.width = `${Math.min(100, (ev.done / ev.total) * 100)}%`;
  }
}

function endOp(ev) {
  clearInterval(ui.opTimer);
  ui.op = null;
  $('progress').hidden = true;
  const secs = ((ev.elapsed_ms || 0) / 1000).toFixed(1);
  ui.log.sys(`${ev.op} ${ev.cancelled ? 'cancelled' : 'finished'} after ${secs} s`);
  ui.stateStale = true;
  scheduleRefresh(0);
}

// ── panes / tabs ────────────────────────────────────────────────

function showPane(which) {
  const inspect = which === 'inspect';
  $('pane-inspect').hidden = !inspect;
  $('pane-checks').hidden = inspect;
  $('tab-inspect').classList.toggle('is-on', inspect);
  $('tab-checks').classList.toggle('is-on', !inspect);
  $('tab-inspect').setAttribute('aria-selected', String(inspect));
  $('tab-checks').setAttribute('aria-selected', String(!inspect));
}

function showView(view) {
  ui.view = view;
  const board = view === 'board';
  $('tab-board').classList.toggle('is-on', board);
  $('tab-schematic').classList.toggle('is-on', !board);
  $('tab-board').setAttribute('aria-selected', String(board));
  $('tab-schematic').setAttribute('aria-selected', String(!board));
  $('rail-left').querySelectorAll('.card')[0].hidden = !board;
  canvas.select(null);
  return refresh().then(() => canvas.fit());
}

function setConn(state) {
  const el = $('conn');
  el.classList.toggle('is-live', state === 'live');
  el.classList.toggle('is-down', state === 'down');
  $('conn-label').textContent = state === 'live' ? 'live' : state === 'down' ? 'reconnecting' : 'connecting';
}

// ── console ─────────────────────────────────────────────────────

async function runConsole() {
  const box = $('script');
  const src = box.value.trim();
  if (!src) return;
  ui.history.push(src);
  ui.histIdx = ui.history.length;
  $('btn-run').disabled = true;
  ui.log.command(src);
  $('st-last').textContent = src.split('\n')[0];
  try {
    const r = await runScript(src);
    ui.log.results(r.text);
  } catch (e) {
    ui.log.sys(String(e));
  } finally {
    $('btn-run').disabled = false;
  }
  ui.stateStale = true;
  scheduleRefresh(0);
}

function historyStep(dir) {
  if (!ui.history.length) return false;
  const next = Math.min(ui.history.length, Math.max(0, ui.histIdx + dir));
  ui.histIdx = next;
  $('script').value = next === ui.history.length ? '' : ui.history[next];
  return true;
}

// ── wiring ──────────────────────────────────────────────────────

$('btn-fit').onclick = () => canvas.fit();
$('btn-zoom-in').onclick = () => canvas.zoomBy(1.25);
$('btn-zoom-out').onclick = () => canvas.zoomBy(1 / 1.25);
$('btn-view-top').onclick = () => setSide(false);
$('btn-view-bottom').onclick = () => setSide(true);

function setSide(bottom) {
  canvas.setFlip(bottom);
  $('btn-view-top').classList.toggle('is-on', !bottom);
  $('btn-view-bottom').classList.toggle('is-on', bottom);
  $('btn-view-top').setAttribute('aria-pressed', String(!bottom));
  $('btn-view-bottom').setAttribute('aria-pressed', String(bottom));
}

document.querySelectorAll('[data-script]').forEach((b) => {
  b.onclick = () => issue(b.dataset.script);
});

$('btn-download').onclick = () => {
  const text = canvas.toSVGText();
  if (!text) return;
  const url = URL.createObjectURL(new Blob([text], { type: 'image/svg+xml' }));
  const a = document.createElement('a');
  a.href = url;
  a.download = `${(ui.summary && ui.summary.name) || 'board'}-${ui.view}.svg`;
  a.click();
  URL.revokeObjectURL(url);
};

$('btn-save').onclick = async () => {
  let path = ui.summary && ui.summary.path;
  if (!path) {
    path = window.prompt('Absolute path for the .fragua file:', '/tmp/board.fragua');
    if (!path) return;
  }
  const r = await save(path);
  ui.log[r.ok ? 'results' : 'sys'](r.text || 'save failed');
  scheduleRefresh(0);
};

$('btn-cancel').onclick = async () => {
  $('btn-cancel').disabled = true;
  const r = await cancelOp();
  ui.log.sys(r.cancelled ? `cancel requested for ${r.op}` : 'nothing to cancel');
};

$('btn-run').onclick = runConsole;
$('btn-clear-log').onclick = () => ui.log.clear();
$('btn-copy-script').onclick = async () => {
  const text = ui.log.issued.join('\n');
  if (!text) {
    ui.log.sys('nothing issued from the UI yet');
    return;
  }
  try {
    await navigator.clipboard.writeText(text);
    ui.log.sys(`copied ${ui.log.issued.length} script lines`);
  } catch (_) {
    ui.log.sys(text);
  }
};

$('btn-run-drc').onclick = () => runCheck('drc');
$('btn-run-erc').onclick = () => runCheck('erc');
$('chk-errors-only').onchange = (ev) => {
  ui.errorsOnly = ev.target.checked;
  paintChecks();
};

$('tab-board').onclick = () => showView('board');
$('tab-schematic').onclick = () => showView('schematic');
$('tab-inspect').onclick = () => showPane('inspect');
$('tab-checks').onclick = () => showPane('checks');

$('script').addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter' && (ev.metaKey || ev.ctrlKey)) {
    ev.preventDefault();
    runConsole();
    return;
  }
  const box = ev.target;
  if (ev.key === 'ArrowUp' && box.selectionStart === 0 && historyStep(-1)) ev.preventDefault();
  if (ev.key === 'ArrowDown' && box.selectionStart === box.value.length && historyStep(1)) ev.preventDefault();
});

document.addEventListener('keydown', (ev) => {
  const t = ev.target;
  if (t && (t.tagName === 'TEXTAREA' || t.tagName === 'INPUT')) return;
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
  const k = ev.key.toLowerCase();
  if (k === 'f') { canvas.fit(); ev.preventDefault(); }
  else if (k === 'escape') { canvas.select(null); canvas.highlightNet(null); }
  else if (k === 'r' && canvas.selection && canvas.selection.ref) {
    const pose = canvas.footprintPose(canvas.selection.ref);
    if (pose) issue(`rotate ${canvas.selection.ref} ${(pose.rot + 90) % 360}`);
  } else if (k === '+' || k === '=') canvas.zoomBy(1.25);
  else if (k === '-') canvas.zoomBy(1 / 1.25);
  else if (k >= '1' && k <= '9') {
    const name = ui.layerOrder[Number(k) - 1];
    if (name) {
      canvas.setLayer(name, !canvas.isLayerVisible(name));
      syncLayerPanel($('rail-left'), canvas);
    }
  }
});

connectEvents(
  (ev) => {
    switch (ev.kind) {
      case 'op_start': startOp(ev.op); break;
      case 'progress': updateOp(ev); break;
      case 'op_end': endOp(ev); break;
      case 'saved': ui.log.sys(`saved ${ev.path}`); scheduleRefresh(0); break;
      case 'hello': break;
      default:
        ui.stateStale = true;
        invalidateChecks();
        scheduleRefresh(0);
    }
  },
  (state) => setConn(state)
);

ui.log.sys('fragua observer — the agent and this page share one live project');
showPane('inspect');
renderInspector($('inspector'), null, {});
refresh().then(() => canvas.fit());
// Slow poll so a change that arrived while the stream was down still lands.
setInterval(() => scheduleRefresh(0), 8000);
