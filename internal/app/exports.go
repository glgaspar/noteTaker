package app

import (
	"archive/zip"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"notetaker/internal/export"
)

// Exports: a single note downloads right away; folders, workspaces and "everything" run as
// background jobs that write a zip to exportDir, which the user downloads when it is done.
// Exports include only notes the user can see.

var exportDir = filepath.Join(os.TempDir(), "notetaker-exports")

const (
	exportsAtOnce = 2 // exports running in parallel, server-wide
	exportTTL     = 24 * time.Hour
)

type exportNote struct {
	ID          int64
	WorkspaceID int64
	FolderID    int64
	Title       string
	Created     time.Time
	Updated     time.Time
	Tags        []string
}

// exportScope selects notes: one of note, folder or workspace by id, or all with id 0.
type exportScope struct {
	kind string // "note", "folder", "workspace" or "all"
	id   int64
}

// exportList returns the notes in scope that the user can see, without their content.
func (s *Server) exportList(ctx context.Context, userID int64, sc exportScope) ([]exportNote, error) {
	rows, err := s.db.Query(ctx, `
		SELECT n.id, n.workspace_id, coalesce(n.folder_id, 0), n.title, n.created_at, n.updated_at,
			ARRAY(SELECT t.name FROM note_tags nt JOIN tags t ON t.id = nt.tag_id WHERE nt.note_id = n.id ORDER BY lower(t.name))
		FROM notes n
		WHERE effective_role($1, n.workspace_id, n.folder_id) IS NOT NULL
			AND CASE $2
				WHEN 'note' THEN n.id = $3
				WHEN 'workspace' THEN n.workspace_id = $3
				WHEN 'folder' THEN n.folder_id IN (
					WITH RECURSIVE sub AS (
						SELECT id FROM folders WHERE id = $3
						UNION ALL
						SELECT c.id FROM folders c JOIN sub ON c.parent_id = sub.id
					) SELECT id FROM sub)
				ELSE true
			END
		ORDER BY n.workspace_id, n.folder_id NULLS FIRST, lower(n.title), n.id`, userID, sc.kind, sc.id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[exportNote])
}

type exportStats struct {
	Notes, Files int
	Errors       []string
}

// exporter writes notes as files into a zip, mirroring folders as directories and copying
// their attachments next to them.
type exporter struct {
	s      *Server
	ctx    context.Context
	userID int64
	format string // "md" or "html"
	zw     *zip.Writer
	scope  exportScope
	stats  exportStats

	names      export.Names
	workspaces map[int64]workspace
	folders    map[int64][]folder // per workspace
	dirs       map[int64]string   // folder id (0 = workspace root, by negative workspace id) -> zip dir
	index      [][2]string        // HTML exports: path, title
}

func (s *Server) newExporter(ctx context.Context, userID int64, format string, sc exportScope, w io.Writer) (*exporter, error) {
	wss, err := s.userWorkspaces(ctx, userID, 0)
	if err != nil {
		return nil, err
	}
	e := &exporter{s: s, ctx: ctx, userID: userID, format: format, scope: sc, zw: zip.NewWriter(w),
		names: export.Names{}, workspaces: map[int64]workspace{}, folders: map[int64][]folder{}, dirs: map[int64]string{}}
	for _, ws := range wss {
		e.workspaces[ws.ID] = ws
	}
	return e, nil
}

// dir is the zip directory for a note's folder (0: the workspace root). Folder exports start
// at the exported folder; the others start with the workspace name.
func (e *exporter) dir(wsID, folderID int64) (string, error) {
	key := folderID
	if folderID == 0 {
		key = -wsID
	}
	if d, ok := e.dirs[key]; ok {
		return d, nil
	}
	if _, ok := e.folders[wsID]; !ok {
		tree, err := e.s.folderTree(e.ctx, e.workspaces[wsID], e.userID)
		if err != nil {
			return "", err
		}
		e.folders[wsID] = tree
	}
	visible := map[int64]folder{}
	for _, f := range e.folders[wsID] {
		visible[f.ID] = f
	}
	f, ok := visible[folderID]
	var parent, name string
	switch {
	case folderID == 0 || !ok:
		if folderID != 0 { // a folder the user cannot see: keep its notes at the top
			return e.dir(wsID, 0)
		}
		name = e.workspaces[wsID].Name
	case e.scope.kind == "folder" && folderID == e.scope.id:
		name = f.Name
	default:
		parentID := orZero(f.ParentID)
		if _, seen := visible[parentID]; !seen {
			parentID = 0
		}
		var err error
		if parent, err = e.dir(wsID, parentID); err != nil {
			return "", err
		}
		name = f.Name
	}
	d := e.names.Unique(parent, export.SafeName(name), "")
	e.dirs[key] = d
	return d, nil
}

