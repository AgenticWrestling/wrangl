// wrangl PWA glue: sheets, drag-and-drop reordering, the read-only terminal, web push.
// Live page updates are Datastar's job (data-on-load @get /ui/live in the layout).
'use strict';

function openSheet(id) {
  const d = document.getElementById(id);
  if (d && !d.open) d.showModal();
}
window.openSheet = openSheet;

// A sheet's submit button carries data-close: Datastar posts the signals, then the sheet
// closes (deferred so Datastar's own click handler reads the form first).
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-close]');
  if (!b) return;
  const d = b.closest('dialog');
  if (d) setTimeout(() => d.close(), 0);
});

function toast(msg, err) {
  const t = document.getElementById('toast');
  if (!t) return;
  t.textContent = msg;
  t.className = 'toast show' + (err ? ' err' : '');
}

// The server's toasts arrive as morphs of #toast; hide them again after a few seconds.
let toastTimer;
new MutationObserver(() => {
  const t = document.getElementById('toast');
  if (!t || !t.classList.contains('show')) return;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), 3500);
}).observe(document.documentElement, { subtree: true, childList: true, attributes: true, attributeFilter: ['class'] });

// --- drag and drop -----------------------------------------------------------------------

let dragging = false;

async function moveTask(evt) {
  const li = evt.item;
  const sqid = li.dataset.sqid;
  const parent = evt.to.dataset.parent || '';
  let prev = li.previousElementSibling;
  while (prev && !prev.dataset.sqid) prev = prev.previousElementSibling;
  const body = {};
  if (parent === '') body.root = true; else body.parent = parent;
  if (prev) body.after = prev.dataset.sqid; else body.first = true;
  try {
    const r = await fetch('/api/v1/tasks/' + encodeURIComponent(sqid) + '/move', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const e = await r.json().catch(() => ({}));
      toast('move failed: ' + (e.error || r.status), true);
      location.reload();
      return;
    }
    toast('moved ' + sqid);
  } catch (e) {
    toast('move failed: ' + e, true);
  }
}

function initSortables() {
  if (!window.Sortable) return;
  document.querySelectorAll('ul.sortable').forEach((el) => {
    if (Sortable.get(el)) return;
    Sortable.create(el, {
      group: 'tasks', handle: '.handle', animation: 150, forceFallback: true, fallbackOnBody: true,
      swapThreshold: 0.65, emptyInsertThreshold: 8, delay: 120, delayOnTouchOnly: true,
      onStart: () => { dragging = true; },
      onEnd: (evt) => {
        dragging = false;
        if (evt.from === evt.to && evt.oldIndex === evt.newIndex) return;
        moveTask(evt);
      },
    });
  });
}

// --- terminal ----------------------------------------------------------------------------

function b64bytes(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function initTerminal() {
  const el = document.getElementById('term');
  if (!el || el.dataset.ready || !window.Terminal) return;
  el.dataset.ready = '1';
  const term = new Terminal({
    convertEol: false, disableStdin: true, cursorBlink: false, fontSize: 12, scrollback: 5000,
    // The PWA's 16-colour palette: ANSI slots get the Okabe–Ito accents.
    theme: {
      background: '#0f1115', foreground: '#eef0f3', cursor: '#56b4e9', selectionBackground: '#2c313c',
      black: '#1f232b', red: '#d55e00', green: '#009e73', yellow: '#f0e442',
      blue: '#0072b2', magenta: '#cc79a7', cyan: '#56b4e9', white: '#c5cad3',
      brightBlack: '#4b5261', brightRed: '#e69f00', brightGreen: '#3fc39b', brightYellow: '#f5ec7a',
      brightBlue: '#a6d8f5', brightMagenta: '#e3a6c8', brightCyan: '#a6d8f5', brightWhite: '#eef0f3',
    },
  });
  const fit = window.FitAddon ? new FitAddon.FitAddon() : null;
  if (fit) term.loadAddon(fit);
  term.open(el);
  if (fit) { fit.fit(); addEventListener('resize', () => fit.fit()); }
  const state = document.getElementById('term-state');
  const es = new EventSource('/api/v1/runs/' + el.dataset.run + '/pty?format=sse', { withCredentials: true });
  es.addEventListener('pty', (e) => term.write(b64bytes(e.data)));
  es.addEventListener('end', () => { if (state) state.textContent = 'ended'; es.close(); });
  es.onopen = () => { if (state) state.textContent = 'live'; };
  es.onerror = () => { if (state && es.readyState !== EventSource.CLOSED) state.textContent = 'reconnecting…'; };
}

// --- web push ----------------------------------------------------------------------------

function urlB64ToBytes(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  return b64bytes((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
}

async function enablePush() {
  const st = document.getElementById('push-state');
  const say = (m) => { if (st) st.textContent = m; };
  try {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) { say('This browser has no web push (on iOS, install to the Home Screen first).'); return; }
    if ((await Notification.requestPermission()) !== 'granted') { say('Notifications were not allowed.'); return; }
    const reg = await navigator.serviceWorker.ready;
    const { public_key } = await (await fetch('/api/v1/push/key', { credentials: 'same-origin' })).json();
    let sub = await reg.pushManager.getSubscription();
    if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: urlB64ToBytes(public_key) });
    const r = await fetch('/api/v1/push/subscribe', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify(sub),
    });
    say(r.ok ? 'Push is on for this device.' : 'Subscribe failed: HTTP ' + r.status);
  } catch (e) {
    say('Push failed: ' + e);
  }
}
window.enablePush = enablePush;

async function pushState() {
  const st = document.getElementById('push-state');
  if (!st || !('serviceWorker' in navigator)) return;
  const reg = await navigator.serviceWorker.getRegistration();
  const sub = reg && reg.pushManager ? await reg.pushManager.getSubscription() : null;
  st.textContent = sub ? 'Push is on for this device.' : 'Push is off for this device.';
}

// --- boot --------------------------------------------------------------------------------

function boot() {
  initSortables();
  initTerminal();
  pushState().catch(() => {});
  if (location.hash.length > 1) openSheet('sheet-' + location.hash.slice(1));
  const main = document.getElementById('main');
  if (main) {
    let t;
    new MutationObserver(() => { clearTimeout(t); t = setTimeout(initSortables, 50); })
      .observe(main, { childList: true, subtree: true });
  }
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => {});
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
