import htmx from "htmx.org";
import { mountEditor } from "./editor.js";

window.htmx = htmx;

// Tell the server our time zone, for timestamps and date filters.
const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
if (tz && !document.cookie.split("; ").includes(`tz=${encodeURIComponent(tz)}`)) {
  document.cookie = `tz=${encodeURIComponent(tz)}; path=/; max-age=31536000; samesite=lax`;
}
// Always fetch pages fresh on back/forward so the editor never shows a stale snapshot.
htmx.config.historyCacheSize = 0;
htmx.config.refreshOnHistoryMiss = true;

let editor = null;

htmx.onLoad((root) => {
  const el = root.querySelector?.("#editor") || (root.id === "editor" ? root : null);
  if (el) editor = mountEditor(el);
});

// Save pending changes before htmx navigates away, and before the page unloads.
document.addEventListener("htmx:beforeRequest", () => editor?.flush(false));
document.addEventListener("htmx:beforeSwap", (e) => {
  // partial swaps (e.g. the tag bar) leave the editor in place
  if (editor && e.detail.target.contains(editor.el)) {
    editor.destroy();
    editor = null;
  }
});
window.addEventListener("pagehide", () => {
  editor?.flush(true);
  // Close the socket so others drop our cursor; the browser may keep this page in its back/forward cache.
  editor?.destroy();
  editor = null;
});
window.addEventListener("pageshow", (e) => {
  if (e.persisted) location.reload();
});
