import htmx from "htmx.org";
import { mountEditor } from "./editor.js";

window.htmx = htmx;
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
document.addEventListener("htmx:beforeSwap", () => {
  editor?.destroy();
  editor = null;
});
window.addEventListener("pagehide", () => editor?.flush(true));
