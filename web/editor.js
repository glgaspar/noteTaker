import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import CodeBlockLowlight from "@tiptap/extension-code-block-lowlight";
import { TaskList, TaskItem } from "@tiptap/extension-list";
import { Placeholder } from "@tiptap/extensions";
import Collaboration from "@tiptap/extension-collaboration";
import CollaborationCaret from "@tiptap/extension-collaboration-caret";
import { common, createLowlight } from "lowlight";
import * as Y from "yjs";
import { NoteProvider } from "./collab.js";

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
};

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

  const doc = new Y.Doc();
  const meta = doc.getMap("meta");
  const provider = new NoteProvider(id, doc, {
    onSynced: (seed) => {
      if (!seed) return;
      const json = JSON.parse(el.dataset.content || "null");
      if (title.value) meta.set("title", title.value);
      if (json) editor.commands.setContent(json);
    },
    onStatus: (s) => (status.textContent = s === "offline" ? "Offline, reconnecting…" : ""),
  });

  const editor = new Editor({
    element: el,
    editable: el.dataset.editable === "true",
    extensions: [
      StarterKit.configure({ codeBlock: false, undoRedo: false }),
      CodeBlockLowlight.configure({ lowlight }),
      TaskList,
      TaskItem.configure({ nested: true }),
      Placeholder.configure({ placeholder: "Start writing…" }),
      Collaboration.configure({ document: doc }),
      CollaborationCaret.configure({
        provider,
        user: { name: el.dataset.userName, color: colors[el.dataset.userId % colors.length] },
      }),
    ],
    onSelectionUpdate: syncLanguage,
  });

  doc.on("update", (_, origin) => {
    if (origin !== provider) schedule();
  });
  meta.observe((e) => {
    if (!e.transaction.local) title.value = meta.get("title") ?? "";
  });
  provider.awareness.on("change", () => {
    const names = [...provider.awareness.getStates()]
      .filter(([clientId, state]) => clientId !== doc.clientID && state.user)
      .map(([, state]) => state.user.name);
    presence.textContent = names.length ? `Also here: ${[...new Set(names)].join(", ")}` : "";
  });

  function save(keepalive = false) {
    clearTimeout(timer);
    timer = null;
    status.textContent = "Saving…";
    return fetch(`/notes/${id}`, {
      method: "PUT",
      keepalive,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title: title.value, content: editor.getJSON(), text: editor.getText() }),
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

  function syncLanguage() {
    if (!language) return;
    const lang = editor.isActive("codeBlock") ? editor.getAttributes("codeBlock").language || "" : "";
    // aliases typed in markdown (```js) are not in the list yet
    if (![...language.options].some((o) => o.value === lang)) language.add(new Option(lang, lang));
    language.value = lang;
  }

  title.addEventListener("input", () => meta.set("title", title.value));
  title.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      editor.commands.focus("start");
    }
  });

  if (toolbar) {
    toolbar.addEventListener("click", (e) => {
      const b = e.target.closest("[data-cmd]");
      if (b) commands[b.dataset.cmd](editor.chain().focus()).run();
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
