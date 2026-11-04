// Escape sequences for the on-screen key bar. Pure, so it is unit-tested under node
// (term_keys_test.go). Arrows/Home/End use SS3 (ESC O x) when the app has set DECCKM
// (application cursor keys); with a modifier they stay CSI 1;m x in both modes.
'use strict';
(function (root) {
  function csi(final, num, m, appCursor) {
    if (m > 1) return '\x1b[' + (num || 1) + ';' + m + final;
    if (!num && appCursor && 'ABCDHF'.indexOf(final) >= 0) return '\x1bO' + final;
    return '\x1b[' + (num || '') + final;
  }
  // Replies the terminal engine generates while replayed history is written (DA, cursor
  // position, ...) answer queries from the past; forwarding them types stray bytes into the
  // shell. The engine calls onData synchronously inside write(), so drop onData calls made
  // during a write until the server's live marker arrives. Typed input is never inside write().
  function replyGate() {
    let replaying = true, writing = false;
    return {
      onData: (fn) => (d) => { if (!(replaying && writing)) fn(d); },
      write(w, bytes) { writing = true; try { w(bytes); } finally { writing = false; } },
      live() { replaying = false; },
      replay() { replaying = true; },
    };
  }
  root.wtermKeys = { csi, replyGate };
  if (typeof module !== 'undefined') module.exports = root.wtermKeys;
})(typeof window !== 'undefined' ? window : globalThis);
