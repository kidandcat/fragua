// Inline-SVG board canvas: pan, zoom, layer visibility, net highlight,
// selection and drag-to-move. The SVG is injected (not an <img>) so every
// element the renderer tagged with data-* is addressable from here.

const MIN_K = 0.12;
const MAX_K = 400;
const DRAG_SLOP_PX = 3;
const SNAP_MM = 0.1;

const parser = new DOMParser();

function parseTranslateRotate(t) {
  const m = /translate\(\s*(-?[\d.]+)[ ,]+(-?[\d.]+)\s*\)/.exec(t || '');
  const r = /rotate\(\s*(-?[\d.]+)/.exec(t || '');
  return {
    x: m ? parseFloat(m[1]) : 0,
    y: m ? parseFloat(m[2]) : 0,
    rot: r ? parseFloat(r[1]) : 0,
  };
}

const snap = (v) => Math.round(v / SNAP_MM) * SNAP_MM;
const cssq = (s) => (window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, '\\$&'));

export class Canvas {
  constructor(host, hooks = {}) {
    this.host = host;
    this.hooks = hooks;
    this.svg = null;
    this.vp = null;
    this.root = null;
    this.vb = { x: 0, y: 0, w: 100, h: 100 };
    this.view = { k: 1, tx: 0, ty: 0 };
    this.flip = false;
    this.layers = new Map(); // name -> visible
    this.hlNet = null;
    this.selection = null;
    this.markers = [];
    this._bindPointer();
  }

  // ── document ──────────────────────────────────────────────────

  setSVG(text) {
    const doc = parser.parseFromString(text, 'image/svg+xml');
    const svg = doc.documentElement;
    if (!svg || svg.nodeName === 'parsererror') return false;
    svg.removeAttribute('width');
    svg.removeAttribute('height');
    const imported = document.importNode(svg, true);
    const root = imported.querySelector('[data-root]');
    if (!root) return false;
    const vp = document.createElementNS('http://www.w3.org/2000/svg', 'g');
    vp.setAttribute('class', 'vp');
    root.parentNode.insertBefore(vp, root);
    vp.appendChild(root);

    this.host.replaceChildren(imported);
    this.svg = imported;
    this.vp = vp;
    this.root = root;
    const b = imported.viewBox.baseVal;
    this.vb = { x: b.x, y: b.y, w: b.width, h: b.height };

    this.knownLayers().forEach((n) => {
      if (!this.layers.has(n)) this.layers.set(n, defaultVisible(n));
    });
    this.applyLayers();
    this.applyFlip();
    this.applyTransform();
    this.renderMarkers();
    this.applyHighlight();
    this.applySelection();
    return true;
  }

  knownLayers() {
    if (!this.svg) return [];
    const out = [];
    this.svg.querySelectorAll('g[data-layer]').forEach((g) => {
      const n = g.dataset.layer;
      if (n && !out.includes(n)) out.push(n);
    });
    return out;
  }

  // copperLayers lists the stack top-down (F.Cu first); the document draws
  // it bottom-up, which is the wrong order to read a stackup in.
  copperLayers() {
    if (!this.svg) return [];
    return [...this.svg.querySelectorAll('g[data-kind="copper"]')]
      .sort((a, b) => Number(a.dataset.index) - Number(b.dataset.index))
      .map((g) => g.dataset.layer);
  }

  toSVGText() {
    if (!this.svg) return '';
    const copy = this.svg.cloneNode(true);
    const vp = copy.querySelector('g.vp');
    if (vp) vp.removeAttribute('transform');
    return new XMLSerializer().serializeToString(copy);
  }

  // ── view ──────────────────────────────────────────────────────

  applyTransform() {
    if (!this.vp) return;
    const { k, tx, ty } = this.view;
    let t = `translate(${tx.toFixed(4)},${ty.toFixed(4)}) scale(${k.toFixed(5)})`;
    if (this.flip) t += ` translate(${(2 * this.centre().x).toFixed(4)},0) scale(-1,1)`;
    this.vp.setAttribute('transform', t);
    this.scaleMarkers();
    if (this.hooks.onZoom) this.hooks.onZoom(k);
  }

