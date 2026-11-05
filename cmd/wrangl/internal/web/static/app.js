// wrangl PWA glue: sheets, drag-and-drop reordering, the read-only terminal, web push.
// Live page updates are Datastar's job (data-init @get /ui/live in the layout).
'use strict';

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

// postMove asks the server to move a task: body is {root|parent, first|after}. Shared by drag
// and drop (moveTask) and the keyboard reorder (keys.js).
async function postMove(sqid, body) {
  try {
    const r = await fetch('/api/v1/tasks/' + encodeURIComponent(sqid) + '/move', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const e = await r.json().catch(() => ({}));
      toast('move failed: ' + (e.error || r.status), true);
      return false;
    }
    toast('moved ' + sqid);
    return true;
  } catch (e) {
    toast('move failed: ' + e, true);
    return false;
  }
}
window.postMove = postMove;

async function moveTask(evt) {
  const li = evt.item;
  const parent = evt.to.dataset.parent || '';
  let prev = li.previousElementSibling;
  while (prev && !prev.dataset.sqid) prev = prev.previousElementSibling;
  const body = {};
  if (parent === '') body.root = true; else body.parent = parent;
  if (prev) body.after = prev.dataset.sqid; else body.first = true;
  if (!(await postMove(li.dataset.sqid, body))) location.reload();
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

// The Queue list reorders runs only (queue_rank); it has its own group so nothing can be dropped
// into the backlog, and it never calls the task move endpoint.
function initQueueSort() {
  if (!window.Sortable) return;
  document.querySelectorAll('ul.queue-sort').forEach((el) => {
    if (Sortable.get(el)) return;
    Sortable.create(el, {
      group: 'queue', handle: '.handle', animation: 150, forceFallback: true, fallbackOnBody: true,
      delay: 120, delayOnTouchOnly: true,
      onStart: () => { dragging = true; },
      onEnd: async (evt) => {
        dragging = false;
        if (evt.oldIndex === evt.newIndex) return;
        const runs = [...el.children].map((li) => Number(li.dataset.run)).filter(Boolean);
        try {
          const r = await fetch('/api/v1/runs/queue-order', {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
            body: JSON.stringify({ runs }),
          });
          if (!r.ok) throw new Error(r.status);
          toast('queue reordered');
        } catch (e) {
          toast('reorder failed: ' + e, true);
          location.reload();
        }
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

// --- inline task details -------------------------------------------------------------------
// Clicking a task's title expands its full details and controls under the row (one at a time).
// The page tells the server which task is open (/ui/expand), so every live re-render of the list
// includes that task's card and cannot collapse it. The edit sheets (dialogs) live outside #main,
// in #detail-sheets, so a re-render cannot close one while you type.

let openDetail = null;

function pageSid() { const m = document.querySelector('meta[name="wr-sid"]'); return m ? m.content : ''; }
function detailBox(sqid) { return document.getElementById('d-' + sqid); }
function sheetsBox() { return document.getElementById('detail-sheets'); }

function postExpand(sqid) {
  const sid = pageSid();
  if (!sid) return;
  fetch('/ui/expand?sid=' + encodeURIComponent(sid) + '&sqid=' + encodeURIComponent(sqid || ''), { method: 'POST', credentials: 'same-origin' }).catch(() => {});
}

// --- live agent output in an expanded task card ------------------------------------------------
// One read-only wterm at a time (the card that is open), connected only while open. Replays the
// last 64 KB of the run's own PTY (the session's, not the runner's), then follows live; EventSource
// resumes by Last-Event-ID on a dropped connection. "load full history" reconnects without the
// tail limit. The card is data-ignore-morph, so live re-renders leave the terminal alone.
let liveTerm = null;

function stopLiveTerm() {
  if (!liveTerm) return;
  liveTerm.es.close();
  try { liveTerm.t.destroy(); } catch (_) { /* already gone */ }
  liveTerm = null;
}

async function startLiveTerm() {
  stopLiveTerm();
  const box = document.querySelector('.detail .liveterm');
  if (!box || !window.wterm || box.dataset.ready) return;
  box.dataset.ready = '1';
  const run = box.dataset.run, body = box.querySelector('.ltbody');
  const state = box.querySelector('.ltstate'), full = box.querySelector('.ltfull');
  const t = new window.wterm.WTerm(body, { cols: 100, rows: 28, autoResize: false, cursorBlink: false });
  await t.init();
  const cur = { t, es: null };
  if (!box.isConnected) { t.destroy(); return; }
  liveTerm = cur;
  const open = (all) => {
    if (cur.es) cur.es.close();
    if (all) t.write('\x1bc'); // reset, then replay everything
    const es = new EventSource('/api/v1/runs/' + run + '/pty?format=sse' + (all ? '&full=1' : '&tail=64'), { withCredentials: true });
    cur.es = es;
    es.addEventListener('tail', () => { full.hidden = false; });
    es.addEventListener('pty', (e) => t.write(b64bytes(e.data)));
    es.addEventListener('end', () => { state.textContent = 'run finished'; es.close(); });
    es.onopen = () => { state.textContent = 'live'; };
    es.onerror = () => { if (es.readyState !== EventSource.CLOSED) state.textContent = 'reconnecting…'; else state.textContent = 'unavailable'; };
  };
  full.addEventListener('click', (e) => { e.preventDefault(); full.hidden = true; open(true); });
  open(false);
}

// Transcript panel (Live output as Markdown): follow the tail while the reader is at the bottom,
// stop when they scroll up; "load earlier" prepends older entries without moving the view.
function startLiveTx() {
  const box = document.querySelector('.detail .livetx');
  if (!box || box.dataset.ready) return;
  box.dataset.ready = '1';
  const body = box.querySelector('.txbody'), list = box.querySelector('[id^="txl-"]');
  let follow = true;
  const atBottom = () => body.scrollHeight - body.scrollTop - body.clientHeight < 24;
  body.addEventListener('scroll', () => { follow = atBottom(); });
  new MutationObserver(() => { if (follow) body.scrollTop = body.scrollHeight; }).observe(list, { childList: true });
  body.scrollTop = body.scrollHeight;
  box.addEventListener('click', async (e) => {
    const a = e.target.closest('.txearlier');
    if (!a) return;
    e.preventDefault();
    const r = await fetch('/ui/transcript?run=' + box.dataset.run + '&before=' + a.dataset.before, { credentials: 'same-origin' });
    if (!r.ok) return;
    const h = body.scrollHeight - body.scrollTop;
    a.remove();
    list.insertAdjacentHTML('afterbegin', await r.text());
    body.scrollTop = body.scrollHeight - h;
  });
}

function closeDetails() {
  stopLiveTerm();
  const sh = sheetsBox();
  if (sh) { sh.querySelectorAll('dialog[open]').forEach((x) => x.close()); sh.innerHTML = ''; }
  document.querySelectorAll('.detail').forEach((d) => {
    d.innerHTML = '';
    const a = d.parentElement && d.parentElement.querySelector(':scope > .row .title');
    if (a) a.setAttribute('aria-expanded', 'false');
  });
}

async function loadDetail(sqid) {
  const box = detailBox(sqid);
  if (!box) { openDetail = null; return; }
  try {
    const q = '/ui/detail?sqid=' + encodeURIComponent(sqid);
    const [card, sheets] = await Promise.all([
      fetch(q, { credentials: 'same-origin' }).then((r) => { if (!r.ok) throw new Error(String(r.status)); return r.text(); }),
      fetch(q + '&part=sheets', { credentials: 'same-origin' }).then((r) => (r.ok ? r.text() : '')),
    ]);
    if (openDetail !== sqid) return; // collapsed or switched meanwhile
    if (!box.firstElementChild) box.innerHTML = card; // the live stream may already have filled it
    const sh = sheetsBox();
    if (sh) sh.innerHTML = sheets;
    const a = box.parentElement.querySelector(':scope > .row .title');
    if (a) a.setAttribute('aria-expanded', 'true');
    startLiveTerm();
    startLiveTx();
  } catch (_) {
    box.innerHTML = '';
    openDetail = null;
    postExpand('');
  }
}

function toggleDetail(sqid, ev) {
  if (ev && (ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.button > 0)) return true; // open the page
  if (ev) ev.preventDefault();
  const same = openDetail === sqid;
  closeDetails();
  openDetail = same ? null : sqid;
  postExpand(openDetail || '');
  if (!same) loadDetail(sqid);
  return false;
}
window.toggleDetail = toggleDetail;

// If the stream reconnected (a new server-side session) the open card is gone: tell the server again.
function checkDetail() {
  if (!openDetail) return;
  const box = detailBox(openDetail);
  if (!box) { openDetail = null; return; }
  if (!box.firstElementChild) { postExpand(openDetail); }
}

// --- text size (Settings) ---------------------------------------------------------------------
// Stored per device in localStorage and applied as --fs on <html> (an inline script in the head
// applies it before first paint). The settings page is re-rendered by the live stream, so the
// selected state is re-applied after each morph.

function currentFs() {
  try { const f = parseFloat(localStorage.getItem('wr.fs')); if (f >= 0.7 && f <= 2) return f; } catch (_) { /* private mode */ }
  return 1;
}

function markFs() {
  const cur = currentFs();
  document.querySelectorAll('#fs-seg button').forEach((b) => b.setAttribute('aria-checked', String(Math.abs(parseFloat(b.dataset.fs) - cur) < 0.001)));
}

document.addEventListener('click', (e) => {
  const b = e.target.closest && e.target.closest('#fs-seg button');
  if (!b) return;
  const f = parseFloat(b.dataset.fs);
  document.documentElement.style.setProperty('--fs', f);
  try { localStorage.setItem('wr.fs', String(f)); } catch (_) { /* private mode */ }
  markFs();
});

// --- runner template editor ---------------------------------------------------------------------
// The instance table, architecture toggle, minimums and image picker of /settings/templates/{name}.
// Datastar holds the form values (hidden inputs are kept in step here); saving posts them.

function initTemplateEditor() {
  const root = document.getElementById('tpl');
  if (!root || root.dataset.ready) return;
  root.dataset.ready = '1';
  const $ = (id) => document.getElementById(id);
  const bound = (n) => root.querySelector('[data-bind="' + n + '"]');
  const setVal = (el, v) => { if (!el) return; el.value = v; el.dispatchEvent(new Event('input', { bubbles: true })); el.dispatchEvent(new Event('change', { bubbles: true })); };
  const toMiB = (s, bare) => {
    const m = /^\s*([\d.]+)\s*([A-Za-z]*)\s*$/.exec(s || '');
    if (!m) return 0;
    const mult = { mib: 1, m: 1, gib: 1024, g: 1024, tib: 1048576, t: 1048576 };
    const u = m[2].toLowerCase();
    return parseFloat(m[1]) * (u ? (mult[u] || 0) : bare);
  };
  const rows = [...root.querySelectorAll('#tp-types tbody tr')];
  const usd = (v) => '$' + v.toFixed(4);
  let arch = root.dataset.arch;

  const minV = () => parseInt((bound('tpvcpu') || {}).value, 10) || 0;
  const minM = () => toMiB((bound('tpram') || {}).value, 1);
  const fits = (tr) => parseInt(tr.dataset.vcpu, 10) >= minV() && parseFloat(tr.dataset.mem) >= minM();

  function refresh() {
    const picked = [];
    rows.forEach((tr) => {
      const here = tr.dataset.arch === arch;
      tr.hidden = !here;
      const ok = fits(tr);
      const cb = tr.querySelector('input[type=checkbox]');
      tr.classList.toggle('nofit', !ok);
      cb.disabled = !ok;
      if (!here) cb.checked = false;
      if (here && ok && cb.checked) picked.push(tr);
    });
    setVal($('tp-types-val'), picked.map((tr) => tr.dataset.type).join(','));
    summary(picked);
  }

  function cheapest(picked) {
    const pool = picked.length ? picked : rows.filter((tr) => tr.dataset.arch === arch && fits(tr));
    return pool.filter((tr) => tr.dataset.known === '1').sort((a, b) => parseFloat(a.dataset.price) - parseFloat(b.dataset.price))[0];
  }

  function summary(picked) {
    const el = $('tp-summary'), mc = $('tp-maxcost');
    const c = cheapest(picked);
    if (!el) return;
    if (!c) { el.textContent = 'Nothing fits these minimums for ' + arch + '.'; if (mc) mc.textContent = ''; return; }
    const p = parseFloat(c.dataset.price), max = parseInt((bound('tpmax') || {}).value, 10) || 0;
    el.innerHTML = 'Would launch <b class="mono">' + c.dataset.type + '</b> at <b>' + usd(p) + '/h</b>' + (picked.length ? '' : ' (none ticked: the cheapest that fits)') +
      (max > 1 ? '; all ' + max + ' runners busy <b>' + usd(p * max) + '/h</b>' : '') + '. <span class="dim">Plus the image\'s software fee, if any.</span>';
    if (mc) mc.textContent = max ? 'at most ' + usd(p * max) + '/h at the cheapest pick' : '';
  }

  function setArch(a) {
    arch = a;
    root.querySelectorAll('#tp-arch button').forEach((b) => b.setAttribute('aria-checked', String(b.dataset.arch === a)));
    setVal($('tp-arch-val'), a);
    refresh();
  }
  root.querySelectorAll('#tp-arch button').forEach((b) => b.addEventListener('click', () => setArch(b.dataset.arch)));
  rows.forEach((tr) => tr.querySelector('input[type=checkbox]').addEventListener('change', refresh));
  ['tpvcpu', 'tpram', 'tpmax'].forEach((n) => { const el = bound(n); if (el) el.addEventListener('input', refresh); });
  // Without a catalogue (no AWS) the types are a plain text field.
  const tt = $('tp-types-text');
  if (tt) tt.addEventListener('input', () => setVal($('tp-types-val'), tt.value));

  // --- image ---
  const imgVal = $('tp-img-val'), box = $('tp-ami-box');
  function setImg(mode) {
    root.querySelectorAll('input[name=tpimgr]').forEach((r) => { r.checked = r.value === mode; });
    box.hidden = mode !== 'ami';
    setVal(imgVal, mode);
  }
  root.querySelectorAll('input[name=tpimgr]').forEach((r) => r.addEventListener('change', () => setImg(r.value)));
  const amiEl = $('tp-ami'), chk = $('tp-ami-check');
  let ct;
  async function checkAmi() {
    const ami = (amiEl.value || '').trim();
    chk.textContent = '';
    if (!/^ami-[0-9a-f]+$/.test(ami)) return;
    const c = cheapest(rows.filter((tr) => tr.dataset.arch === arch && tr.querySelector('input').checked)) || cheapest([]);
    if (!c) return;
    chk.textContent = 'Checking with EC2…';
    try {
      const r = await fetch('/api/v1/images/check?ami=' + encodeURIComponent(ami) + '&type=' + encodeURIComponent(c.dataset.type), { credentials: 'same-origin' });
      const j = await r.json();
      if (j.ok) chk.innerHTML = '<span class="ok">✓ EC2 says it can launch this image on ' + c.dataset.type + '</span>';
      else if (j.opt_in_required) chk.innerHTML = '<span class="warn">! Not subscribed.</span> Accept the vendor\'s terms in the AWS console first' + (j.subscribe_url ? ': <a href="' + j.subscribe_url.replace(/"/g, '') + '" target="_blank" rel="noopener">open the Marketplace page</a>' : '') + '.';
      else chk.innerHTML = '<span class="warn">! ' + String(j.error || r.status).replace(/</g, '&lt;') + '</span>';
    } catch (e) { chk.textContent = ''; }
  }
  if (amiEl) amiEl.addEventListener('input', () => { clearTimeout(ct); ct = setTimeout(checkAmi, 700); });
  // The full test: launch one instance, check what a runner bootstrap needs, terminate it.
  const tb = $('tp-ami-test'), tout = $('tp-ami-test-out');
  if (tb) tb.addEventListener('click', async () => {
    const ami = (amiEl.value || '').trim();
    if (!/^ami-[0-9a-f]+$/.test(ami)) { tout.textContent = 'Enter an AMI id first.'; return; }
    const c = cheapest(rows.filter((tr) => tr.dataset.arch === arch && tr.querySelector('input').checked)) || cheapest([]);
    if (!c) { tout.textContent = 'Pick an instance type first.'; return; }
    tb.disabled = true;
    tout.textContent = 'Launching ' + c.dataset.type + ' and waiting for it to report (about 2 minutes)…';
    const esc = (v) => String(v == null ? '' : v).replace(/[&<>"]/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[ch]));
    try {
      const r = await fetch('/api/v1/images/test?ami=' + encodeURIComponent(ami) + '&type=' + encodeURIComponent(c.dataset.type) + '&arch=' + encodeURIComponent(arch), { method: 'POST', credentials: 'same-origin' });
      const j = await r.json();
      if (!r.ok) { tout.textContent = j.error || r.status; return; }
      tout.innerHTML = (j.checks || []).map((k) => '<li><span class="' + (k.ok ? 'ok">✓' : 'warn">✗') + '</span> ' + esc(k.name) + (k.detail ? ' <span class="dim small">' + esc(k.detail) + '</span>' : '') + '</li>').join('') +
        '<li class="dim small">' + (j.terminated ? 'Test instance terminated.' : 'Could not terminate the test instance ' + esc(j.instance_id) + '; the reaper will remove it.') + '</li>';
    } catch (e) { tout.textContent = 'Test failed: ' + e; } finally { tb.disabled = false; }
  });
  const ms = $('tp-msearch');
  async function search() {
    const out = $('tp-mres'), q = ($('tp-mq').value || '').trim();
    out.textContent = 'Searching…';
    try {
      const r = await fetch('/api/v1/images?q=' + encodeURIComponent(q) + '&arch=' + encodeURIComponent(arch), { credentials: 'same-origin' });
      const j = await r.json();
      if (!r.ok) { out.textContent = j.error || r.status; return; }
      out.innerHTML = '';
      (j.images || []).forEach((im) => {
        const row = document.createElement('div');
        row.className = 'row mrow';
        row.innerHTML = '<span class="grow"><b></b> <span class="dim small"></span></span><code class="mono small"></code><button type="button" class="btn small">Use</button>';
        row.querySelector('b').textContent = im.name;
        row.querySelector('.dim').textContent = im.arch;
        row.querySelector('code').textContent = im.id;
        row.querySelector('button').addEventListener('click', () => { setImg('ami'); setVal(amiEl, im.id); if (im.arch !== arch) setArch(im.arch); checkAmi(); });
        out.appendChild(row);
      });
      if (!out.children.length) out.textContent = 'No Marketplace images for "' + q + '" on ' + arch + '.';
    } catch (e) { out.textContent = 'Search failed.'; }
  }
  if (ms) { ms.addEventListener('click', search); $('tp-mq').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); search(); } }); }

  setArch(arch);
  setImg(root.dataset.img || 'al');
  if ((amiEl.value || '').startsWith('ami-')) checkAmi();
}

// --- boot --------------------------------------------------------------------------------

function boot() {
  initSortables();
  initQueueSort();
  initTerminal();
  markFs();
  initTemplateEditor();
  pushState().catch(() => {});
  const main = document.getElementById('main');
  if (main) {
    let t;
    let rt;
    new MutationObserver((muts) => {
      clearTimeout(t); t = setTimeout(() => { initSortables(); initQueueSort(); markFs(); }, 50);
      if (openDetail) { clearTimeout(rt); rt = setTimeout(checkDetail, 600); }
    }).observe(main, { childList: true, subtree: true });
  }
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => {});
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();

// Setup page: put the chosen App name into the manifest before it goes to GitHub.
function ghManifest(form) {
  const name = form.querySelector('.gh-name').value.trim();
  if (!name) return false;
  const field = form.querySelector('.gh-manifest');
  const m = JSON.parse(field.value);
  m.name = name;
  field.value = JSON.stringify(m);
  return true;
}
window.ghManifest = ghManifest;

// Lightbox for deliverable images: click a thumbnail (a[data-lightbox]) to view it full size;
// arrows / swipe-free buttons step through the page's images, Esc closes.
(function () {
  let dlg, imgs = [], at = 0;
  function build() {
    dlg = document.createElement('dialog');
    dlg.className = 'lightbox';
    dlg.setAttribute('aria-label', 'Image viewer');
    dlg.innerHTML = '<div class="lb"><img alt=""><div class="cap"><button type="button" data-lb="prev" aria-label="Previous">‹</button><span></span><button type="button" data-lb="next" aria-label="Next">›</button><button type="button" data-lb="close">Close</button></div></div>';
    dlg.addEventListener('click', (e) => {
      const b = e.target.closest('[data-lb]');
      if (b && b.dataset.lb === 'prev') show(at - 1);
      else if (b && b.dataset.lb === 'next') show(at + 1);
      else if (!b && e.target.tagName !== 'IMG' || (b && b.dataset.lb === 'close')) dlg.close();
    });
    dlg.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowLeft') show(at - 1);
      else if (e.key === 'ArrowRight') show(at + 1);
    });
    document.body.appendChild(dlg);
  }
  function show(i) {
    if (!imgs.length) return;
    at = (i + imgs.length) % imgs.length;
    const a = imgs[at];
    dlg.querySelector('img').src = a.href;
    dlg.querySelector('img').alt = a.textContent.trim();
    dlg.querySelector('.cap span').textContent = a.textContent.trim() + (imgs.length > 1 ? '  (' + (at + 1) + '/' + imgs.length + ')' : '');
    dlg.querySelectorAll('[data-lb=prev],[data-lb=next]').forEach((b) => { b.hidden = imgs.length < 2; });
  }
  document.addEventListener('click', (e) => {
    const a = e.target.closest && e.target.closest('a[data-lightbox]');
    if (!a || e.metaKey || e.ctrlKey || e.shiftKey || typeof HTMLDialogElement === 'undefined') return;
    e.preventDefault();
    if (!dlg) build();
    imgs = Array.from(a.closest('.gallery').querySelectorAll('a[data-lightbox]'));
    show(imgs.indexOf(a));
    dlg.showModal();
  });
})();

// Inline confirm. Put data-confirm="Question?" on any button (optionally data-confirm-yes="Delete"
// for the confirming button's label) and its click is held back: the button is replaced, in
// place, by "Question? [Yes] [Cancel]". Yes re-fires the original click (so Datastar's
// data-on:click, a form submit, or a link behave as if the first click had gone through);
// Cancel, Esc, or focus leaving the group restores the button. Never uses window.confirm().
(function () {
  function ask(btn, prompt, yesLabel, onYes) {
    const box = document.createElement('span');
    box.className = 'iconfirm';
    box.setAttribute('role', 'group');
    box.setAttribute('aria-label', 'Confirm');
    const q = document.createElement('span');
    q.className = 'iq';
    q.textContent = prompt;
    const yes = document.createElement('button');
    yes.type = 'button';
    yes.className = 'btn small danger yes';
    yes.textContent = yesLabel || 'Yes';
    const no = document.createElement('button');
    no.type = 'button';
    no.className = 'btn small no';
    no.textContent = 'Cancel';
    box.append(q, yes, no);
    const prevHidden = btn.hidden;
    btn.hidden = true;
    btn.after(box);
    const close = (refocus) => {
      box.remove();
      btn.hidden = prevHidden;
      if (refocus) btn.focus();
    };
    yes.addEventListener('click', () => { close(false); onYes(); });
    no.addEventListener('click', () => close(true));
    box.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { e.stopPropagation(); close(true); }
    });
    box.addEventListener('focusout', (e) => {
      if (!box.isConnected || (e.relatedTarget && box.contains(e.relatedTarget))) return;
      // A tap elsewhere on a phone may not report relatedTarget; give a click on Yes/Cancel time to land.
      setTimeout(() => { if (box.isConnected && !box.contains(document.activeElement)) close(false); }, 150);
    });
    no.focus(); // the safe choice has focus: Enter/Space cancel, Tab reaches Yes
  }
  window.wrConfirm = ask;

  document.addEventListener('click', (e) => {
    const btn = e.target.closest && e.target.closest('[data-confirm]');
    if (!btn || btn.disabled) return;
    if (btn.dataset.confirmed === '1') { delete btn.dataset.confirmed; return; }
    e.preventDefault();
    e.stopImmediatePropagation(); // before Datastar's handler on the button
    ask(btn, btn.dataset.confirm, btn.dataset.confirmYes, () => {
      btn.dataset.confirmed = '1';
      btn.click();
      delete btn.dataset.confirmed;
    });
  }, true);
})();

// Flash the row of the task that was just acted on (queue, dequeue, move). The act's response asks
// for it; the row may only appear once the live patch has re-rendered #main, so it is applied
// when it shows up (for a few seconds at most).
(function () {
  let want = null;
  function apply() {
    if (!want) return;
    if (Date.now() > want.until) { want = null; return; }
    const li = document.querySelector('li.task[data-sqid="' + want.sqid + '"]');
    if (!li) return;
    want = null;
    li.classList.remove('flash');
    void li.offsetWidth; // restart the animation
    li.classList.add('flash');
    li.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }
  window.wrFlash = (sqid) => {
    want = { sqid, until: Date.now() + 5000 };
    setTimeout(apply, 0);
  };
  new MutationObserver(apply).observe(document.documentElement, { childList: true, subtree: true });
})();

// Project chat: keep the newest message in view, and let a task id in a reply jump to its row
// (closing the full-screen chat on a phone first); a task not on this page opens its own page.
(function () {
  function toEnd() { document.querySelectorAll('.projchat .chatlog').forEach((el) => { el.scrollTop = el.scrollHeight; }); }
  new MutationObserver(toEnd).observe(document.documentElement, { childList: true, subtree: true });
  document.addEventListener('click', (e) => {
    const a = e.target.closest && e.target.closest('a.tlink');
    if (!a) return;
    const sq = a.dataset.goto;
    if (!document.querySelector('li.task[data-sqid="' + sq + '"]')) return; // follow the link to /task/<sqid>
    e.preventDefault();
    if (window.matchMedia('(max-width: 899px)').matches) document.querySelector('.projchat .chathead .btn')?.click();
    setTimeout(() => window.wrFlash && wrFlash(sq), 50);
  });
})();
