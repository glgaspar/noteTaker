// Collapsible folder tree. The server renders folders as a flat list in tree order with
// data-depth; collapsing a folder hides the rows below it with a greater depth. Expanded
// folders are remembered per browser; the current folder's ancestors always open.

const KEY = "expanded-folders";

function load() {
  try {
    return new Set(JSON.parse(localStorage.getItem(KEY) || "[]"));
  } catch {
    return new Set();
  }
}

function save(set) {
  try {
    localStorage.setItem(KEY, JSON.stringify([...set]));
  } catch {}
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
    const chevron = li.querySelector("[data-toggle] svg");
    if (chevron) chevron.style.transform = open ? "rotate(90deg)" : "";
  }
}

export function initTree(root) {
  const tree = root.querySelector?.("#folder-tree");
  if (!tree) return;
  const expanded = load();
  // open the ancestors of the current folder
  const rows = [...tree.querySelectorAll("li[data-folder]")];
  const current = rows.findIndex((li) => li.hasAttribute("data-current"));
  for (let i = current - 1, depth = Number(rows[current]?.dataset.depth); i >= 0 && depth > 0; i--) {
    if (Number(rows[i].dataset.depth) < depth) {
      expanded.add(rows[i].dataset.folder);
      depth = Number(rows[i].dataset.depth);
    }
  }
  render(tree, expanded);
  tree.addEventListener("click", (e) => {
    const toggle = e.target.closest("[data-toggle]");
    if (!toggle) return;
    const id = toggle.closest("li").dataset.folder;
    expanded.has(id) ? expanded.delete(id) : expanded.add(id);
    save(expanded);
    render(tree, expanded);
  });
}
