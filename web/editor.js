import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import CodeBlockLowlight from "@tiptap/extension-code-block-lowlight";
import { TaskList, TaskItem } from "@tiptap/extension-list";
import { Placeholder } from "@tiptap/extensions";
import Image from "@tiptap/extension-image";
import Collaboration from "@tiptap/extension-collaboration";
import CollaborationCaret from "@tiptap/extension-collaboration-caret";
import { common, createLowlight } from "lowlight";
import * as Y from "yjs";
import { NoteProvider } from "./collab.js";
import { SlashCommands } from "./slash.js";
import { openPrompt } from "./ui.js";

const lowlight = createLowlight(common);

const colors = ["#e11d48", "#2563eb", "#16a34a", "#d97706", "#9333ea", "#0891b2", "#db2777", "#65a30d"];

const commands = {
  bold: (c) => c.toggleBold(),
  italic: (c) => c.toggleItalic(),
  strike: (c) => c.toggleStrike(),
  code: (c) => c.toggleCode(),
  h1: (c) => c.toggleHeading({ level: 1 }),
  h2: (c) => c.toggleHeading({ level: 2 }),
  h3: (c) => c.toggleHeading({ level: 3 }),
  bullet: (c) => c.toggleBulletList(),
  ordered: (c) => c.toggleOrderedList(),
  task: (c) => c.toggleTaskList(),
  quote: (c) => c.toggleBlockquote(),
  codeblock: (c) => c.toggleCodeBlock(),
  undo: (c) => c.undo(),
  redo: (c) => c.redo(),
  attach: (c) => (document.getElementById("attach-input").click(), c),
};

// Which toolbar buttons light up for the formatting at the cursor.
const active = {
  bold: ["bold"],
  italic: ["italic"],
  strike: ["strike"],
  code: ["code"],
  link: ["link"],
  h1: ["heading", { level: 1 }],
  h2: ["heading", { level: 2 }],
  h3: ["heading", { level: 3 }],
  bullet: ["bulletList"],
  ordered: ["orderedList"],
  task: ["taskList"],
  quote: ["blockquote"],
  codeblock: ["codeBlock"],
};

// setLink asks for a URL and links the selection (an empty URL removes the link).
function setLink(editor) {
  openPrompt({
    title: "Link",
    value: editor.getAttributes("link").href || "",
    placeholder: "https://…",
    submit: "Apply",
    onSubmit: (url) => {
      const chain = editor.chain().focus().extendMarkRange("link");
      (url.trim() ? chain.setLink({ href: url.trim() }) : chain.unsetLink()).run();
    },
  });
}

// baseExtensions are the editor features shared by live and static editors.
function baseExtensions() {
  return [
    StarterKit.configure({ codeBlock: false, undoRedo: false }),
    CodeBlockLowlight.configure({ lowlight }),
    TaskList,
    TaskItem.configure({ nested: true }),
    Placeholder.configure({ placeholder: "Start writing…" }),
    Image,
  ];
}

// storedContent is the note content saved on the server (TipTap JSON, or HTML for a fresh
// import), used to seed an empty Yjs doc.
function storedContent(el) {
  return JSON.parse(el.dataset.content || "null") || el.dataset.importHtml || null;
}

