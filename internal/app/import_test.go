package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// importFiles uploads files to the import form and waits for the background job to finish.
func (u *user) importFiles(ws int64, files map[string][]byte) string {
	u.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, data := range files {
		w, _ := mw.CreateFormFile("files", name)
		w.Write(data)
	}
	mw.WriteField("folder_id", "")
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/w/%d/import", u.base, ws), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res := u.do(req)
	if res.code != http.StatusSeeOther {
		u.t.Fatalf("import = %d %.200s", res.code, res.body)
	}
	for range 100 {
		page := u.get(res.location)
		if !strings.Contains(page.body, "Importing…") {
			return page.body
		}
		time.Sleep(50 * time.Millisecond)
	}
	u.t.Fatal("import did not finish")
	return ""
}

const enexFixture = `<?xml version="1.0" encoding="UTF-8"?>
<en-export><note><title>Reunião</title><created>20240105T120000Z</created><updated>20240210T153000Z</updated>
<tag>work</tag><content><![CDATA[<en-note><div><en-todo checked="true"/>Comprar café</div><div><en-media hash="%s" type="image/png"/></div></en-note>]]></content>
<resource><data encoding="base64">%s</data><mime>image/png</mime><resource-attributes><file-name>pic.png</file-name></resource-attributes></resource>
</note></en-export>`

func TestImportWithoutStorage(t *testing.T) {
	a := newApp(t, false)
	ann := a.user("Ann", false)
	ws := ann.workspace("W")
	page := ann.importFiles(ws, map[string][]byte{
		"Notebook.enex": fmt.Appendf(nil, enexFixture, "d41d8cd98f00b204e9800998ecf8427e", ""),
		"readme.md":     []byte("# Setup\n\n- [x] clone\n"),
		"doc.docx":      []byte("PK"),
	})
	for _, want := range []string{"Imported 2 notes", "Unsupported files: doc.docx"} {
		if !strings.Contains(page, want) {
			t.Errorf("result page lacks %q", want)
		}
	}
	var html, text string
	var updated time.Time
	if err := testDB.QueryRow(context.Background(), `SELECT import_html, content_text, updated_at FROM notes WHERE title = 'Reunião' AND workspace_id = $1`, ws).
		Scan(&html, &text, &updated); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `data-checked="true"><p>Comprar café</p>`) || !strings.Contains(text, "Comprar café") {
		t.Errorf("checklist not imported: %s / %s", html, text)
	}
	if updated.Year() != 2024 {
		t.Errorf("original date lost: %v", updated)
	}
	if got := ann.search(ws, "q=cafe"); fmt.Sprint(got) != "[Reunião]" {
		t.Errorf("imported note not searchable: %v", got)
	}
	// Opening the note seeds it; that first save must not count as an edit.
	id := dbInt(t, `SELECT id FROM notes WHERE title = 'Reunião' AND workspace_id = $1`, ws)
	ann.putJSON(fmt.Sprintf("/notes/%d", id), map[string]any{"title": "Reunião", "content": map[string]any{"type": "doc"}, "text": "x", "seed": true})
	if dbInt(t, `SELECT extract(year FROM updated_at)::bigint FROM notes WHERE id = $1`, id) != 2024 {
		t.Error("seeding changed the note's date")
	}
	if dbInt(t, `SELECT count(*) FROM notes WHERE id = $1 AND import_html IS NULL`, id) != 1 {
		t.Error("import HTML not cleared after the first save")
	}
}

var fileURL = regexp.MustCompile(`/files/(\d+)`)

