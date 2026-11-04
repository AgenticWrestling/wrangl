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
  root.wtermKeys = { csi };
  if (typeof module !== 'undefined') module.exports = root.wtermKeys;
})(typeof window !== 'undefined' ? window : globalThis);
