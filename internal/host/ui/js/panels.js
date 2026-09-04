// Side panels: inspector, DRC/ERC list, layer switches, log, status bar.

export const esc = (s) =>
  String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));

const fmt = (n, d = 2) => (Number.isFinite(n) ? n.toFixed(d) : '—');

// ── layers ──────────────────────────────────────────────────────

const SWATCH = {
  'F.Cu': '#c97a2b', 'B.Cu': '#2b6cc9', 'In1.Cu': '#3aa66c', 'In2.Cu': '#a63a8c',
  pours: '#4a6b7a', vias: '#7d8590', drills: '#404752', mask: '#5cd6a0',
  silk: '#e6edf3', edge: '#d6905b', ratsnest: '#7d8da8', drc: '#ff6b6b',
  'pad-names': '#8b949e', footprints: '#8b949e', background: '#222a35',
  substrate: '#5a3a1f',
};

// Overlays the human toggles most; copper layers come from the board itself.
const OVERLAYS = ['pours', 'vias', 'drills', 'mask', 'silk', 'pad-names', 'edge', 'ratsnest', 'drc'];

// buildLayerPanel renders the copper stack and the overlays, numbering the
// first nine so the keyboard shortcuts and the panel agree. Returns the
// ordered names, which is what keys 1..9 index into.
export function buildLayerPanel(copperEl, overlayEl, canvas, onChange) {
  const copper = canvas.copperLayers();
  const known = canvas.knownLayers();
  const ordered = [...copper, ...OVERLAYS.filter((n) => known.includes(n))];
  const keyOf = (n) => {
    const i = ordered.indexOf(n);
    return i >= 0 && i < 9 ? String(i + 1) : '';
  };
  const row = (name, key) => {
    const b = document.createElement('button');
    b.className = 'layer';
    b.type = 'button';
    b.dataset.layer = name;
    b.setAttribute('aria-pressed', String(canvas.isLayerVisible(name)));
    b.innerHTML =
      `<span class="swatch" style="--sw:${SWATCH[name] || '#6b7686'}"></span>` +
      `<span class="name">${esc(name)}</span>` +
      (key ? `<span class="key">${esc(key)}</span>` : '');
    b.classList.toggle('is-off', !canvas.isLayerVisible(name));
    b.onclick = () => {
      const on = !canvas.isLayerVisible(name);
      canvas.setLayer(name, on);
      b.classList.toggle('is-off', !on);
      b.setAttribute('aria-pressed', String(on));
      if (onChange) onChange();
    };
    return b;
  };
  copperEl.replaceChildren(...copper.map((n) => row(n, keyOf(n))));
  overlayEl.replaceChildren(
    ...OVERLAYS.filter((n) => known.includes(n)).map((n) => row(n, keyOf(n)))
  );
  return ordered;
}

export function syncLayerPanel(rootEl, canvas) {
  rootEl.querySelectorAll('.layer').forEach((b) => {
    const on = canvas.isLayerVisible(b.dataset.layer);
    b.classList.toggle('is-off', !on);
    b.setAttribute('aria-pressed', String(on));
  });
}

// ── inspector ───────────────────────────────────────────────────

