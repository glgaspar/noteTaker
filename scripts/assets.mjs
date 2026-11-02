// Copies the Inter font and builds an SVG sprite of the Lucide icons the app uses.
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";

const icons = [
  "search", "file-text", "upload", "folder", "folder-open", "chevron-right", "plus", "ellipsis", "hash", "users",
  "log-out", "sun", "moon", "monitor", "shield", "trash-2", "pencil", "folder-input", "share-2", "link", "check",
  "cloud", "cloud-off", "loader-circle", "bold", "italic", "strikethrough", "code", "heading-1", "heading-2",
  "heading-3", "list", "list-ordered", "list-todo", "text-quote", "square-code", "paperclip", "image", "undo-2",
  "redo-2", "minus", "arrow-left", "menu", "x", "notebook-pen", "file-plus", "folder-plus", "command", "pilcrow",
  "chevrons-up-down", "settings", "user-plus", "key-round", "user-x", "user-check", "tag", "mail", "inbox",
];

mkdirSync("static/dist/fonts", { recursive: true });
for (const subset of ["latin", "latin-ext"]) {
  copyFileSync(`node_modules/@fontsource-variable/inter/files/inter-${subset}-wght-normal.woff2`, `static/dist/fonts/inter-${subset}.woff2`);
}

const symbols = icons.map((name) => {
  const svg = readFileSync(`node_modules/lucide-static/icons/${name}.svg`, "utf8");
  const body = svg.slice(svg.indexOf(">", svg.indexOf("<svg")) + 1, svg.lastIndexOf("</svg>")).trim();
  return `<symbol id="${name}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${body}</symbol>`;
});
writeFileSync("static/dist/icons.svg", `<svg xmlns="http://www.w3.org/2000/svg">${symbols.join("")}</svg>`);