// noteHTML renders a note body; link rewrites attachment URLs.
func (e *exporter) noteHTML(id int64, link export.Link) (string, error) {
	var content, imported string
	if err := e.s.db.QueryRow(e.ctx, `SELECT coalesce(content::text, ''), coalesce(import_html, '') FROM notes WHERE id = $1`, id).
		Scan(&content, &imported); err != nil {
		return "", err
	}
	switch {
	case content != "" && content != "null":
		return export.HTML([]byte(content), link)
	case imported != "":
		return export.RewriteHTML(imported, link)
	}
	return "", nil
}

type attachmentInfo struct {
	id         int64
	name, mime string
}

// attachment returns the stored file behind a /files/{id} URL, if it belongs to the note.
func (e *exporter) attachment(noteID int64, url string) (attachmentInfo, bool) {
	rest, isFile := strings.CutPrefix(url, "/files/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if !isFile || err != nil || e.s.files == nil {
		return attachmentInfo{}, false
	}
	a := attachmentInfo{id: id}
	err = e.s.db.QueryRow(e.ctx, `SELECT name, mime FROM attachments WHERE id = $1 AND note_id = $2`, id, noteID).Scan(&a.name, &a.mime)
	return a, err == nil
}

// add writes one note (and its attachments) into the zip.
func (e *exporter) add(n exportNote) error {
	dir, err := e.dir(n.WorkspaceID, n.FolderID)
	if err != nil {
		return err
	}
	copied := map[string]string{}
	link := func(url string) string {
		if done, ok := copied[url]; ok {
			return done
		}
		a, ok := e.attachment(n.ID, url)
		if !ok {
			return url
		}
		ext := path.Ext(a.name)
		target := e.names.Unique(path.Join(dir, "attachments"), export.SafeName(strings.TrimSuffix(a.name, ext)), ext)
		if err := e.copyFile(a.id, target); err != nil {
			e.stats.Errors = append(e.stats.Errors, fmt.Sprintf("%s: %v", a.name, err))
			return url
		}
		e.stats.Files++
		copied[url] = export.PathURL(strings.TrimPrefix(target, dir+"/"))
		return copied[url]
	}
	body, err := e.noteHTML(n.ID, link)
	if err != nil {
		return err
	}
	meta := export.Meta{Title: n.Title, Tags: n.Tags, Created: n.Created, Updated: n.Updated}
	file, data, err := e.render(meta, body)
	if err != nil {
		return err
	}
	name := e.names.Unique(dir, export.SafeName(n.Title), file)
	w, err := e.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: n.Updated})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, data); err != nil {
		return err
	}
	e.index = append(e.index, [2]string{name, cmp.Or(n.Title, "Untitled")})
	e.stats.Notes++
	return nil
}

// render turns a note into a file: extension and contents.
func (e *exporter) render(meta export.Meta, body string) (string, string, error) {
	if e.format == "html" {
		return ".html", export.Page(meta, body), nil
	}
	md, err := export.Markdown(body)
	return ".md", export.FrontMatter(meta) + md + "\n", err
}

func (e *exporter) copyFile(id int64, target string) error {
	r, modified, err := e.s.files.Open(e.ctx, objectKey(id))
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := e.zw.CreateHeader(&zip.FileHeader{Name: target, Method: zip.Store, Modified: modified})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}

// finish adds the HTML table of contents and closes the zip.
func (e *exporter) finish(title string) error {
	if e.format == "html" && len(e.index) > 1 {
		w, err := e.zw.Create(e.names.Unique("", "index", ".html"))
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, export.IndexPage(title, e.index)); err != nil {
			return err
		}
	}
	return e.zw.Close()
}