func TestImportZipWithStorage(t *testing.T) {
	a := newApp(t, true)
	ann := a.user("Ann", false)
	other := a.user("Other", false)
	ws := ann.workspace("W")
	png := []byte("\x89PNG\r\n\x1a\nfake")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string]string{
		"Vault/Daily/day.md":             "![a](../img/pic%20one.png) ![[logo.png]] [doc](../img/spec.pdf) [other](../Home.md)",
		"Vault/img/pic one.png":          string(png),
		"Vault/assets/logo.png":          string(png),
		"Vault/img/spec.pdf":             "%PDF",
		"Vault/Home.md":                  "![x](data:image/png;base64," + base64.StdEncoding.EncodeToString(png) + ")",
		"Vault/.obsidian/workspace.json": "{}",
	} {
		w, _ := zw.Create(name)
		io.WriteString(w, data)
	}
	zw.Close()
	page := ann.importFiles(ws, map[string][]byte{"vault.zip": buf.Bytes()})
	if !strings.Contains(page, "Imported 2 notes and 4 attachments") {
		t.Fatalf("unexpected result: %s", regexp.MustCompile(`Imported[^<]*`).FindString(page))
	}
	if n := dbInt(t, `SELECT count(*) FROM folders WHERE workspace_id = $1 AND name IN ('vault', 'Daily')`, ws); n != 2 {
		t.Errorf("expected folders vault/Daily, got %d", n)
	}
	var day string
	testDB.QueryRow(context.Background(), `SELECT import_html FROM notes WHERE title = 'day' AND workspace_id = $1`, ws).Scan(&day)
	ids := fileURL.FindAllStringSubmatch(day, -1)
	if len(ids) != 3 || strings.Contains(day, "Home.md") {
		t.Fatalf("day note links: %s", day)
	}
	if res := ann.get("/files/" + ids[0][1]); res.code != 200 || res.header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("owner fetch = %d, CSP %q", res.code, res.header.Get("Content-Security-Policy"))
	}
	if res := other.get("/files/" + ids[0][1]); res.code != 404 {
		t.Errorf("outsider fetch = %d", res.code)
	}

	// Deleting the note leaves files in the bucket until the sweep removes them.
	id := dbInt(t, `SELECT id FROM notes WHERE title = 'day' AND workspace_id = $1`, ws)
	ann.post(fmt.Sprintf("/notes/%d/delete", id), nil)
	removed, err := SweepFiles(context.Background(), testDB, testFiles)
	if err != nil || removed < 3 {
		t.Errorf("sweep removed %d (err %v), want at least 3", removed, err)
	}
	if removed, _ := SweepFiles(context.Background(), testDB, testFiles); removed != 0 {
		t.Errorf("second sweep removed %d live files", removed)
	}
}

func TestStaticAssets(t *testing.T) {
	a := newApp(t, false)
	c := a.client()
	path := a.s.assetURL("/static/dist/app.js")
	req, _ := http.NewRequest("GET", a.url+path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := c.do(req)
	if res.code != 200 || res.header.Get("Content-Encoding") != "gzip" || !strings.Contains(res.header.Get("Cache-Control"), "immutable") {
		t.Errorf("versioned asset: %d %q %q", res.code, res.header.Get("Content-Encoding"), res.header.Get("Cache-Control"))
	}
	if res := c.get("/static/dist/app.js"); res.header.Get("Cache-Control") != "no-cache" || res.header.Get("Content-Encoding") != "" {
		t.Errorf("unversioned asset: %q %q", res.header.Get("Cache-Control"), res.header.Get("Content-Encoding"))
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestImportLimits(t *testing.T) {
	if _, err := readDocument(io.LimitReader(zeros{}, maxDocument+1)); err == nil {
		t.Error("document over the limit accepted")
	}
	if src, err := readDocument(strings.NewReader("# small")); err != nil || string(src) != "# small" {
		t.Errorf("small document: %q %v", src, err)
	}

	// One import at a time per user.
	a := newApp(t, false)
	ann := a.user("Ann", false)
	ws := ann.workspace("W")
	var running int64
	testDB.QueryRow(context.Background(), `INSERT INTO import_jobs (workspace_id, user_id) VALUES ($1, $2) RETURNING id`, ws, ann.ID).Scan(&running)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("files", "a.txt")
	w.Write([]byte("hello"))
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/w/%d/import", ann.base, ws), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if res := ann.do(req); res.location != fmt.Sprintf("/w/%d/imports/%d", ws, running) {
		t.Errorf("second import went to %q", res.location)
	}
}
