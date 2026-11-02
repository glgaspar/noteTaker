// Browser end-to-end checks against a running server.
//   E2E_URL (default http://localhost:8080), E2E_ADMIN_EMAIL / E2E_ADMIN_PASSWORD (an existing
//   admin), CHROME (path to Chrome or Chromium; default /usr/bin/google-chrome).
// It creates its own workspace and a second user through the UI.
import puppeteer from "puppeteer-core";

const B = process.env.E2E_URL || "http://localhost:8080";
const ADMIN = process.env.E2E_ADMIN_EMAIL || "admin@example.com";
const ADMIN_PW = process.env.E2E_ADMIN_PASSWORD || "adminpass123";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const errors = [];
let failed = 0;

function check(name, ok, detail = "") {
  console.log(`${ok ? "ok  " : "FAIL"} ${name}${ok ? "" : ` ${detail}`}`);
  if (!ok) failed++;
}

const browser = await puppeteer.launch({
  executablePath: process.env.CHROME || "/usr/bin/google-chrome",
  headless: true,
  args: ["--no-sandbox"],
});

async function page() {
  const p = await (await browser.createBrowserContext()).newPage();
  p.on("pageerror", (e) => errors.push(e.message));
  p.on("dialog", (d) => d.accept());
  await p.setViewport({ width: 1400, height: 900 });
  return p;
}

async function login(p, email, password) {
  await p.goto(`${B}/login`);
  await p.type("input[name=email]", email);
  await p.type("input[name=password]", password);
  await Promise.all([p.waitForNavigation(), p.click("button")]);
}

// Text of the editor without the collaborators' cursor labels.
const text = (p) =>
  p.$eval(".tiptap", (e) => {
    const c = e.cloneNode(true);
    c.querySelectorAll(".collaboration-carets__caret").forEach((x) => x.remove());
    return c.textContent;
  });

try {
  const admin = await page();
  await login(admin, ADMIN, ADMIN_PW);
  check("admin logs in", !admin.url().includes("/login"), admin.url());

  // A second user, invited through the admin page (the link is shown without SMTP).
  const email = `e2e-${Date.now()}@example.com`;
  await admin.goto(`${B}/admin/users`);
  await admin.type("form[action='/admin/invites'] input", email);
  await Promise.all([admin.waitForNavigation({ waitUntil: "networkidle0" }).catch(() => {}), admin.click("form[action='/admin/invites'] button")]);
  await sleep(500);
  const link = await admin.$eval("input[readonly]", (e) => e.value).catch(() => null);
  check("invite link shown", !!link, "(configure no SMTP for this test, or the link is emailed)");
  const bob = await page();
  await bob.goto(link);
  await bob.type("input[name=name]", "E2E Bob");
  await bob.type("input[name=password]", "e2e-password");
  await Promise.all([bob.waitForNavigation(), bob.click("button")]);

  // Workspace shared with Bob, and a note both open.
  await admin.goto(`${B}/`);
  await admin.evaluate(() => (document.querySelector("details").open = true));
  await admin.type("form[action='/workspaces'] input", `E2E ${Date.now()}`);
  await Promise.all([admin.waitForNavigation(), admin.click("form[action='/workspaces'] button")]);
  const ws = admin.url().match(/\/w\/(\d+)/)[1];
  await admin.goto(`${B}/w/${ws}/members`);
  await admin.type("input[name=email]", email);
  await Promise.all([admin.waitForNavigation(), admin.click("form[action$='/members'] button")]);
  await admin.goto(`${B}/w/${ws}`);
  await Promise.all([admin.waitForNavigation(), admin.click("form[action$='/notes'] button")]);
  await admin.waitForSelector(".ProseMirror");
  const noteURL = admin.url();
  await bob.goto(noteURL);
  await bob.waitForSelector(".ProseMirror");
  await sleep(800);
  check("presence shows the other user", (await admin.$eval("#presence", (e) => e.textContent)).includes("E2E Bob"));

  // Live co-editing converges; titles merge.
  await admin.click(".tiptap");
  await admin.keyboard.type("Hello from admin.");
  await sleep(600);
  check("edits appear live", (await text(bob)).includes("Hello from admin."));
  await bob.click(".tiptap");
  await bob.keyboard.press("End");
  await Promise.all([admin.keyboard.type(" A", { delay: 30 }), bob.keyboard.type(" B", { delay: 30 })]);
  await sleep(800);
  check("concurrent edits converge", (await text(admin)) === (await text(bob)), `${await text(admin)} / ${await text(bob)}`);
  await admin.click("#note-title");
  await admin.keyboard.type("E2E note");
  await sleep(400);
  await bob.click("#note-title");
  await bob.keyboard.press("End");
  await bob.keyboard.type(" (draft)");
  await sleep(800);
  const titles = [await admin.$eval("#note-title", (e) => e.value), await bob.$eval("#note-title", (e) => e.value)];
  check("titles merge", titles[0] === "E2E note (draft)" && titles[1] === titles[0], titles.join(" / "));

  // Pasting an image uploads it (skipped when file storage is off).
  const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC";
  await admin.click(".tiptap");
  await admin.evaluate(async (b64) => {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    const dt = new DataTransfer();
    dt.items.add(new File([bytes], "dot.png", { type: "image/png" }));
    document.querySelector(".ProseMirror").dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
  }, png);
  await sleep(1200);
  const status = await admin.$eval("#save-status", (e) => e.textContent);
  if (status.includes("not configured")) console.log("skip paste image (file storage off)");
  else check("pasted image is shown to both", (await bob.$$(".tiptap img")).length === 1, status);

  // Saved content is searchable.
  await sleep(1500);
  await admin.goto(`${B}/w/${ws}/search`);
  await admin.type("section input[name=q]", "hello admin", { delay: 30 });
  await sleep(1000);
  const results = await admin.$$eval("#results .font-medium", (e) => e.map((x) => x.textContent));
  check("live search finds the note", results.includes("E2E note (draft)"), JSON.stringify(results));
} catch (e) {
  check("run", false, e.stack);
} finally {
  check("no JavaScript errors", errors.length === 0, errors.join("; "));
  await browser.close();
}
process.exit(failed ? 1 : 0);