func exportFormat(r *http.Request) string {
	if r.FormValue("format") == "html" {
		return "html"
	}
	return "md"
}

func attachmentHeader(w http.ResponseWriter, name, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
}

// exportNoteFile downloads one note: HTML as a single file with attachments embedded,
// Markdown as a .md file, or a zip when the note has attachments.
func (s *Server) exportNoteFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	notes, err := s.exportList(ctx, user.ID, exportScope{"note", pathID(r, "id")})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(notes) == 0 {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	n := notes[0]
	format := exportFormat(r)
	name := export.SafeName(n.Title)
	e := &exporter{s: s, ctx: ctx, userID: user.ID, format: format}
	meta := export.Meta{Title: n.Title, Tags: n.Tags, Created: n.Created, Updated: n.Updated}

	if format == "html" { // embed attachments as data: URLs so the file stands alone
		body, err := e.noteHTML(n.ID, func(url string) string {
			a, ok := e.attachment(n.ID, url)
			if !ok {
				return url
			}
			rc, _, err := s.files.Open(ctx, objectKey(a.id))
			if err != nil {
				return url
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return url
			}
			return "data:" + a.mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		attachmentHeader(w, name+".html", "text/html; charset=utf-8")
		io.WriteString(w, export.Page(meta, body))
		return
	}

	hasFiles := false
	if _, err := e.noteHTML(n.ID, func(url string) string {
		_, ok := e.attachment(n.ID, url)
		hasFiles = hasFiles || ok
		return url
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !hasFiles {
		body, err := e.noteHTML(n.ID, func(url string) string { return url })
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		_, data, err := e.render(meta, body)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		attachmentHeader(w, name+".md", "text/markdown; charset=utf-8")
		io.WriteString(w, data)
		return
	}
	attachmentHeader(w, name+".zip", "application/zip")
	ex, err := s.newExporter(ctx, user.ID, format, exportScope{"note", n.ID}, w)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ex.dirs[n.FolderID] = "" // a single note sits at the top of its zip
	if n.FolderID == 0 {
		ex.dirs[-n.WorkspaceID] = ""
	}
	if err := ex.add(n); err != nil {
		log.Printf("export note %d: %v", n.ID, err)
	}
	ex.zw.Close()
}

type exportJob struct {
	ID          int64
	Name        string
	Format      string
	Status      string
	Notes       int
	Files       int
	Error       string
	ExpiresAt   time.Time
	Unavailable bool // finished, but the file is gone (expired, or lost in a restart)
}

// startExport queues a zip export of a folder, a workspace or everything the user can see.
func (s *Server) startExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	sc := exportScope{r.FormValue("scope"), orZero(formID(r, "id"))}
	var name string
	switch sc.kind {
	case "folder":
		ws, err := s.roleOf(ctx, user.ID, folderRole, sc.id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if ws.Role == "" {
			s.fail(w, r, http.StatusNotFound, "")
			return
		}
		s.db.QueryRow(ctx, `SELECT name FROM folders WHERE id = $1`, sc.id).Scan(&name)
	case "workspace":
		found, err := s.userWorkspaces(ctx, user.ID, sc.id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if len(found) == 0 {
			s.fail(w, r, http.StatusNotFound, "")
			return
		}
		name = found[0].Name
	case "all":
		name = "NoteTaker"
	default:
		s.fail(w, r, http.StatusBadRequest, "unknown export scope")
		return
	}
	// One export at a time per user: a second request shows the one in progress.
	var id int64
	err := s.db.QueryRow(ctx, `SELECT id FROM export_jobs WHERE user_id = $1 AND status IN ('queued', 'running')`, user.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.db.QueryRow(ctx, `INSERT INTO export_jobs (user_id, name, format) VALUES ($1, $2, $3) RETURNING id`,
			user.ID, name, exportFormat(r)).Scan(&id)
		if err == nil {
			go s.runExport(id, user.ID, exportFormat(r), sc, name)
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/exports/%d", id))
}

func exportFile(id int64) string { return filepath.Join(exportDir, fmt.Sprintf("%d.zip", id)) }

// runExport builds a job's zip, at most exportsAtOnce at a time.
func (s *Server) runExport(id, userID int64, format string, sc exportScope, name string) {
	ctx := context.Background()
	s.exportSlots <- struct{}{}
	defer func() { <-s.exportSlots }()
	s.db.Exec(ctx, `UPDATE export_jobs SET status = 'running' WHERE id = $1`, id)

	fail := func(err error) {
		log.Printf("export job %d: %v", id, err)
		os.Remove(exportFile(id))
		s.db.Exec(ctx, `UPDATE export_jobs SET status = 'failed', error = $2 WHERE id = $1`, id, err.Error())
	}
	notes, err := s.exportList(ctx, userID, sc)
	if err != nil {
		fail(err)
		return
	}
	if err := os.MkdirAll(exportDir, 0o700); err != nil {
		fail(err)
		return
	}
	f, err := os.Create(exportFile(id))
	if err != nil {
		fail(err)
		return
	}
	defer f.Close()
	e, err := s.newExporter(ctx, userID, format, sc, f)
	if err != nil {
		fail(err)
		return
	}
	last := time.Now()
	for _, n := range notes {
		if err := e.add(n); err != nil {
			fail(err)
			return
		}
		if time.Since(last) > time.Second {
			s.db.Exec(ctx, `UPDATE export_jobs SET notes = $2, files = $3 WHERE id = $1`, id, e.stats.Notes, e.stats.Files)
			last = time.Now()
		}
	}
	if err := e.finish(name); err != nil {
		fail(err)
		return
	}
	s.db.Exec(ctx, `UPDATE export_jobs SET status = 'done', notes = $2, files = $3, error = $4, expires_at = $5 WHERE id = $1`,
		id, e.stats.Notes, e.stats.Files, strings.Join(e.stats.Errors, "\n"), time.Now().Add(exportTTL))
}

func (s *Server) loadExportJob(r *http.Request) (*exportJob, error) {
	var j exportJob
	err := s.db.QueryRow(r.Context(), `
		SELECT id, name, format, status, notes, files, error, expires_at FROM export_jobs WHERE id = $1 AND user_id = $2`,
		pathID(r, "id"), currentUser(r).ID).Scan(&j.ID, &j.Name, &j.Format, &j.Status, &j.Notes, &j.Files, &j.Error, &j.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if j.Status == "done" {
		_, statErr := os.Stat(exportFile(j.ID))
		j.Unavailable = statErr != nil || time.Now().After(j.ExpiresAt)
	}
	return &j, nil
}

func (s *Server) exportJobPage(w http.ResponseWriter, r *http.Request) {
	j, err := s.loadExportJob(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if j == nil {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	s.render(w, "export.html", map[string]any{"User": currentUser(r), "Job": j, "Loc": userLoc(r), "URL": r.URL.Path})
}

func (s *Server) downloadExport(w http.ResponseWriter, r *http.Request) {
	j, err := s.loadExportJob(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if j == nil || j.Status != "done" || j.Unavailable {
		s.fail(w, r, http.StatusNotFound, "this export is no longer available; start a new one")
		return
	}
	f, err := os.Open(exportFile(j.ID))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	attachmentHeader(w, export.SafeName(j.Name)+".zip", "application/zip")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// recoverExports fails jobs cut short by a restart.
func recoverExports(ctx context.Context, s *Server) {
	if _, err := s.db.Exec(ctx, `UPDATE export_jobs SET status = 'failed', error = 'interrupted by a server restart'
		WHERE status IN ('queued', 'running')`); err != nil {
		log.Printf("recover exports: %v", err)
	}
}

// cleanExportsEvery deletes expired export jobs and their files, for the life of the process.
func cleanExportsEvery(s *Server, interval time.Duration) {
	for {
		rows, err := s.db.Query(context.Background(), `DELETE FROM export_jobs WHERE expires_at < now() RETURNING id`)
		if err == nil {
			ids, _ := pgx.CollectRows(rows, pgx.RowTo[int64])
			for _, id := range ids {
				os.Remove(exportFile(id))
			}
		} else {
			log.Printf("clean exports: %v", err)
		}
		time.Sleep(interval)
	}
}
