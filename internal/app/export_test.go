package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// doc builds a TipTap document: a paragraph per string, plus extra nodes.
func doc(text string, extra ...map[string]any) map[string]any {
	content := []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}}}
	for _, e := range extra {
		content = append(content, e)
	}
	return map[string]any{"type": "doc", "content": content}
}

var (
	checklist = map[string]any{"type": "taskList", "content": []any{
		map[string]any{"type": "taskItem", "attrs": map[string]any{"checked": true}, "content": []any{
			map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "ship it"}}}}}}}
	goCode = map[string]any{"type": "codeBlock", "attrs": map[string]any{"language": "go"},
		"content": []any{map[string]any{"type": "text", "text": "x := 1"}}}
)

func image(src string) map[string]any {
	return map[string]any{"type": "image", "attrs": map[string]any{"src": src, "alt": "pic"}}
}

// noteWith creates a note with TipTap content and tags.
func (u *user) noteWith(ws, folder int64, title string, content map[string]any, tags ...string) int64 {
	u.t.Helper()
	id := u.note(ws, folder, title, "")
	if res := u.putJSON(fmt.Sprintf("/notes/%d", id), map[string]any{"title": title, "content": content, "text": title}); res.code != http.StatusNoContent {
		u.t.Fatalf("save: %d", res.code)
	}
	for _, tag := range tags {
		u.post(fmt.Sprintf("/notes/%d/tags", id), url.Values{"name": {tag}})
	}
	return id
}

// upload attaches a file to a note and returns its /files URL.
func (u *user) upload(noteID int64, name string, data []byte) string {
	u.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", name)
	w.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/notes/%d/attachments", u.base, noteID), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res := u.do(req)
	var out struct{ URL string }
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		u.t.Fatalf("upload: %d %s", res.code, res.body)
	}
	return out.URL
}

func unzip(t *testing.T, data string) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(strings.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		files[f.Name] = string(b)
	}
	return files
}

func names(files map[string]string) []string {
	var out []string
	for name := range files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestExportNote(t *testing.T) {
	a := newApp(t, false)
	ann := a.user("Ann", false)
	ws := ann.workspace("W")
	id := ann.noteWith(ws, 0, `Plan <script>`, doc("Hello", checklist, goCode), "work")

	md := ann.get(fmt.Sprintf("/notes/%d/export?format=md", id))
	if md.code != 200 || !strings.Contains(md.header.Get("Content-Disposition"), `Plan -script-.md`) {
		t.Fatalf("md download: %d %q", md.code, md.header.Get("Content-Disposition"))
	}
	for _, want := range []string{"title: \"Plan \\u003cscript\\u003e\"", `tags: ["work"]`, "Hello", "- [x] ship it", "```go\nx := 1\n```"} {
		if !strings.Contains(md.body, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md.body)
		}
	}
	html := ann.get(fmt.Sprintf("/notes/%d/export?format=html", id))
	if !strings.Contains(html.body, "<!doctype html>") || strings.Contains(html.body, "<script>") || !strings.Contains(html.body, "Plan &lt;script&gt;") {
		t.Errorf("html export: %.300s", html.body)
	}
	if res := a.user("Other", false).get(fmt.Sprintf("/notes/%d/export", id)); res.code != 404 {
		t.Errorf("outsider export = %d", res.code)
	}
}

// waitExport starts an export and returns the finished job page.
func (u *user) waitExport(form url.Values) (string, string) {
	u.t.Helper()
	res := u.post("/exports", form)
	if res.code != http.StatusSeeOther {
		u.t.Fatalf("start export: %d %.200s", res.code, res.body)
	}
	for range 200 {
		page := u.get(res.location)
		if strings.Contains(page.body, "Your export is ready") || strings.Contains(page.body, "export failed") {
			return res.location, page.body
		}
		time.Sleep(25 * time.Millisecond)
	}
	u.t.Fatal("export did not finish")
	return "", ""
}