  centre() {
    return { x: this.vb.x + this.vb.w / 2, y: this.vb.y + this.vb.h / 2 };
  }

  fit() {
    this.view = { k: 1, tx: 0, ty: 0 };
    this.applyTransform();
  }

  // zoomAt scales about a client point so the board does not slide out from
  // under the cursor.
  zoomAt(factor, clientX, clientY) {
    const k = Math.min(MAX_K, Math.max(MIN_K, this.view.k * factor));
    const f = k / this.view.k;
    if (f === 1) return;
    const v = this.clientToViewBox(clientX, clientY);
    this.view.tx = v.x * (1 - f) + f * this.view.tx;
    this.view.ty = v.y * (1 - f) + f * this.view.ty;
    this.view.k = k;
    this.applyTransform();
  }

  zoomBy(factor) {
    const r = this.host.getBoundingClientRect();
    this.zoomAt(factor, r.left + r.width / 2, r.top + r.height / 2);
  }

  clientToViewBox(cx, cy) {
    const m = this.svg.getScreenCTM();
    if (!m) return { x: 0, y: 0 };
    const p = this.svg.createSVGPoint();
    p.x = cx;
    p.y = cy;
    return p.matrixTransform(m.inverse());
  }

  // boardToViewBox maps a board point in mm to viewBox coordinates before the
  // pan/zoom transform (the renderer's root group flips Y).
  boardToViewBox(xmm, ymm) {
    const x = this.flip ? 2 * this.centre().x - xmm : xmm;
    return { x, y: -ymm };
  }

  focusPoint(xmm, ymm, k) {
    const target = k || Math.max(this.view.k, Math.min(MAX_K, 60 / Math.max(this.vb.w, 1) * 6));
    this.view.k = Math.min(MAX_K, Math.max(MIN_K, target));
    const c = this.centre();
    const b = this.boardToViewBox(xmm, ymm);
    this.view.tx = c.x - this.view.k * b.x;
    this.view.ty = c.y - this.view.k * b.y;
    this.applyTransform();
  }

  setFlip(on) {
    this.flip = !!on;
    this.applyFlip();
    this.applyTransform();
  }

  applyFlip() {
    if (this.svg) this.svg.setAttribute('data-flip', this.flip ? '1' : '0');
  }

  // ── layers ────────────────────────────────────────────────────

  setLayer(name, visible) {
    this.layers.set(name, visible);
    this.applyLayers();
  }

  isLayerVisible(name) {
    return this.layers.get(name) !== false;
  }

  applyLayers() {
    if (!this.svg) return;
    for (const [name, visible] of this.layers) {
      const sel = `[data-layer="${cssq(name)}"]`;
      this.svg.querySelectorAll(sel).forEach((el) => {
        // A drilled pad is copper on every layer: hiding F.Cu must not make
        // a through-hole pin disappear.
        if (!visible && el.hasAttribute('data-through')) return;
        el.style.display = visible ? '' : 'none';
      });
    }
  }

  // ── markers ───────────────────────────────────────────────────

  setMarkers(violations) {
    this.markers = (violations || []).filter((v) => v.x_mm != null && v.y_mm != null);
    this.renderMarkers();
  }

  renderMarkers() {
    if (!this.svg) return;
    const g = this.svg.querySelector('g[data-layer="drc"]');
    if (!g) return;
    g.replaceChildren();
    g.setAttribute('pointer-events', 'none');
    for (const m of this.markers) {
      // Errors read loud, warnings stay out of the way: a clean board with
      // a hundred conservative warnings must not look like a crime scene.
      const err = m.severity === 'error';
      const stroke = err ? '#ff6b6b' : '#e3b341';
      const r = err ? 0.9 : 0.6;
      const el = document.createElementNS('http://www.w3.org/2000/svg', 'g');
      el.setAttribute('data-marker', m.id);
      el.setAttribute('data-severity', m.severity);
      el.setAttribute('opacity', err ? '1' : '0.5');
      el.dataset.x = m.x_mm;
      el.dataset.y = m.y_mm;
      el.innerHTML =
        `<circle r="${r}" fill="none" stroke="${stroke}" stroke-width="0.14"/>` +
        `<line x1="${-r - 0.5}" y1="0" x2="${r + 0.5}" y2="0" stroke="${stroke}" stroke-width="0.1"/>` +
        `<line x1="0" y1="${-r - 0.5}" x2="0" y2="${r + 0.5}" stroke="${stroke}" stroke-width="0.1"/>`;
      g.appendChild(el);
    }
    this.scaleMarkers();
  }

