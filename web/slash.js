// "/" menu for inserting blocks, built on TipTap's suggestion utility.
import { Extension } from "@tiptap/core";
import { Suggestion } from "@tiptap/suggestion";

const icon = (name) => `<svg class="icon" aria-hidden="true"><use href="${document.body.dataset.icons}#${name}"/></svg>`;

const items = [
  { title: "Text", hint: "Plain paragraph", icon: "pilcrow", run: (c) => c.setParagraph() },
  { title: "Heading 1", hint: "Large section heading", icon: "heading-1", keys: "h1 title", run: (c) => c.setHeading({ level: 1 }) },
  { title: "Heading 2", hint: "Medium section heading", icon: "heading-2", keys: "h2 subtitle", run: (c) => c.setHeading({ level: 2 }) },
  { title: "Heading 3", hint: "Small section heading", icon: "heading-3", keys: "h3", run: (c) => c.setHeading({ level: 3 }) },
  { title: "Checklist", hint: "Track tasks", icon: "list-todo", keys: "todo task check", run: (c) => c.toggleTaskList() },
  { title: "Bulleted list", hint: "Simple list", icon: "list", keys: "ul bullet unordered", run: (c) => c.toggleBulletList() },
  { title: "Numbered list", hint: "List with numbers", icon: "list-ordered", keys: "ol ordered", run: (c) => c.toggleOrderedList() },
  { title: "Quote", hint: "Quotation", icon: "text-quote", keys: "blockquote", run: (c) => c.toggleBlockquote() },
  { title: "Code block", hint: "With syntax highlighting", icon: "square-code", keys: "code pre snippet", run: (c) => c.setCodeBlock() },
  { title: "Divider", hint: "Horizontal line", icon: "minus", keys: "hr line separator", run: (c) => c.setHorizontalRule() },
  {
    title: "Image or file",
    hint: "Upload from your computer",
    icon: "image",
    keys: "picture attachment upload",
    run: (c) => {
      document.getElementById("attach-input")?.click();
      return c;
    },
  },
];

function filter(query) {
  const q = query.toLowerCase();
  return items.filter((i) => `${i.title} ${i.keys || ""}`.toLowerCase().includes(q));
}

// popup renders the item list next to the caret and handles the keyboard.
function popup() {
  let el, list, selected, props;
  const draw = () => {
    el.innerHTML = list.length
      ? list
          .map(
            (it, i) => `<button type="button" data-i="${i}" class="menu-item h-auto py-1.5 ${i === selected ? "bg-hover" : ""}">
              <span class="grid size-8 shrink-0 place-items-center rounded-md border border-line bg-canvas">${icon(it.icon)}</span>
              <span class="min-w-0"><span class="block text-[13.5px]">${it.title}</span><span class="block text-[12px] text-faint">${it.hint}</span></span>
            </button>`,
          )
          .join("")
      : `<div class="px-2 py-1.5 text-[13px] text-faint">No matching blocks</div>`;
    el.querySelector(`[data-i="${selected}"]`)?.scrollIntoView({ block: "nearest" });
  };
  const place = () => {
    const r = props.clientRect?.();
    if (!r) return;
    const top = r.bottom + 6 + el.offsetHeight > innerHeight ? r.top - el.offsetHeight - 6 : r.bottom + 6;
    el.style.top = `${Math.max(8, top)}px`;
    el.style.left = `${Math.min(r.left, innerWidth - el.offsetWidth - 8)}px`;
  };
  const choose = (i) => list[i] && props.command(list[i]);
  return {
    onStart(p) {
      props = p;
      list = p.items;
      selected = 0;
      el = document.createElement("div");
      el.className = "menu z-50 max-h-80 w-72 overflow-y-auto";
      el.addEventListener("mousedown", (e) => {
        e.preventDefault(); // keep the editor focused
        const b = e.target.closest("[data-i]");
        if (b) choose(Number(b.dataset.i));
      });
      document.body.append(el);
      draw();
      place();
    },
    onUpdate(p) {
      props = p;
      list = p.items;
      selected = 0;
      draw();
      place();
    },
    onKeyDown({ event }) {
      if (event.key === "ArrowDown") selected = (selected + 1) % Math.max(list.length, 1);
      else if (event.key === "ArrowUp") selected = (selected - 1 + list.length) % Math.max(list.length, 1);
      else if (event.key === "Enter") return choose(selected), true;
      else if (event.key === "Escape") return el.remove(), true;
      else return false;
      draw();
      return true;
    },
    onExit() {
      el?.remove();
    },
  };
}

export const SlashCommands = Extension.create({
  name: "slashCommands",
  addProseMirrorPlugins() {
    return [
      Suggestion({
        editor: this.editor,
        char: "/",
        items: ({ query }) => filter(query),
        command: ({ editor, range, props }) => props.run(editor.chain().focus().deleteRange(range)).run(),
        render: popup,
      }),
    ];
  },
});