func TestExportRoundTrip(t *testing.T) {
	a := newApp(t, true)
	ann := a.user("Ann", false)
	ws := ann.workspace("Team")
	work := ann.folder(ws, "Work", 0)
	deep := ann.folder(ws, "Deep", work)
	ann.folder(ws, "Deep", 0) // same name elsewhere: must not merge
	withPic := ann.noteWith(ws, deep, "Diagram", doc("see"), "design")
	pic := ann.upload(withPic, "chart one.png", []byte("\x89PNG\r\n\x1a\nfake"))
	ann.putJSON(fmt.Sprintf("/notes/%d", withPic), map[string]any{"title": "Diagram", "content": doc("see", image(pic)), "text": "see"})
	ann.noteWith(ws, work, "Same", doc("first", checklist))
	ann.noteWith(ws, work, "same", doc("second", goCode)) // differs only in case
	ann.noteWith(ws, 0, "Root note", doc("top"))

	job, page := ann.waitExport(url.Values{"scope": {"workspace"}, "id": {fmt.Sprint(ws)}, "format": {"md"}})
	if !strings.Contains(page, "4 notes and 1 file") {
		t.Fatalf("job page: %s", regexp.MustCompile(`(?s)<h1.*?</p>`).FindString(page))
	}
	dl := ann.get(job + "/download")
	if dl.code != 200 || !strings.Contains(dl.header.Get("Content-Disposition"), "Team.zip") {
		t.Fatalf("download: %d %q", dl.code, dl.header.Get("Content-Disposition"))
	}
	files := unzip(t, dl.body)
	want := []string{"Team/Root note.md", "Team/Work/Deep/Diagram.md", "Team/Work/Deep/attachments/chart one.png", "Team/Work/Same.md", "Team/Work/same (2).md"}
	if fmt.Sprint(names(files)) != fmt.Sprint(want) {
		t.Fatalf("zip entries:\n got %v\nwant %v", names(files), want)
	}
	if !strings.Contains(files["Team/Work/Deep/Diagram.md"], "![pic](attachments/chart%20one.png)") {
		t.Errorf("image link not rewritten:\n%s", files["Team/Work/Deep/Diagram.md"])
	}

	// The export imports back: same titles, tags, structure and the attachment.
	back := ann.workspace("Restored")
	result := ann.importFiles(back, map[string][]byte{"Team.zip": []byte(dl.body)})
	if !strings.Contains(result, "Imported 4 notes and 1 attachment") {
		t.Fatalf("re-import: %s", regexp.MustCompile(`Imported[^<]*`).FindString(result))
	}
	var title string
	var tags []string
	var html string
	err := testDB.QueryRow(context.Background(), `
		SELECT n.title, ARRAY(SELECT t.name FROM note_tags nt JOIN tags t ON t.id = nt.tag_id WHERE nt.note_id = n.id), n.import_html
		FROM notes n JOIN folders f ON f.id = n.folder_id WHERE n.workspace_id = $1 AND f.name = 'Deep'`, back).Scan(&title, &tags, &html)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Diagram" || fmt.Sprint(tags) != "[design]" || !strings.Contains(html, `src="/files/`) {
		t.Errorf("round trip: %q %v %s", title, tags, html)
	}
	var same string
	testDB.QueryRow(context.Background(), `SELECT import_html FROM notes WHERE workspace_id = $1 AND title = 'Same'`, back).Scan(&same)
	if !strings.Contains(same, `data-type="taskItem" data-checked="true"`) {
		t.Errorf("checklist lost in round trip: %s", same)
	}
}

func TestExportAccess(t *testing.T) {
	a := newApp(t, false)
	owner := a.user("Owner", false)
	guest := a.user("Guest", false)
	ws := owner.workspace("W")
	shared := owner.folder(ws, "Shared", 0)
	owner.noteWith(ws, shared, "Visible", doc("ok"))
	owner.noteWith(ws, 0, "Secret", doc("no"))
	owner.share(fmt.Sprintf("/folders/%d/share", shared), guest.Email, "viewer")

	job, _ := guest.waitExport(url.Values{"scope": {"all"}, "format": {"html"}})
	files := unzip(t, guest.get(job+"/download").body)
	if fmt.Sprint(names(files)) != "[W/Shared/Visible.html]" {
		t.Errorf("guest export: %v", names(files))
	}
	if res := owner.get(job); res.code != 404 {
		t.Errorf("someone else's job page = %d", res.code)
	}
	if res := owner.get(job + "/download"); res.code != 404 {
		t.Errorf("someone else's download = %d", res.code)
	}
	if res := guest.post("/exports", url.Values{"scope": {"workspace"}, "id": {"999999"}}); res.code != 404 {
		t.Errorf("export of an unknown workspace = %d", res.code)
	}

	// One export at a time: a second request shows the one in progress.
	var running int64
	testDB.QueryRow(context.Background(), `INSERT INTO export_jobs (user_id, name, format, status) VALUES ($1, 'x', 'md', 'running') RETURNING id`, guest.ID).Scan(&running)
	if res := guest.post("/exports", url.Values{"scope": {"all"}}); res.location != fmt.Sprintf("/exports/%d", running) {
		t.Errorf("second export went to %q", res.location)
	}
	testDB.Exec(context.Background(), `UPDATE export_jobs SET status = 'done', expires_at = now() - interval '1 minute' WHERE user_id = $1`, guest.ID)
	if res := guest.get(job); !strings.Contains(res.body, "expired") {
		t.Error("expired export not shown as expired")
	}
	if res := guest.get(job + "/download"); res.code != 404 {
		t.Errorf("expired download = %d", res.code)
	}
}
