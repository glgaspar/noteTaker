// Collapsible folder tree. The server renders folders as a flat list in tree order with
// data-depth, already collapsed according to the "expanded" cookie (ids of open folders),
// so toggling only has to update the cookie and hide or show the rows below a folder.

function load() {
  const value = document.cookie.split("; ").find((c) => c.startsWith("expanded="));
  return new Set(value ? value.slice(9).split(".").filter(Boolean) : []);
}

function save(set) {
  document.cookie = `expanded=${[...set].join(".")}; path=/; max-age=31536000; samesite=lax`;
}

function render(tree, expanded) {
  let hideBelow = Infinity;
  for (const li of tree.querySelectorAll("li[data-folder]")) {
    const depth = Number(li.dataset.depth);
    if (depth > hideBelow) {
      li.hidden = true;
      continue;
    }
    li.hidden = false;
    const open = expanded.has(li.dataset.folder);
    hideBelow = li.hasAttribute("data-parent") && !open ? depth : Infinity;
    li.querySelector("[data-toggle] svg")?.classList.toggle("rotate-90", open);
  }
}

export function initTree(root) {
  const tree = root.querySelector?.("#folder-tree");
  if (!tree) return;
  // Folders the server opened because the current folder is inside them stay open, so
  // leaving the folder (All notes, a tag, ...) does not collapse the tree.
  const expanded = load();
  const before = expanded.size;
  for (const row of tree.querySelectorAll("li[data-parent]")) {
    if (row.querySelector("[data-toggle] svg.rotate-90")) expanded.add(row.dataset.folder);
  }
  if (expanded.size !== before) save(expanded);
  tree.addEventListener("click", (e) => {
    const toggle = e.target.closest("[data-toggle]");
    if (!toggle) return;
    const id = toggle.closest("li").dataset.folder;
    expanded.has(id) ? expanded.delete(id) : expanded.add(id);
    save(expanded);
    render(tree, expanded);
  });
}
