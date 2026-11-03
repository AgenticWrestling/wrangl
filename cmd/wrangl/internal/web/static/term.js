// wrangl runner terminal: one tab per agent session or shell on a runner, each a wterm
// instance kept alive so its scrollback survives tab switches. View-only by default;
// "Controlling" is an explicit switch. Server protocol: see the comment above connect().
'use strict';
(function () {
  const app = document.getElementById('termapp');
  if (!app || app.dataset.usable !== '1' || !window.wterm) return;

  const NAME = app.dataset.runner;
  const API = '/api/v1/runners/' + encodeURIComponent(NAME);
  const $ = (id) => document.getElementById(id);
  const enc = new TextEncoder();
  const store = {
    get(k, d) { try { const v = localStorage.getItem(k); return v === null ? d : v; } catch (_) { return d; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch (_) { /* private mode */ } },
  };

  // --- engine adapter ---------------------------------------------------------------------
  // Everything wterm-specific lives here. To swap in xterm.js, return the same handle:
  //   write(bytes) resize(cols, rows) fit() size() focus() setFont(px) lockHeight(bool) setPaused(bool) dispose()
  // and call o.onData(string) for typed input, o.onBytes(Uint8Array) for raw (mouse) bytes and
  // o.onResize(cols, rows) after the grid changes.
  const Engine = {
    name: 'wterm',
    async create(el, o) {
      const t = new window.wterm.WTerm(el, {
        cols: o.cols, rows: o.rows, autoResize: false, cursorBlink: true,
        onData: o.onData, onBinary: o.onBytes, onResize: o.onResize,
      });
      await t.init();
      let rowPx = 16;
      return {
        write: (bytes) => t.write(bytes),
        resize: (c, r) => { if (c !== t.cols || r !== t.rows) t.resize(c, r); },
        fit: () => t.fit(),
        size: () => ({ cols: t.cols, rows: t.rows }),
        focus: () => t.focus(),
        setFont(px) {
          rowPx = Math.round(px * 1.2);
          el.style.setProperty('--term-font-size', px + 'px');
          el.style.setProperty('--term-row-height', rowPx + 'px');
        },
        // wterm locks the element's height once, at init. Watching: pin it to the grid so the
        // whole PTY is there to scroll; controlling: clear it so the element fills the stage.
        lockHeight(on) { el.style.height = on ? (t.rows * rowPx + 12) + 'px' : ''; },
        setPaused: (p) => t.setRenderingPaused && t.setRenderingPaused(p),
        dispose: () => t.destroy(),
      };
    },
  };

  // --- state ------------------------------------------------------------------------------
  const tabs = new Map();      // id -> tab
  let active = null;           // active tab id
  let controlling = false;     // the page-level Control switch (per active tab on the wire)
  let fitOn = store.get('term.fit', '1') === '1';
  const mods = { ctrl: false, alt: false };
  const narrow = () => window.matchMedia('(max-width: 560px)').matches;
  const baseFont = () => (narrow() ? 12 : 13);
  const ADVANCE = 0.6;         // IBM Plex Mono advance width in em

  const tabsEl = $('ttabs'), stage = $('tstage'), emptyEl = $('tempty'), stateEl = $('tstate');
  const ctlBtn = $('tctl'), fitBtn = $('tfit'), shellBtn = $('tshell'), keysEl = $('tkeys');

  function setState(text, cls) {
    stateEl.textContent = text;
    stateEl.className = 'tstate' + (cls ? ' ' + cls : '');
  }

  function banner(tab, msg) {
    let b = tab.pane.querySelector('.tbanner');
    if (!b) { b = document.createElement('div'); b.className = 'tbanner'; b.setAttribute('role', 'alert'); tab.pane.appendChild(b); }
    b.textContent = msg || '';
    b.hidden = !msg;
    clearTimeout(tab.bannerT);
    if (msg) tab.bannerT = setTimeout(() => { b.hidden = true; }, 6000);
  }

  function refreshState() {
    const t = tabs.get(active);
    if (!t) { setState(tabs.size ? 'pick a session' : 'no sessions'); return; }
    if (t.ended) setState('ended', 'bad');
    else if (t.ws && t.ws.readyState === WebSocket.OPEN && t.helloSeen) setState(controlling ? 'live · typing' : 'live', 'live');
    else setState(t.everOpen ? 'reconnecting…' : 'connecting…', t.everOpen ? 'bad' : '');
  }

  // --- layout: fit-to-width when watching, fit-to-screen when controlling -------------------
  function layout(tab) {
    if (!tab.h) return;
    const ctl = controlling && tab.id === active;
    tab.pane.classList.toggle('ctl', ctl);
    if (ctl) {
      tab.pane.classList.remove('fitted');
      tab.h.setFont(baseFont());
      tab.h.lockHeight(false);
      requestAnimationFrame(() => {
        tab.h.fit();
        sendSize(tab);
      });
      return;
    }
    const s = tab.server;
    if (!s) return;
    let px = baseFont();
    if (fitOn) {
      const avail = tab.scroll.clientWidth - 12;
      // never below 7px: a 200-column PTY on a phone then scrolls sideways instead of being unreadable
      px = Math.floor(Math.max(7, Math.min(baseFont() + 1, avail / (s.cols * ADVANCE))) * 10) / 10;
    }
    tab.pane.classList.toggle('fitted', fitOn);
    tab.h.setFont(px);
    tab.h.resize(s.cols, s.rows);
    tab.h.lockHeight(true);
  }
  const layoutAll = () => tabs.forEach(layout);

  function sendSize(tab, force) {
    if (!(controlling && tab.id === active) || !tab.h || !tab.ws || tab.ws.readyState !== WebSocket.OPEN) return;
    const { cols, rows } = tab.h.size();
    const key = cols + 'x' + rows;
    if (!force && tab.sentSize === key) return;
    tab.sentSize = key;
    tab.ws.send(JSON.stringify({ t: 'size', cols, rows }));
  }

  // --- websocket ----------------------------------------------------------------------------
  // Server -> client: BINARY = PTY output (history replay, then live). TEXT JSON:
  //   {t:'hello',cols,rows,live} first; {t:'size',cols,rows}; {t:'end'}; {t:'control',on}; {t:'error',msg}.
  // Client -> server: TEXT {t:'control',on}, {t:'size',cols,rows}; BINARY = keystrokes (dropped
  // by the server unless control is on).
  function connect(tab) {
    if (tab.disposed || tab.ended) return;
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(scheme + '//' + location.host + API + '/term/' + encodeURIComponent(tab.id));
    ws.binaryType = 'arraybuffer';
    tab.ws = ws;
    tab.helloSeen = false;
    ws.onopen = () => {
      tab.everOpen = true;
      tab.backoff = 1000;
      tab.sentSize = '';
      if (controlling && tab.id === active) ws.send(JSON.stringify({ t: 'control', on: true }));
      else if (tab.id === active && tab.kind === 'shell' && !tab.released && !tab.ended) autoControl(tab);
      refreshState();
    };
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') { onControlFrame(tab, e.data); return; }
      const bytes = new Uint8Array(e.data);
      if (!tab.h) { tab.pending.push(bytes); return; }
      feed(tab, bytes);
    };
    ws.onclose = () => {
      if (tab.ws === ws) tab.ws = null;
      if (tab.disposed || tab.ended) { refreshState(); return; }
      refreshState();
      tab.timer = setTimeout(() => connect(tab), tab.backoff);
      tab.backoff = Math.min(tab.backoff * 2, 15000);
    };
    ws.onerror = () => { /* onclose follows and reconnects */ };
  }

  function feed(tab, bytes) {
    const sc = tab.scroll;
    const stick = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 40;
    tab.h.write(bytes);
    if (stick && !tab.pane.classList.contains('ctl')) requestAnimationFrame(() => { sc.scrollTop = sc.scrollHeight; });
  }

  function onControlFrame(tab, text) {
    let m;
    try { m = JSON.parse(text); } catch (_) { return; }
    switch (m.t) {
      case 'hello':
        if (tab.sawHello && tab.h) tab.h.write(enc.encode('\x1bc')); // reconnect replays history from the start
        tab.sawHello = true;
        tab.helloSeen = true;
        tab.server = { cols: m.cols, rows: m.rows };
        if (m.live === false) markEnded(tab);
        layout(tab);
        break;
      case 'size':
        tab.server = { cols: m.cols, rows: m.rows };
        if (!(controlling && tab.id === active)) layout(tab);
        break;
      case 'end':
        markEnded(tab);
        break;
      case 'control':
        if (tab.id === active) setControlUI(!!m.on);
        break;
      case 'error':
        banner(tab, m.msg || 'error');
        if (tab.id === active && controlling && !tab.ctlOk) setControlUI(false);
        break;
    }
    refreshState();
  }

  function markEnded(tab) {
    tab.ended = true;
    tab.btn.classList.add('ended');
    renderTabLabel(tab);
    refreshState();
  }

  // --- tabs ---------------------------------------------------------------------------------
  function renderTabLabel(tab) {
    tab.btn.textContent = '';
    const label = document.createElement('span');
    label.textContent = tab.title + (tab.ended ? ' (ended)' : '');
    tab.btn.appendChild(label);
    if (tab.kind === 'shell' || tab.ended) {
      const x = document.createElement('span');
      x.className = 'tx';
      x.setAttribute('role', 'button');
      x.setAttribute('aria-label', 'Close ' + tab.title);
      x.textContent = '✕';
      x.addEventListener('click', (e) => { e.stopPropagation(); closeTab(tab); });
      tab.btn.appendChild(x);
    }
  }

  function ensureTab(s) {
    let tab = tabs.get(s.id);
    if (tab) {
      if (s.title && s.title !== tab.title) { tab.title = s.title; renderTabLabel(tab); }
      return tab;
    }
    const pane = document.createElement('div');
    pane.className = 'tpane';
    const scroll = document.createElement('div');
    scroll.className = 'tscroll';
    const el = document.createElement('div');
    el.className = 'tterm wterm';
    scroll.appendChild(el);
    pane.appendChild(scroll);
    stage.appendChild(pane);

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'ttab';
    btn.setAttribute('role', 'tab');
    btn.setAttribute('aria-selected', 'false');
    btn.addEventListener('click', () => select(s.id));

    tab = { id: s.id, kind: s.kind, title: s.title || s.id, pane, scroll, el, btn, pending: [], backoff: 1000, created: Date.now() };
    tabs.set(s.id, tab);
    renderTabLabel(tab);
    // run tabs first, shells after, each group in arrival order
    const firstShell = [...tabs.values()].find((t) => t !== tab && t.kind === 'shell');
    if (s.kind === 'run' && firstShell) tabsEl.insertBefore(btn, firstShell.btn); else tabsEl.appendChild(btn);
    emptyEl.hidden = true;

    Engine.create(el, {
      cols: 80, rows: 24,
      onData: (d) => sendTyped(tab, d),
      onBytes: (b) => { if (controlling && tab.id === active && tab.ws && tab.ws.readyState === WebSocket.OPEN) tab.ws.send(b); },
      onResize: () => { if (controlling && tab.id === active) sendSize(tab); },
    }).then((h) => {
      if (tab.disposed) { h.dispose(); return; }
      tab.h = h;
      h.setFont(baseFont());
      h.setPaused(tab.id !== active);
      tab.pending.forEach((b) => feed(tab, b));
      tab.pending = [];
      layout(tab);
    }).catch((err) => { banner(tab, 'Terminal failed to start: ' + err); });

    connect(tab);
    return tab;
  }

  let autoCtl = false;         // control was taken automatically (shell tab), not by the button
  function autoControl(tab) {
    autoCtl = true;
    if (tab.ws && tab.ws.readyState === WebSocket.OPEN) tab.ws.send(JSON.stringify({ t: 'control', on: true }));
  }

  function select(id) {
    const next = tabs.get(id);
    if (!next) return;
    const prev = tabs.get(active);
    if (prev && prev !== next) {
      if (controlling && prev.ws && prev.ws.readyState === WebSocket.OPEN) prev.ws.send(JSON.stringify({ t: 'control', on: false }));
      prev.ctlOk = false;
      prev.sentSize = '';
      prev.pane.classList.remove('on');
      prev.btn.setAttribute('aria-selected', 'false');
      if (prev.h) prev.h.setPaused(true);
    }
    active = id;
    next.pane.classList.add('on');
    next.btn.setAttribute('aria-selected', 'true');
    next.btn.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    if (next.h) next.h.setPaused(false);
    try { history.replaceState(null, '', '#' + encodeURIComponent(id)); } catch (_) { /* ignore */ }
    // Shells open in control (unless released by hand); agent runs stay view-only.
    if (next.kind === 'shell' && !next.released && !controlling) autoControl(next);
    else if (next.kind !== 'shell' && controlling && autoCtl) setControlUI(false);
    else if (controlling && next.ws && next.ws.readyState === WebSocket.OPEN) next.ws.send(JSON.stringify({ t: 'control', on: true }));
    layout(next);
    refreshState();
  }

  function closeTab(tab) {
    const go = () => {
      tab.disposed = true;
      clearTimeout(tab.timer);
      if (tab.ws) tab.ws.close();
      if (tab.h) tab.h.dispose();
      tab.pane.remove();
      tab.btn.remove();
      tabs.delete(tab.id);
      if (active === tab.id) {
        active = null;
        const first = [...tabs.values()][0];
        if (first) select(first.id); else { emptyEl.hidden = false; setControlUI(false); }
      }
      refreshState();
    };
    if (tab.kind === 'shell' && !tab.ended) {
      if (!window.confirm('Close ' + tab.title + '? Anything running in it will stop.')) return;
      fetch(API + '/term/' + encodeURIComponent(tab.id), { method: 'DELETE', credentials: 'same-origin' }).then(go, go);
    } else go();
  }

  // --- polling the session list ---------------------------------------------------------------
  let preferred = decodeURIComponent((location.hash || '').slice(1));
  async function poll() {
    try {
      const r = await fetch(API + '/terms', { credentials: 'same-origin', cache: 'no-store' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const { sessions } = await r.json();
      const seen = new Set();
      const ordered = [...sessions].sort((a, b) => (a.kind === b.kind ? 0 : a.kind === 'run' ? -1 : 1));
      ordered.forEach((s) => { seen.add(s.id); ensureTab(s); });
      tabs.forEach((t) => {
        // gone from the list (and not just created locally): its session is over
        if (!seen.has(t.id) && !t.ended && Date.now() - t.created > 10000) markEnded(t);
      });
      if (!active || !tabs.has(active)) {
        const pick = tabs.has(preferred) ? preferred : ([...tabs.values()].find((t) => t.kind === 'run' && !t.ended) || [...tabs.values()][0] || {}).id;
        if (pick) select(pick);
      }
      refreshState();
    } catch (_) {
      if (!tabs.size) setState('cannot reach the controller', 'bad');
    }
  }

  // --- input ------------------------------------------------------------------------------------
  function canType() {
    const t = tabs.get(active);
    return controlling && t && t.ws && t.ws.readyState === WebSocket.OPEN ? t : null;
  }

  function clearMods() {
    mods.ctrl = mods.alt = false;
    keysEl.querySelectorAll('[data-mod]').forEach((b) => b.setAttribute('aria-pressed', 'false'));
  }

  // Sticky Ctrl/Alt from the bar apply to the next character typed on any keyboard.
  function applyMods(s) {
    if (mods.ctrl && s.length === 1) {
      let c = s.charCodeAt(0);
      if (c >= 97 && c <= 122) c -= 96;
      else if ((c >= 64 && c <= 95)) c -= 64;
      else if (c === 63) c = 127;
      else if (c === 32) c = 0;
      s = String.fromCharCode(c);
    }
    if (mods.alt) s = '\x1b' + s;
    clearMods();
    return s;
  }

  function sendTyped(tab, data) {
    const t = canType();
    if (!t || t !== tab || !data) return;
    t.ws.send(enc.encode(mods.ctrl || mods.alt ? applyMods(data) : data));
  }

  function sendRaw(s) {
    const t = canType();
    if (t) t.ws.send(enc.encode(s));
  }

  const csi = (final, num) => {
    const m = 1 + (mods.alt ? 2 : 0) + (mods.ctrl ? 4 : 0);
    const out = m > 1 ? '\x1b[' + (num || 1) + ';' + m + final : '\x1b[' + (num || '') + final;
    clearMods();
    return out;
  };
  const KEYS = {
    esc: () => '\x1b',
    tab: () => (mods.alt ? (clearMods(), '\x1b\t') : (clearMods(), '\t')),
    up: () => csi('A'), down: () => csi('B'), right: () => csi('C'), left: () => csi('D'),
    home: () => csi('H'), end: () => csi('F'), pgup: () => csi('~', 5), pgdn: () => csi('~', 6),
    ctrlc: () => (clearMods(), '\x03'),
  };

  function pressKey(btn) {
    if (btn.disabled) return;
    const mod = btn.dataset.mod;
    if (mod) {
      mods[mod] = !mods[mod];
      btn.setAttribute('aria-pressed', String(mods[mod]));
      return;
    }
    if (btn.dataset.k === 'kbd') { const t = tabs.get(active); if (t && t.h) t.h.focus(); return; }
    if (btn.dataset.t) { sendRaw(applyMods(btn.dataset.t)); return; }
    const f = KEYS[btn.dataset.k];
    if (f) sendRaw(f());
  }

  // pointerdown keeps focus (and the soft keyboard) on the terminal; keyboard users get click.
  keysEl.addEventListener('pointerdown', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    e.preventDefault();
    b.dataset.handled = '1';
    pressKey(b);
  });
  keysEl.addEventListener('click', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    if (b.dataset.handled) { delete b.dataset.handled; return; }
    pressKey(b);
  });

  // --- controls ---------------------------------------------------------------------------------
  function setControlUI(on) {
    controlling = on;
    const t = tabs.get(active);
    if (t) t.ctlOk = on;
    app.classList.toggle('controlling', on);
    ctlBtn.setAttribute('aria-pressed', String(on));
    ctlBtn.textContent = on ? 'Release' : 'Take control';
    keysEl.querySelectorAll('button').forEach((b) => { b.disabled = !on; });
    if (!on) { clearMods(); tabs.forEach((x) => { x.sentSize = ''; }); }
    layoutAll();
    if (on && t && t.h) setTimeout(() => t.h.focus(), 50);
    refreshState();
  }

  ctlBtn.addEventListener('click', () => {
    const t = tabs.get(active);
    if (!t) return;
    autoCtl = false;
    if (controlling) {
      t.released = true;
      if (t.ws && t.ws.readyState === WebSocket.OPEN) t.ws.send(JSON.stringify({ t: 'control', on: false }));
      setControlUI(false);
      return;
    }
    if (!t.ws || t.ws.readyState !== WebSocket.OPEN) { banner(t, 'Not connected yet.'); return; }
    if (t.ended) { banner(t, 'This session has ended.'); return; }
    // The UI switches when the server confirms ({t:'control', on:true}); errors show a banner.
    t.released = false;
    t.ws.send(JSON.stringify({ t: 'control', on: true }));
  });

  fitBtn.setAttribute('aria-pressed', String(fitOn));
  fitBtn.addEventListener('click', () => {
    fitOn = !fitOn;
    store.set('term.fit', fitOn ? '1' : '0');
    fitBtn.setAttribute('aria-pressed', String(fitOn));
    layoutAll();
  });

  shellBtn.addEventListener('click', async () => {
    shellBtn.disabled = true;
    try {
      const r = await fetch(API + '/shell', { method: 'POST', credentials: 'same-origin' });
      if (!r.ok) throw new Error((await r.text()) || 'HTTP ' + r.status);
      const { id } = await r.json();
      ensureTab({ id, kind: 'shell', title: id.replace('sh-', 'shell ') });
      select(id);
    } catch (err) {
      setState('shell failed: ' + err.message, 'bad');
    } finally {
      shellBtn.disabled = false;
    }
  });

  // --- viewport: shrink above the on-screen keyboard (iOS does not resize the layout) -------------
  let fitRaf = 0;
  function onViewport() {
    const v = window.visualViewport;
    if (v) {
      app.style.height = v.height + 'px';
      app.style.top = v.offsetTop + 'px';
    }
    cancelAnimationFrame(fitRaf);
    fitRaf = requestAnimationFrame(layoutAll);
  }
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', onViewport);
    window.visualViewport.addEventListener('scroll', onViewport);
  }
  window.addEventListener('resize', onViewport);
  window.addEventListener('hashchange', () => {
    const id = decodeURIComponent(location.hash.slice(1));
    if (tabs.has(id)) select(id); else preferred = id;
  });
  window.addEventListener('pagehide', () => tabs.forEach((t) => { t.disposed = true; if (t.ws) t.ws.close(); }));

  keysEl.querySelectorAll('button').forEach((b) => { b.disabled = true; });
  poll();
  setInterval(poll, 3000);
})();
