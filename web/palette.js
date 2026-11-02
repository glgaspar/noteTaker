// Command palette: Ctrl/Cmd+K or any [data-palette] button. Results come from
// /w/{id}/palette; arrow keys move between [data-option] rows and Enter opens one.

let timer = null;

function options(dialog) {
  return [...dialog.querySelectorAll("[data-option]")].filter((o) => o.offsetParent);
}

function select(dialog, index) {
  const all = options(dialog);
  all.forEach((o, i) => o.classList.toggle("bg-hover", i === index));
  all[index]?.scrollIntoView({ block: "nearest" });
  dialog.dataset.selected = String(index);
}

async function load(dialog, query) {
  const res = await fetch(`/w/${dialog.dataset.workspace}/palette?q=${encodeURIComponent(query)}`);
  if (!res.ok || !dialog.open) return;
  dialog.querySelector("[data-palette-results]").innerHTML = await res.text();
  select(dialog, 0);
}

function open() {
  const dialog = document.getElementById("palette");
  if (!dialog || dialog.open) return;
  const input = dialog.querySelector("[data-palette-input]");
  input.value = "";
  dialog.showModal();
  load(dialog, "");
}

// Show the shortcut as Macs write it.
export function labelShortcuts(root) {
  if (/Mac|iPhone|iPad/.test(navigator.platform)) root.querySelectorAll?.("[data-shortcut]").forEach((el) => (el.textContent = "⌘K"));
}

document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    open();
  }
});

document.addEventListener("click", (e) => {
  if (e.target.closest("[data-palette]")) open();
  if (e.target.closest("#palette [data-option]")) document.getElementById("palette").close();
});

document.addEventListener("input", (e) => {
  if (!e.target.matches("[data-palette-input]")) return;
  const dialog = e.target.closest("dialog");
  clearTimeout(timer);
  timer = setTimeout(() => load(dialog, e.target.value.trim()), 120);
});

document.addEventListener("keydown", (e) => {
  if (!e.target.matches?.("[data-palette-input]")) return;
  const dialog = e.target.closest("dialog");
  const index = Number(dialog.dataset.selected || 0);
  const count = options(dialog).length;
  if (e.key === "ArrowDown" || e.key === "ArrowUp") {
    e.preventDefault();
    select(dialog, (index + (e.key === "ArrowDown" ? 1 : -1) + count) % Math.max(count, 1));
  } else if (e.key === "Enter") {
    e.preventDefault();
    options(dialog)[index]?.click();
  }
});
