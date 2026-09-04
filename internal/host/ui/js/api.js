// Thin wrapper over the fragua HTTP API. Same endpoints an agent uses.

export async function runScript(body) {
  const r = await fetch('/script', {
    method: 'POST',
    headers: { 'Content-Type': 'text/plain' },
    body,
  });
  const text = await r.text();
  return { ok: r.ok, text: text.trimEnd() };
}

export async function getJSON(path) {
  const r = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`${path}: HTTP ${r.status}`);
  return r.json();
}

export async function getSVG(path) {
  const r = await fetch(path, { cache: 'no-store' });
  if (!r.ok) throw new Error(`${path}: HTTP ${r.status}`);
  return r.text();
}

export async function cancelOp() {
  const r = await fetch('/cancel', { method: 'POST' });
  return r.json();
}

export async function save(path) {
  const r = await fetch('/save', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(path ? { path } : {}),
  });
  return { ok: r.ok, text: (await r.text()).trim() };
}

// connectEvents keeps an SSE subscription alive and hands every event to
// onEvent. The browser reconnects EventSource itself; onState reports the
// swing so the header dot can tell the truth.
export function connectEvents(onEvent, onState) {
  let es;
  const open = () => {
    es = new EventSource('/events');
    es.onopen = () => onState('live');
    es.onerror = () => onState('down');
    es.onmessage = (ev) => {
      try {
        onEvent(JSON.parse(ev.data));
      } catch (_) {
        /* a malformed frame is not worth breaking the stream over */
      }
    };
  };
  open();
  return () => es && es.close();
}
