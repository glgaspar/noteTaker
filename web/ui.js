// Theme, popover menus and dialogs.

// ---- theme: "light", "dark" or unset (follow the system) ----

const darkQuery = matchMedia("(prefers-color-scheme: dark)");

function storedTheme() {
  try {
    return localStorage.getItem("theme");
  } catch {
    return null;
  }
}

function applyTheme() {
  const theme = storedTheme();
  document.documentElement.classList.toggle("dark", theme === "dark" || (theme !== "light" && darkQuery.matches));
  document.querySelectorAll("[data-theme]").forEach((b) => b.setAttribute("aria-checked", String((b.dataset.theme || null) === theme)));
}

darkQuery.addEventListener("change", applyTheme);
document.addEventListener("click", (e) => {
  const button = e.target.closest("[data-theme]");
  if (!button) return;
  try {
    if (button.dataset.theme) localStorage.setItem("theme", button.dataset.theme);
    else localStorage.removeItem("theme");
  } catch {}
  applyTheme();
});

// ---- popover menus: <button popovertarget="x"> + <div id="x" popover class="menu"> ----

let invoker = null;
document.addEventListener("click", (e) => (invoker = e.target.closest("[popovertarget]") || invoker), true);

// Shared menus (one element for every folder row) are filled from the invoker's data-menu
// attributes: elements with data-fill="href,action" get {id} and {name} replaced, and
// elements with data-requires="edit" are hidden unless the invoker has data-menu-edit.
function fillMenu(menu, from) {
  const values = { id: from.dataset.menuId || "", name: from.dataset.menuName || "" };
  for (const el of menu.querySelectorAll("[data-fill]")) {
    for (const attr of el.dataset.fill.split(",")) {
      const key = `tpl${attr.replace(/(^|-)(\w)/g, (_, __, c) => c.toUpperCase())}`;
      el.dataset[key] ??= el.getAttribute(attr) || "";
      el.setAttribute(attr, el.dataset[key].replace(/\{(id|name)\}/g, (_, k) => values[k]));
    }
  }
  for (const el of menu.querySelectorAll("[data-requires]")) {
    el.hidden = !from.hasAttribute(`data-menu-${el.dataset.requires}`);
  }
}

document.addEventListener(
  "toggle",
  (e) => {
    const menu = e.target;
    if (!menu.classList?.contains("menu") || e.newState !== "open" || !invoker) return;
    if (menu.hasAttribute("data-shared")) fillMenu(menu, invoker);
    const r = invoker.getBoundingClientRect();
    const w = menu.offsetWidth;
    const h = menu.offsetHeight;
    let top = r.bottom + 4;
    if (top + h > innerHeight - 8) top = Math.max(8, r.top - h - 4);
    let left = menu.dataset.align === "end" ? r.right - w : r.left;
    left = Math.min(Math.max(8, left), innerWidth - w - 8);
    menu.style.top = `${top}px`;
    menu.style.left = `${left}px`;
  },
  true,
);

export function closeMenus() {
  document.querySelectorAll(".menu:popover-open").forEach((m) => m.hidePopover());
}

// ---- dialogs ----

// Clicking the backdrop closes a dialog; [data-close] buttons too.
document.addEventListener("click", (e) => {
  if (e.target.tagName === "DIALOG") e.target.close();
  if (e.target.closest("[data-close]")) e.target.closest("dialog")?.close();
});

// openPrompt shows the prompt dialog. With action it posts a form (name=<value> plus fields);
// with onSubmit it calls back with the value instead.
export function openPrompt({ title, label = "", value = "", submit = "Save", action = "", fields = {}, onSubmit, placeholder = "" }) {
  const dialog = document.getElementById("prompt-dialog");
  const form = dialog.querySelector("form");
  dialog.querySelector("[data-title]").textContent = title;
  dialog.querySelector("[data-label]").textContent = label;
  dialog.querySelector("[data-submit]").textContent = submit;
  const input = form.elements.name;
  input.value = value;
  input.placeholder = placeholder;
  form.action = action;
  const hidden = dialog.querySelector("[data-fields]");
  hidden.replaceChildren(
    ...Object.entries(fields).map(([name, v]) => Object.assign(document.createElement("input"), { type: "hidden", name, value: v })),
  );
  form.onsubmit = onSubmit
    ? (e) => {
        e.preventDefault();
        dialog.close();
        onSubmit(input.value);
      }
    : null;
  closeMenus();
  dialog.showModal();
  input.select();
}

