import htmx from "htmx.org";
import { mountEditor } from "./editor.js";
import { initUI } from "./ui.js";
import { initTree } from "./tree.js";
import { labelShortcuts } from "./palette.js";

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
  initUI();
  initTree(root);
  labelShortcuts(root);
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
// Keep the sidebar's scroll position across navigations, and the note list's while staying
// in the same folder or tag (e.g. moving between notes).
const listKey = (url) => {
  const q = new URL(url, location.href).searchParams;
  return `${new URL(url, location.href).pathname}?${q.get("folder")}&${q.get("tag")}`;
};
let scrolls = null;
document.addEventListener("htmx:beforeSwap", (e) => {
  if (!e.detail.boosted) return;
  scrolls = {
    sidebar: document.querySelector("#sidebar .overflow-y-auto")?.scrollTop,
    list: document.querySelector("#list-pane ul")?.scrollTop,
    key: listKey(location.href),
  };
});
document.addEventListener("htmx:afterSwap", (e) => {
  if (!scrolls || !e.detail.boosted) return;
  const sidebar = document.querySelector("#sidebar .overflow-y-auto");
  if (sidebar) sidebar.scrollTop = scrolls.sidebar || 0;
  const list = document.querySelector("#list-pane ul");
  if (list && listKey(e.detail.pathInfo?.finalRequestPath || location.href) === scrolls.key) list.scrollTop = scrolls.list || 0;
  scrolls = null;
});

// Error pages (404, 403, ...) replace the page on navigation; htmx skips error responses by
// default. Partial updates keep ignoring them.
document.addEventListener("htmx:beforeSwap", (e) => {
  if (e.detail.xhr.status >= 400 && e.detail.boosted) {
    e.detail.shouldSwap = true;
    e.detail.isError = false;
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
