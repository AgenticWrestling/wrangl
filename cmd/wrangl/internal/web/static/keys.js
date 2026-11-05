// wrangl desktop keyboard control. Loaded after app.js. Ignored on touch devices, on the
// terminal page, with Ctrl/Cmd/Alt held, and while typing in an input or a dialog is open.
'use strict';
(function () {
  if (!matchMedia('(hover:hover) and (pointer:fine)').matches) return;

  // Selection, collapsed rows and the filter are Datastar signals (ksel, kcol, kfilter; see #kstate
  // in the layout and the data-class on each li.task), so a live patch cannot drop them. This file
  // only reads them back from #kstate's dataset and sends changes as window events.
  const state = () => document.getElementById('kstate').dataset;
  const set = (name, v) => { window.dispatchEvent(new CustomEvent(name, { detail: v })); state()[{ ksel: 'sel', kcol: 'col', kfilter: 'filter' }[name]] = v; };
  const sel0 = () => state().sel || '';
  const collapsedSet = () => new Set((state().col || '').split(',').filter(Boolean));
  let chord = '';               // 'g' while waiting for the second key
  let chordTimer;

  const $ = (s, r) => (r || document).querySelector(s);
  const LISTS = 'ul.tasks.root, ul.tasks.runs';
  const tree = () => $(LISTS);
  const li = (sq) => (sq ? $('li.task[data-sqid="' + sq + '"]') : null);
  const visible = () => [...document.querySelectorAll('ul.tasks.root li.task, ul.tasks.runs li.task')].filter((x) => x.offsetParent !== null);
  const kids = (x) => [...x.querySelectorAll(':scope > ul.tasks > li.task, :scope > details.sub > ul.tasks > li.task')];

  function select(x) {
    set('ksel', x ? x.dataset.sqid : '');
    if (x) { x.scrollIntoView({ block: 'nearest' }); x.focus({ preventScroll: true }); }
  }

  function toggleCollapse(x, want) {
    const sq = x.dataset.sqid, col = collapsedSet();
    const c = want === undefined ? !col.has(sq) : want;
    if (c) col.add(sq); else col.delete(sq);
    set('kcol', [...col].join(','));
  }

  function openDetailOf(sq) { return window.toggleDetail && window.toggleDetail(sq); }
  function detailOpen(sq) { const d = document.getElementById('d-' + sq); return !!(d && d.firstElementChild); }

  // --- help dialog ---
  function help() {
    let d = document.getElementById('khelp');
    if (!d) {
      d = document.createElement('dialog');
      d.id = 'khelp'; d.className = 'sheet khelp';
      const rows = (a) => a.map(([k, t]) => '<div>' + t + '<span>' + k.split(' ').map((x) => '<kbd>' + x + '</kbd>').join('') + '</span></div>').join('');
      d.innerHTML = '<h2>Keyboard shortcuts</h2><div class="g">' +
        '<h3>Move</h3>' + rows([['j k', 'Next / previous'], ['↓ ↑', 'Next / previous'], ['h', 'Collapse / to parent'], ['l', 'Expand / to child'], ['J K', 'Reorder down / up'], ['/', 'Filter']]) +
        '<h3>Selected task</h3>' + rows([['Enter o', 'Toggle details'], ['Esc', 'Collapse, then clear'], ['q', 'Queue'], ['e', 'Edit'], ['c', 'Complete'], ['s', 'Split'], ['a', 'Add subtask'], ['d', 'Needs…'], ['x', 'Cancel / reopen'], ['D', 'Delete (confirms)']]) +
        '<h3>Go</h3>' + rows([['g t', 'Tasks'], ['g b', 'Backlog'], ['g q', 'Queue'], ['g p', 'In progress'], ['g r', 'Runners'], ['g l', 'Logs'], ['g i', 'Inbox'], ['g m', 'Settings'], ['[ ]', 'Previous / next project'], ['1-9', 'Project n']]) +
        '<h3>Global</h3>' + rows([['n', 'New task'], ['?', 'This help']]) + '</div>' +
        '<div class="actions"><button class="btn" command="close" commandfor="khelp">Close</button></div>';
      document.body.appendChild(d);
    }
    if (!d.open) d.showModal();
  }

  // --- filter ---
  function openFilter() {
    let bar = document.getElementById('kbar');
    if (!bar) {
      bar = document.createElement('div');
      bar.id = 'kbar'; bar.className = 'kbar';
      bar.innerHTML = '<span class="dim">/</span><input aria-label="Filter tasks" placeholder="Filter tasks (Enter keeps, Esc clears)" autocomplete="off">';
      document.body.appendChild(bar);
      const inp = bar.querySelector('input');
      inp.addEventListener('input', () => set('kfilter', inp.value.trim().toLowerCase()));
      inp.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') { set('kfilter', ''); inp.value = ''; bar.remove(); e.stopPropagation(); }
        else if (e.key === 'Enter') { inp.blur(); const v = visible()[0]; if (v && !li(sel0())) select(v); if (!state().filter) bar.remove(); }
      });
    }
    bar.querySelector('input').focus();
  }

  // --- projects & nav ---
  function projectLinks() { return [...document.querySelectorAll('.projbar a[href^="/projects/"], .projbar .menu a')]; }
  function goProject(i) { const l = projectLinks(); if (l[i]) location.href = l[i].href; }
  function stepProject(d) {
    const l = projectLinks(); if (!l.length) return;
    let cur = l.findIndex((a) => a.classList.contains('on') || a.getAttribute('aria-current'));
    if (cur < 0) cur = 0;
    goProject((cur + d + l.length) % l.length);
  }
  const GO = { t: '/projects', b: '/projects#backlog', q: '/projects#queue', p: '/projects#in-progress', r: '/runners', l: '/logs', i: '/inbox', m: '/settings' };

  // --- reorder ---
  async function reorder(x, dir) {
    if (x.parentElement.classList.contains('runs')) return; // run entries are not reorderable
    const sibs = [...x.parentElement.children].filter((c) => c.dataset && c.dataset.sqid);
    const i = sibs.indexOf(x);
    const j = i + dir;
    if (j < 0 || j >= sibs.length) return;
    const parent = x.parentElement.dataset.parent || '';
    const body = parent === '' ? { root: true } : { parent };
    if (dir > 0) body.after = sibs[j].dataset.sqid;
    else if (j === 0) body.first = true;
    else body.after = sibs[j - 1].dataset.sqid;
    const ok = await window.postMove(x.dataset.sqid, body);
    if (ok) { // reflect it now; the live stream confirms
      if (dir > 0) sibs[j].after(x); else sibs[j].before(x);
      select(x);
    }
  }

  function press(key) { // click the data-key button of the selected task's open card (or page)
    const sel = sel0();
    const root = sel && detailOpen(sel) ? document.getElementById('d-' + sel) : $('#main section.card');
    const b = root && root.querySelector('[data-key="' + key + '"]');
    if (b) { b.click(); return true; }
    return false;
  }

  document.addEventListener('keydown', (e) => {
    if (e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented) return;
    if (/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName) || e.target.isContentEditable || e.target.closest('dialog') || document.querySelector('dialog[open]')) return;
    if (document.getElementById('term')) return;
    const k = e.key;
    if (chord) {
      const c = chord; chord = ''; clearTimeout(chordTimer);
      if (c === 'g' && GO[k]) { e.preventDefault(); location.href = GO[k]; }
      return;
    }
    const onList = !!tree();
    const sel = sel0(), collapsed = collapsedSet(), filter = state().filter;
    const cur = li(sel);
    let handled = true;
    if (k === 'g') { chord = 'g'; chordTimer = setTimeout(() => { chord = ''; }, 1200); }
    else if (k === '?') help();
    else if (k === 'n') { const b = $('[data-key="n"]'); if (b) b.click(); else handled = false; }
    else if (k === '[') stepProject(-1);
    else if (k === ']') stepProject(1);
    else if (/^[1-9]$/.test(k) && onList) goProject(+k - 1);
    else if (k === '/' && onList) openFilter();
    else if (!onList) handled = k === 'q' || k === 'e' || k === 'c' || k === 's' || k === 'a' || k === 'd' || k === 'x' || k === 'D' ? press(k) : false;
    else if (k === 'j' || k === 'ArrowDown' || k === 'k' || k === 'ArrowUp') {
      const v = visible(); const i = v.indexOf(cur);
      const d = k === 'j' || k === 'ArrowDown' ? 1 : -1;
      select(v[i < 0 ? (d > 0 ? 0 : v.length - 1) : Math.max(0, Math.min(v.length - 1, i + d))]);
    } else if (k === 'J' || k === 'K') { if (cur) reorder(cur, k === 'J' ? 1 : -1); }
    else if (k === 'Enter' || k === 'o') { if (cur) openDetailOf(sel); else handled = false; }
    else if (k === 'Escape') {
      if (cur && detailOpen(sel)) openDetailOf(sel);
      else if (cur && kids(cur).length && !collapsed.has(sel)) toggleCollapse(cur, true);
      else if (filter) { set('kfilter', ''); const b = document.getElementById('kbar'); if (b) b.remove(); }
      else if (cur) select(null);
      else handled = false;
    } else if (k === 'l') {
      const sub = cur && cur.querySelector(':scope > details.sub');
      if (sub && !sub.open) sub.open = true;
      else if (cur && collapsed.has(sel)) toggleCollapse(cur, false);
      else if (cur && kids(cur)[0]) select(kids(cur)[0]);
    } else if (k === 'h') {
      const sub = cur && cur.querySelector(':scope > details.sub');
      if (sub && sub.open) sub.open = false;
      else if (cur && kids(cur).length && !collapsed.has(sel)) toggleCollapse(cur, true);
      else if (cur) { const p = cur.parentElement.closest('li.task'); if (p) select(p); }
    } else if (cur && 'qecsadxD'.includes(k) && k.length === 1) {
      if (!detailOpen(sel)) { openDetailOf(sel); setTimeout(() => press(k), 400); } else press(k);
    } else handled = false;
    if (handled) e.preventDefault();
  });

})();