function kv(pairs) {
  const rows = pairs
    .filter(([, v]) => v !== '' && v != null)
    .map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`)
    .join('');
  return `<dl class="kv">${rows}</dl>`;
}

// netFacts walks the project state once for everything the net inspector
// shows: copper length, widths, vias and the pads it has to reach.
export function netFacts(state, net) {
  const out = { net, lengthMM: 0, traces: 0, vias: 0, pads: 0, widths: new Set(), layers: new Set(), cls: '' };
  const b = state && state.board;
  if (!b) return out;
  for (const t of b.traces || []) {
    if (t.net !== net) continue;
    out.traces++;
    const dx = (t.end.x - t.start.x) / 1e6;
    const dy = (t.end.y - t.start.y) / 1e6;
    out.lengthMM += Math.hypot(dx, dy);
    out.widths.add((t.width / 1e6).toFixed(3));
    out.layers.add(layerName(state, t.layer));
  }
  for (const v of b.vias || []) if (v.net === net) out.vias++;
  for (const id of Object.keys(b.footprints || {})) {
    const fp = b.footprints[id];
    for (const p of (fp && fp.pads) || []) if (p.net === net) out.pads++;
  }
  const sch = state.schematic || {};
  out.cls = (sch.net_to_class || {})[net] || ((sch.nets || {})[net] || {}).class || '';
  return out;
}

function layerName(state, layer) {
  const stack = ((state.board || {}).stackup || {}).layers || [];
  const i = typeof layer === 'string' ? (layer === 'Top' ? 0 : 1) : (layer && layer.index) || 0;
  return (stack[i] && stack[i].name) || (i === 0 ? 'F.Cu' : `In${i}.Cu`);
}

export function renderInspector(el, sel, ctx) {
  const { state, part, onNet, onScript } = ctx;
  if (!sel) {
    el.innerHTML =
      '<p class="empty">Nothing selected. Hover or click a pad, a trace or a part.</p>' +
      '<p class="empty">The agent and this page share one live project — anything you move here, the next agent step sees.</p>';
    return;
  }
  if (sel.kind === 'footprint') {
    renderPart(el, sel, state, part, onNet, onScript);
    return;
  }
  if (sel.kind === 'violation') {
    const v = sel.violation;
    el.innerHTML =
      `<h3>${esc(v.kind)}</h3><p class="sub">${esc(v.source)} · ${esc(v.severity)}</p>` +
      kv([
        ['message', esc(v.message)],
        ['net', v.net ? `<a href="#" data-net="${esc(v.net)}">${esc(v.net)}</a>` : ''],
        ['symbol', esc(v.symbol || '')],
        ['at', v.x_mm != null ? `${fmt(v.x_mm)}, ${fmt(v.y_mm)} mm` : ''],
      ]);
    wireNetLinks(el, onNet);
    return;
  }
  renderNet(el, sel, state, onNet);
}

function renderNet(el, sel, state, onNet) {
  const net = sel.net;
  if (!net) {
    el.innerHTML = `<h3>${esc(sel.kind)}</h3><p class="empty">No net on this element.</p>`;
    return;
  }
  const f = netFacts(state, net);
  const where =
    sel.kind === 'pad' ? `${esc(sel.ref)}.${esc(sel.padName)}` :
    sel.kind === 'trace' ? 'trace segment' :
    sel.kind === 'via' ? 'via' : sel.kind;
  el.innerHTML =
    `<h3>${esc(net)}</h3><p class="sub">${where}${sel.layer ? ' · ' + esc(sel.layer) : ''}</p>` +
    kv([
      ['class', esc(f.cls || 'default')],
      ['pads', f.pads],
      ['copper', `${fmt(f.lengthMM)} mm`],
      ['segments', f.traces],
      ['vias', f.vias],
      ['width', f.widths.size ? [...f.widths].join(' / ') + ' mm' : '—'],
      ['layers', f.layers.size ? [...f.layers].join(', ') : (f.traces ? '' : 'unrouted')],
    ]);
  wireNetLinks(el, onNet);
}

function renderPart(el, sel, state, part, onNet, onScript) {
  const fp = findFootprint(state, sel.ref);
  const lcsc = (fp && fp.lcsc_id) || (part && part.lcsc_id) || '';
  const ds = part && part.datasheet;
  el.innerHTML =
    `<h3>${esc(sel.ref)}</h3>` +
    `<p class="sub">${esc([(fp && fp.value) || '', (fp && fp.key) || ''].filter(Boolean).join(' · '))}</p>` +
    kv([
      ['key', esc((fp && fp.key) || '')],
      ['value', esc((fp && fp.value) || '')],
      ['description', esc((fp && fp.description) || (part && part.description) || '')],
      ['position', fp ? `${fmt(fp.position.x / 1e6)}, ${fmt(fp.position.y / 1e6)} mm` : ''],
      ['rotation', fp ? `${fmt(fp.rotation, 0)}°` : ''],
      ['side', fp ? layerName(state, fp.layer) : ''],
      ['pads', fp ? (fp.pads || []).length : ''],
      ['LCSC', lcsc ? `<a href="https://www.lcsc.com/product-detail/${esc(lcsc)}.html" target="_blank" rel="noreferrer">${esc(lcsc)}</a>` : ''],
      ['MPN', esc((fp && fp.mpn) || (part && part.mpn) || '')],
      ['maker', esc((fp && fp.manufacturer) || (part && part.manufacturer) || '')],
      ['datasheet', ds ? `<a href="${esc(ds)}" target="_blank" rel="noreferrer">PDF</a>` : ''],
    ]) +
    pinTable(fp) +
    `<div class="actions">
       <button class="ghost small" data-act="rotate">rotate 90°</button>
       <button class="ghost small" data-act="place-legal">place-legal</button>
       <button class="ghost small" data-act="unplace">unplace</button>
     </div>`;
  wireNetLinks(el, onNet);
  el.querySelectorAll('[data-act]').forEach((b) => {
    b.onclick = () => {
      const act = b.dataset.act;
      if (act === 'rotate') {
        const rot = fp ? (Number(fp.rotation) + 90) % 360 : 90;
        onScript(`rotate ${sel.ref} ${rot}`);
      } else {
        onScript(`${act} ${sel.ref}`);
      }
    };
  });
}

function pinTable(fp) {
  if (!fp || !(fp.pads || []).length) return '';
  const rows = fp.pads
    .map(
      (p) =>
        `<tr data-net="${esc(p.net || '')}"><td>${esc(p.number)}</td><td>${esc(p.name || '')}</td>` +
        `<td class="net">${esc(p.net || '—')}</td></tr>`
    )
    .join('');
  return `<div class="pins"><table><thead><tr><th>pad</th><th>name</th><th>net</th></tr></thead><tbody>${rows}</tbody></table></div>`;
}

function findFootprint(state, ref) {
  const fps = ((state || {}).board || {}).footprints || {};
  for (const id of Object.keys(fps)) if (fps[id] && fps[id].reference === ref) return fps[id];
  return null;
}

function wireNetLinks(el, onNet) {
  if (!onNet) return;
  el.querySelectorAll('[data-net]').forEach((n) => {
    if (!n.dataset.net) return;
    n.addEventListener('click', (ev) => {
      ev.preventDefault();
      onNet(n.dataset.net);
    });
  });
}

// ── checks ──────────────────────────────────────────────────────

export function renderChecks(el, reports, opts) {
  const all = [];
  for (const rep of reports) if (rep) all.push(...rep.violations);
  const list = opts.errorsOnly ? all.filter((v) => v.severity === 'error') : all;
  if (!reports.some(Boolean)) {
    el.innerHTML = '<p class="empty">No check has run yet.</p>';
    return;
  }
  if (!list.length) {
    el.innerHTML = `<p class="empty">${opts.errorsOnly ? 'No errors.' : 'Clean — no findings.'}</p>`;
    return;
  }
  el.replaceChildren(
    ...list.map((v) => {
      const b = document.createElement('button');
      b.className = 'viol' + (v.severity === 'error' ? ' is-error' : '');
      b.type = 'button';
      b.innerHTML =
        `<span class="kind">${esc(v.source)} · ${esc(v.kind)}</span>` +
        `<span class="msg">${esc(v.message)}</span>` +
        (v.x_mm != null ? `<span class="at">at ${fmt(v.x_mm)}, ${fmt(v.y_mm)} mm</span>` : '');
      b.onclick = () => opts.onPick(v);
      return b;
    })
  );
}

// ── log ─────────────────────────────────────────────────────────

export class Log {
  constructor(el) {
    this.el = el;
    this.issued = [];
  }

  line(text, cls) {
    const atBottom = this.el.scrollHeight - this.el.scrollTop - this.el.clientHeight < 24;
    const div = document.createElement('div');
    div.className = cls || '';
    div.textContent = text;
    this.el.appendChild(div);
    while (this.el.childElementCount > 600) this.el.firstElementChild.remove();
    if (atBottom) this.el.scrollTop = this.el.scrollHeight;
  }

  sys(text) { this.line(text, 'l-sys'); }

  // command echoes the script the UI is about to run, so the human learns
  // the verbs by watching their own edits become script lines.
  command(text) {
    for (const l of text.split('\n')) if (l.trim()) this.line('> ' + l, 'l-cmd');
    this.issued.push(text);
  }

  results(text) {
    for (const l of text.split('\n')) {
      if (!l.trim()) continue;
      this.line(l, /^error\b/.test(l) ? 'l-err' : /^ok\b/.test(l) ? 'l-ok' : '');
    }
  }

  clear() { this.el.replaceChildren(); }
}

// ── status bar ──────────────────────────────────────────────────

export function renderStatus(ids, summary, checks) {
  const s = summary || {};
  ids.outline.textContent = s.width_mm ? `${fmt(s.width_mm, 1)} × ${fmt(s.height_mm, 1)} mm` : 'no outline';
  ids.layers.textContent = `${(s.layers || []).length} layers · ${(s.layers || []).join('/')}`;
  ids.parts.textContent = `${s.parts || 0} parts · ${s.symbols || 0} symbols`;
  const routed = `${s.nets_routed || 0}/${s.nets || 0} nets routed`;
  ids.nets.textContent = s.unrouted ? `${routed} · ${s.unrouted} open` : routed;
  ids.nets.className = 'stat' + (s.nets && !s.unrouted ? ' is-ok' : '');
  if (!checks || (!checks.drc && !checks.erc)) {
    ids.drc.textContent = 'drc not run';
    ids.drc.className = 'stat';
    return;
  }
  const errs = (checks.drc ? checks.drc.errors : 0) + (checks.erc ? checks.erc.errors : 0);
  const warns = (checks.drc ? checks.drc.warnings : 0) + (checks.erc ? checks.erc.warnings : 0);
  ids.drc.textContent = `${errs} errors · ${warns} warnings`;
  ids.drc.className = 'stat ' + (errs ? 'is-error' : warns ? 'is-warn' : 'is-ok');
}