// Elements with data-prompt-action (or data-prompt-title for callbacks) open the prompt.
// data-prompt-field-<name> become hidden fields.
document.addEventListener("click", (e) => {
  const el = e.target.closest("[data-prompt-title]");
  if (!el || el.dataset.promptJs) return;
  e.preventDefault();
  const fields = {};
  for (const [k, v] of Object.entries(el.dataset)) {
    if (k.startsWith("promptField")) fields[k.slice(11).replace(/^./, (c) => c.toLowerCase()).replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`)] = v;
  }
  openPrompt({
    title: el.dataset.promptTitle,
    label: el.dataset.promptLabel,
    value: el.dataset.promptValue,
    submit: el.dataset.promptSubmit,
    placeholder: el.dataset.promptPlaceholder,
    action: el.dataset.promptAction,
    fields,
  });
});

// [data-export-scope] buttons open the export dialog for a folder, workspace or everything.
document.addEventListener("click", (e) => {
  const el = e.target.closest("[data-export-scope]");
  if (!el) return;
  const dialog = document.getElementById("export-dialog");
  const form = dialog.querySelector("form");
  form.elements.scope.value = el.dataset.exportScope;
  form.elements.id.value = el.dataset.exportId || "";
  dialog.querySelector("[data-export-name]").textContent = el.dataset.exportName || "";
  closeMenus();
  dialog.showModal();
});

// confirmDialog resolves to true when the user confirms.
export function confirmDialog(question, confirmLabel = "Delete") {
  const dialog = document.getElementById("confirm-dialog");
  dialog.querySelector("[data-question]").textContent = question;
  const ok = dialog.querySelector("[data-confirm]");
  ok.textContent = confirmLabel;
  closeMenus();
  dialog.showModal();
  return new Promise((resolve) => {
    ok.onclick = () => {
      dialog.close();
      resolve(true);
    };
    dialog.onclose = () => resolve(false);
  });
}

// Forms that htmx does not handle (hx-boost="false") confirm with data-confirm.
document.addEventListener("submit", (e) => {
  const form = e.target;
  if (!form.dataset.confirm || form.dataset.confirmed) return;
  e.preventDefault();
  confirmDialog(form.dataset.confirm, form.dataset.confirmLabel).then((ok) => {
    if (!ok) return;
    form.dataset.confirmed = "1";
    form.requestSubmit();
  });
});

// hx-confirm uses our dialog instead of the browser's.
document.addEventListener("htmx:confirm", (e) => {
  if (!e.detail.question) return;
  e.preventDefault();
  confirmDialog(e.detail.question, e.detail.elt.dataset.confirmLabel).then((ok) => ok && e.detail.issueRequest(true));
});

// ---- small actions; markup has no inline handlers (the CSP forbids them) ----

document.addEventListener("click", (e) => {
  const el = e.target.closest("[data-action]");
  if (!el) return;
  switch (el.dataset.action) {
    case "back":
      history.back();
      break;
    case "print":
      closeMenus();
      print();
      break;
    case "copy-link":
      navigator.clipboard.writeText(location.href);
      closeMenus();
      break;
    case "copy-field": // copies the input just before the button
      navigator.clipboard.writeText(el.previousElementSibling.value);
      el.textContent = "Copied";
      break;
    case "select":
      el.select();
      break;
    case "toggle": // shows or hides the element named by data-target
      document.getElementById(el.dataset.target)?.toggleAttribute("hidden");
      break;
    case "open-dialog":
      closeMenus();
      document.getElementById(el.dataset.dialog)?.showModal();
      break;
  }
});

document.addEventListener("change", (e) => {
  if (e.target.matches("[data-autosubmit]")) e.target.form.requestSubmit();
  if (e.target.matches("[data-file-names]")) {
    const label = e.target.parentElement.querySelector("[data-files]");
    label.dataset.empty ??= label.textContent;
    label.textContent = [...e.target.files].map((f) => f.name).join(", ") || label.dataset.empty;
  }
});

// ---- collapsible columns: [data-toggle-pane="sidebar"|"list"] and Ctrl+\ for both ----

const paneIds = { sidebar: "sidebar", list: "list-pane" };

function savePanes() {
  const hidden = Object.entries(paneIds)
    .filter(([, id]) => document.getElementById(id)?.hasAttribute("data-collapsed"))
    .map(([name]) => name);
  document.cookie = `collapsed=${hidden.join(".")}; path=/; max-age=31536000; samesite=lax`;
}

document.addEventListener("click", (e) => {
  const button = e.target.closest("[data-toggle-pane]");
  if (!button) return;
  document.getElementById(paneIds[button.dataset.togglePane])?.toggleAttribute("data-collapsed");
  savePanes();
});

document.addEventListener("keydown", (e) => {
  if (!(e.ctrlKey || e.metaKey) || e.key !== "\\") return;
  e.preventDefault();
  // The list only collapses next to an open note.
  const panes = [document.getElementById("sidebar"), document.getElementById("note-pane") && document.getElementById("list-pane")].filter(Boolean);
  const hide = panes.some((p) => !p.hasAttribute("data-collapsed"));
  panes.forEach((p) => p.toggleAttribute("data-collapsed", hide));
  savePanes();
});

// ---- sidebar drawer on small screens ----

document.addEventListener("click", (e) => {
  const sidebar = document.getElementById("sidebar");
  if (!sidebar) return;
  if (e.target.closest("[data-drawer-open]")) sidebar.toggleAttribute("data-open", true);
  else if (e.target.closest("[data-drawer-close]") || (e.target.closest("#sidebar a[href]") && innerWidth < 768)) sidebar.removeAttribute("data-open");
});

export function initUI() {
  applyTheme();
}