  // scaleMarkers keeps a violation marker the same size on screen at every
  // zoom. Left in board millimetres, a crosshair at 600% swallows the pad it
  // is pointing at.
  scaleMarkers() {
    if (!this.svg) return;
    const s = Math.min(3, Math.max(0.2, 1 / this.view.k));
    this.svg.querySelectorAll('g[data-marker]').forEach((el) => {
      el.setAttribute('transform', `translate(${el.dataset.x},${el.dataset.y}) scale(${s.toFixed(4)})`);
    });
  }

  focusMarker(m) {
    if (m.x_mm == null || m.y_mm == null) return;
    this.focusPoint(m.x_mm, m.y_mm, Math.max(this.view.k, 6));
    const el = this.svg && this.svg.querySelector(`g[data-marker="${cssq(m.id)}"]`);
    if (el) {
      el.classList.remove('is-focus');
      void el.getBoundingClientRect();
      el.classList.add('is-focus');
    }
  }

  // ── highlight + selection ─────────────────────────────────────

  highlightNet(net) {
    this.hlNet = net || null;
    this.applyHighlight();
  }

  applyHighlight() {
    if (!this.svg) return;
    this.svg.querySelectorAll('.hl').forEach((el) => el.classList.remove('hl'));
    if (!this.hlNet) {
      this.svg.classList.remove('has-hl');
      return;
    }
    this.svg.classList.add('has-hl');
    this.svg
      .querySelectorAll(`[data-net="${cssq(this.hlNet)}"]`)
      .forEach((el) => el.classList.add('hl'));
  }

  select(sel) {
    this.selection = sel;
    this.applySelection();
    if (this.hooks.onSelect) this.hooks.onSelect(sel);
  }

  applySelection() {
    if (!this.svg) return;
    this.svg.querySelectorAll('.sel').forEach((el) => el.classList.remove('sel'));
    const ref = this.selection && this.selection.ref;
    if (!ref) return;
    const g = this.svg.querySelector(`g[data-ref="${cssq(ref)}"]`);
    if (g) g.classList.add('sel');
  }

  selectedFootprint() {
    if (!this.svg || !this.selection || !this.selection.ref) return null;
    return this.svg.querySelector(`g[data-ref="${cssq(this.selection.ref)}"]`);
  }

  // footprintPose reads the live position/rotation off the SVG, so the
  // steering verbs are always built from what the human can see.
  footprintPose(ref) {
    const g = this.svg && this.svg.querySelector(`g[data-ref="${cssq(ref)}"]`);
    return g ? parseTranslateRotate(g.getAttribute('transform')) : null;
  }

  // ── picking ───────────────────────────────────────────────────

  pick(target) {
    if (!target || !target.closest) return null;
    const el = target.closest('[data-marker], [data-pad], [data-net], [data-ref]');
    if (!el || !this.svg.contains(el)) return null;
    const layerOf = (n) => {
      const g = n.closest('g[data-layer]');
      return g ? g.dataset.layer : '';
    };
    if (el.hasAttribute('data-marker')) {
      return { kind: 'marker', id: el.dataset.marker, el };
    }
    if (el.hasAttribute('data-pad')) {
      const fp = el.closest('g[data-ref]');
      return {
        kind: 'pad',
        ref: fp ? fp.dataset.ref : '',
        pad: el.dataset.pad,
        padName: el.dataset.padName || el.dataset.pad,
        net: el.dataset.net || '',
        layer: el.dataset.layer || '',
        el,
      };
    }
    if (el.hasAttribute('data-ref')) {
      return { kind: 'footprint', ref: el.dataset.ref, el };
    }
    const tag = el.tagName.toLowerCase();
    const kind = tag === 'line' ? 'trace' : tag === 'circle' ? 'via' : 'copper';
    return { kind, net: el.dataset.net || '', id: el.dataset.id || '', layer: layerOf(el), el };
  }