// mountEditor turns #editor into a collaborative TipTap editor. The Yjs doc (content and
// title) syncs live through NoteProvider; local edits are also saved as JSON and plain
// text to PUT /notes/{id} for listing and search.
// It returns { flush, destroy } so the page can save pending changes before navigating.
export function mountEditor(el) {
  const id = el.dataset.noteId;
  const title = document.getElementById("note-title");
  const status = document.getElementById("save-status");
  const presence = document.getElementById("presence");
  const toolbar = document.getElementById("toolbar");
  const language = document.getElementById("code-language");
  let timer = null;
  let mounted = false;

  // Viewers cannot seed a note no editor has opened yet, so they get a static copy.
  if (el.dataset.editable !== "true" && el.dataset.seeded !== "true") {
    const editor = new Editor({ element: el, editable: false, content: storedContent(el), extensions: baseExtensions() });
    return { el, flush: () => {}, destroy: () => editor.destroy() };
  }

  const doc = new Y.Doc();
  const ytitle = doc.getText("title"); // shared text, so concurrent title edits merge
  const provider = new NoteProvider(id, doc, {
    onSynced: (seed) => {
      // Notes from before titles were shared text keep it in the "meta" map; move it once.
      const meta = doc.getMap("meta");
      if (ytitle.length === 0 && meta.has("title")) {
        doc.transact(() => {
          ytitle.insert(0, meta.get("title"));
          meta.delete("title");
        });
      }
      if (!seed) return;
      // One transaction, so the server gets content and title together or not at all:
      // a half-seeded note would never be seeded again.
      const content = storedContent(el);
      doc.transact(() => {
        if (content) editor.commands.setContent(content);
        if (title.value) ytitle.insert(0, title.value);
      });
      if (content) save(false, true); // store the JSON right away, without counting it as an edit
    },
    onStatus: (s) => (status.textContent = s === "offline" ? "Offline, reconnecting…" : ""),
  });

  const editor = new Editor({
    element: el,
    editable: el.dataset.editable === "true",
    extensions: [
      ...baseExtensions(),
      Collaboration.configure({ document: doc }),
      SlashCommands,
      CollaborationCaret.configure({
        provider,
        user: { name: el.dataset.userName, color: colors[el.dataset.userId % colors.length] },
      }),
    ],
    onSelectionUpdate: syncToolbar,
    onTransaction: syncToolbar,
    editorProps: {
      handlePaste: (view, event) => insertFiles(event.clipboardData?.files),
      handleDrop: (view, event, slice, moved) =>
        !moved && insertFiles(event.dataTransfer?.files, view.posAtCoords({ left: event.clientX, top: event.clientY })?.pos),
    },
  });

  // insertFiles uploads files and inserts them at pos (default: the cursor): images inline,
  // other files as links. It returns false when there is nothing to upload.
  function insertFiles(fileList, pos) {
    const files = [...(fileList || [])];
    if (!files.length || !editor.isEditable) return false;
    for (const file of files) {
      status.textContent = `Uploading ${file.name}…`;
      const body = new FormData();
      body.append("file", file);
      fetch(`/notes/${id}/attachments`, { method: "POST", body })
        .then(async (r) => (r.ok ? r.json() : Promise.reject(new Error(await r.text()))))
        .then(({ url, name, image }) => {
          const node = image
            ? { type: "image", attrs: { src: url, alt: name } }
            : { type: "paragraph", content: [{ type: "text", text: name, marks: [{ type: "link", attrs: { href: url } }] }] };
          const at = Math.min(pos ?? editor.state.selection.to, editor.state.doc.content.size);
          editor.chain().focus().insertContentAt(at, node).run();
          status.textContent = "";
        })
        .catch((e) => (status.textContent = `Upload failed: ${e.message.trim()}`));
    }
    return true;
  }

  mounted = true;
  syncToolbar();

  doc.on("update", (_, origin) => {
    if (origin !== provider) schedule();
  });
  // Remote title edits: update the input, shifting the caret past text inserted before it.
  ytitle.observe((e) => {
    if (e.transaction.local) return;
    let { selectionStart: start, selectionEnd: end } = title;
    let pos = 0;
    for (const op of e.delta) {
      if (op.retain) pos += op.retain;
      if (op.insert) {
        if (pos <= start) start += op.insert.length;
        if (pos <= end) end += op.insert.length;
        pos += op.insert.length;
      }
      if (op.delete) {
        start -= Math.max(0, Math.min(op.delete, start - pos));
        end -= Math.max(0, Math.min(op.delete, end - pos));
      }
    }
    title.value = ytitle.toString();
    if (document.activeElement === title) title.setSelectionRange(start, end);
  });
  provider.awareness.on("change", () => {
    const seen = new Set();
    const others = [...provider.awareness.getStates()]
      .filter(([clientId, state]) => clientId !== doc.clientID && state.user && !seen.has(state.user.name) && seen.add(state.user.name))
      .map(([, state]) => state.user);
    presence.replaceChildren(
      ...others.map(({ name, color }) =>
        Object.assign(document.createElement("span"), {
          className: "grid size-7 place-items-center rounded-full text-[11px] font-semibold text-white ring-2 ring-[var(--canvas)]",
          textContent: name
            .split(/\s+/)
            .map((w) => w[0])
            .join("")
            .slice(0, 2)
            .toUpperCase(),
          title: `${name} is here`,
          style: `background:${color}`,
        }),
      ),
    );
  });

  function save(keepalive = false, seed = false) {
    clearTimeout(timer);
    timer = null;
    status.textContent = "Saving…";
    return fetch(`/notes/${id}`, {
      method: "PUT",
      keepalive,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title: title.value, content: editor.getJSON(), text: editor.getText(), seed }),
    })
      .then((r) => {
        if (!r.ok) throw new Error(r.statusText);
        status.textContent = "Saved";
        document.querySelectorAll(`[data-note-title="${id}"]`).forEach((e) => (e.textContent = title.value || "Untitled"));
      })
      .catch(() => {
        status.textContent = "Save failed, retrying…";
        timer = setTimeout(save, 3000);
      });
  }

  function schedule() {
    status.textContent = "Unsaved";
    clearTimeout(timer);
    timer = setTimeout(save, 800);
  }

  function syncToolbar() {
    if (!toolbar || !mounted) return; // TipTap calls this while it is still constructing

    for (const b of toolbar.querySelectorAll("[data-cmd]")) {
      const on = Boolean(active[b.dataset.cmd] && editor.isActive(...active[b.dataset.cmd]));
      b.classList.toggle("bg-selected", on);
      b.classList.toggle("text-ink", on);
    }
    const inCode = editor.isActive("codeBlock");
    language.hidden = !inCode;
    toolbar.querySelector("[data-hint]")?.classList.toggle("lg:inline", !inCode);
    if (!inCode) return;
    const lang = editor.isActive("codeBlock") ? editor.getAttributes("codeBlock").language || "" : "";
    // aliases typed in markdown (```js) are not in the list yet
    if (![...language.options].some((o) => o.value === lang)) language.add(new Option(lang, lang));
    language.value = lang;
  }

  // Local title edits: apply only the changed span, so they merge with others'.
  title.addEventListener("input", () => {
    const before = ytitle.toString();
    const after = title.value;
    let start = 0;
    while (start < before.length && start < after.length && before[start] === after[start]) start++;
    let end = 0;
    while (end < before.length - start && end < after.length - start && before.at(-1 - end) === after.at(-1 - end)) end++;
    doc.transact(() => {
      if (before.length - start - end > 0) ytitle.delete(start, before.length - start - end);
      if (after.length - start - end > 0) ytitle.insert(start, after.slice(start, after.length - end));
    });
  });
  title.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      editor.commands.focus("start");
    }
  });

  if (toolbar) {
    const picker = document.getElementById("attach-input");
    picker.addEventListener("change", () => {
      insertFiles(picker.files);
      picker.value = "";
    });
    toolbar.addEventListener("click", (e) => {
      const b = e.target.closest("[data-cmd]");
      if (!b) return;
      if (b.dataset.cmd === "link") return setLink(editor);
      commands[b.dataset.cmd](editor.chain().focus()).run();
    });
    for (const name of lowlight.listLanguages().sort()) language.add(new Option(name, name));
    language.addEventListener("change", () => {
      const attrs = { language: language.value || null };
      const chain = editor.chain().focus();
      (editor.isActive("codeBlock") ? chain.updateAttributes("codeBlock", attrs) : chain.setCodeBlock(attrs)).run();
    });
  }

  return {
    el,
    flush: (keepalive) => timer && save(keepalive),
    destroy: () => {
      editor.destroy();
      provider.destroy();
      doc.destroy();
    },
  };
}