  // ── pointer ───────────────────────────────────────────────────

  _bindPointer() {
    const host = this.host;
    let mode = null; // pan | drag
    let start = null;
    let drag = null;

    host.addEventListener('wheel', (ev) => {
      if (!this.svg) return;
      ev.preventDefault();
      this.zoomAt(Math.exp(-ev.deltaY * 0.0016), ev.clientX, ev.clientY);
    }, { passive: false });

    host.addEventListener('pointerdown', (ev) => {
      if (!this.svg || ev.button !== 0) return;
      host.focus({ preventScroll: true });
      const hit = this.pick(ev.target);
      start = { x: ev.clientX, y: ev.clientY, tx: this.view.tx, ty: this.view.ty, moved: false };
      if (hit && hit.kind === 'footprint' && !ev.shiftKey) {
        const pose = this.footprintPose(hit.ref);
        drag = { ref: hit.ref, el: hit.el, pose, mm: this._mmPerPixel() };
        mode = 'drag';
        this.select({ kind: 'footprint', ref: hit.ref });
      } else {
        mode = 'pan';
        host.classList.add('is-panning');
      }
      try {
        host.setPointerCapture(ev.pointerId);
      } catch (_) {
        // A pointer that is already gone (or synthesised) cannot be
        // captured; the drag still works off the bubbled events.
      }
    });

    host.addEventListener('pointermove', (ev) => {
      if (!this.svg) return;
      if (!mode) {
        const hit = this.pick(ev.target);
        this.highlightNet(hit && hit.net ? hit.net : null);
        if (this.hooks.onHover) this.hooks.onHover(hit, ev);
        return;
      }
      const dx = ev.clientX - start.x;
      const dy = ev.clientY - start.y;
      if (!start.moved && Math.hypot(dx, dy) < DRAG_SLOP_PX) return;
      start.moved = true;
      if (mode === 'pan') {
        const s = this._viewBoxPerPixel();
        this.view.tx = start.tx + dx * s.x;
        this.view.ty = start.ty + dy * s.y;
        this.applyTransform();
        return;
      }
      host.classList.add('is-dragging');
      const nx = drag.pose.x + dx * drag.mm.x;
      const ny = drag.pose.y + dy * drag.mm.y;
      drag.el.setAttribute('transform', `translate(${nx},${ny}) rotate(${drag.pose.rot})`);
      drag.last = { x: nx, y: ny };
    });

    const endPointer = (ev) => {
      if (mode === 'drag' && start && start.moved && drag && drag.last) {
        const x = snap(drag.last.x);
        const y = snap(drag.last.y);
        if (this.hooks.onMove) this.hooks.onMove(drag.ref, x, y);
      } else if (mode === 'pan' && start && !start.moved) {
        const hit = this.pick(ev.target);
        if (hit && hit.kind === 'marker') {
          if (this.hooks.onMarker) this.hooks.onMarker(hit.id);
        } else if (hit) {
          this.select(hit);
        } else {
          this.select(null);
        }
      }
      mode = null;
      start = null;
      drag = null;
      host.classList.remove('is-panning', 'is-dragging');
    };
    host.addEventListener('pointerup', endPointer);
    host.addEventListener('pointercancel', endPointer);
    host.addEventListener('pointerleave', () => {
      if (!mode && this.hooks.onHover) this.hooks.onHover(null);
    });
  }

  // _viewBoxPerPixel is how many viewBox units one screen pixel is worth.
  _viewBoxPerPixel() {
    const m = this.svg.getScreenCTM();
    if (!m) return { x: 1, y: 1 };
    return { x: 1 / m.a, y: 1 / m.d };
  }

  // _mmPerPixel is how many board millimetres one screen pixel is worth,
  // sign included: the renderer's root flips Y, and bottom view flips X.
  _mmPerPixel() {
    const m = this.root.getScreenCTM();
    if (!m) return { x: 1, y: -1 };
    return { x: 1 / m.a, y: 1 / m.d };
  }
}

// defaultVisible: the mask is a manufacturing overlay, off until asked for.
function defaultVisible(name) {
  return name !== 'mask';
}
