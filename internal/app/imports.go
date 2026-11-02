package app

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"notetaker/internal/importer"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Imports are converted to HTML on the server and stored in notes.import_html. The first
// editor to open the note seeds its live document from that HTML with TipTap's own parser,
// so we never have to produce TipTap JSON here.

type importStats struct {
	Notes   int
	Files   int      // attachments stored
	Missing int      // attachments skipped (no storage, or upload failed)
	Skipped []string // input files with an unsupported type
	Errors  []string

	onNote func() // called after each stored note, to report progress
}

// storeImported saves one imported note, uploading its attachments.
func (s *Server) storeImported(ctx context.Context, wsID int64, folderID *int64, authorID int64, n importer.Note, st *importStats) error {
	var id int64
	if err := s.db.QueryRow(ctx, `
		INSERT INTO notes (workspace_id, folder_id, title, author_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, coalesce($5, now()), coalesce($6, $5, now())) RETURNING id`,
		wsID, folderID, n.Title, authorID, n.Created, n.Updated).Scan(&id); err != nil {
		return err
	}
	if n.Body == nil {
		n.Body = importer.Element(atom.Body)
	}
	stored := map[string]string{} // reference -> attachment URL, so repeats upload once
	store := func(ref string, res importer.Resource) string {
		if u, ok := stored[ref]; ok {
			return u
		}
		attID, err := s.putAttachment(ctx, id, res.Name, res.Mime, res.Data)
		if err != nil {
			st.Missing++
			return ""
		}
		st.Files++
		stored[ref] = fmt.Sprintf("/files/%d", attID)
		return stored[ref]
	}
	embed := func(ref string) string {
		if res, ok := importer.DataURI(ref); ok {
			return store(ref, res)
		}
		if n.Files != nil && importer.IsRelative(ref) {
			if res, ok := n.Files(ref); ok {
				return store(ref, res)
			}
		}
		return ""
	}
	importer.Normalize(n.Body, func(hash string) *html.Node {
		res, ok := n.Resources[hash]
		if !ok {
			return nil
		}
		url := store(hash, res)
		if url == "" {
			return nil
		}
		if strings.HasPrefix(res.Mime, "image/") {
			return importer.Element(atom.Img, "src", url, "alt", res.Name)
		}
		a := importer.Element(atom.A, "href", url)
		a.AppendChild(&html.Node{Type: html.TextNode, Data: cmp.Or(res.Name, "attachment")})
		p := importer.Element(atom.P)
		p.AppendChild(a)
		return p
	}, embed)
	body, err := importer.RenderChildren(n.Body)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `UPDATE notes SET import_html = $1, content_text = $2 WHERE id = $3`,
		body, importer.PlainText(n.Body), id); err != nil {
		return err
	}
	for _, t := range n.Tags {
		if t = strings.TrimSpace(t); t != "" {
			if err := s.tagNote(ctx, wsID, id, t); err != nil {
				return err
			}
		}
	}
	st.Notes++
	if st.onNote != nil {
		st.onNote()
	}
	return nil
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	data, ok := s.sidebar(w, r)
	if !ok {
		return
	}
	data["Storage"] = s.files != nil
	data["Page"] = "import"
	s.render(w, "import.html", data)
}

// importDir holds uploads while their import job runs.
var importDir = filepath.Join(os.TempDir(), "notetaker-imports")

// importNotes saves the uploaded files and imports them in the background; the browser is
// sent to a page that shows the job's progress.
func (s *Server) importNotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "upload too large or invalid", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	folderID := formID(r, "folder_id")
	ws, ok := s.container(w, r, pathID(r, "id"), folderID)
	if !ok {
		return
	}
	user := currentUser(r)
	var jobID int64
	if err := s.db.QueryRow(ctx, `INSERT INTO import_jobs (workspace_id, user_id) VALUES ($1, $2) RETURNING id`,
		ws.ID, user.ID).Scan(&jobID); err != nil {
		serverError(w, err)
		return
	}
	dir := filepath.Join(importDir, strconv.FormatInt(jobID, 10))
	var names []string
	err := os.MkdirAll(dir, 0o700)
	for i, fh := range r.MultipartForm.File["files"] {
		if err != nil {
			break
		}
		name := filepath.Base(fh.Filename)
		names = append(names, name)
		err = saveUpload(fh, filepath.Join(dir, fmt.Sprintf("%d-%s", i, name)))
	}
	if err != nil {
		os.RemoveAll(dir)
		s.db.Exec(ctx, `DELETE FROM import_jobs WHERE id = $1`, jobID)
		serverError(w, err)
		return
	}
	go s.runImport(jobID, ws.ID, folderID, user.ID, dir, names)
	redirect(w, r, fmt.Sprintf("/w/%d/imports/%d", ws.ID, jobID))
}

func saveUpload(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// runImport imports the files saved for a job, recording progress in import_jobs.
func (s *Server) runImport(jobID, wsID int64, folderID *int64, userID int64, dir string, names []string) {
	ctx := context.Background()
	defer os.RemoveAll(dir)
	var st importStats
	save := func(running bool) {
		stats, _ := json.Marshal(&st)
		if _, err := s.db.Exec(ctx, `UPDATE import_jobs SET stats = $1, running = $2 WHERE id = $3`, stats, running, jobID); err != nil {
			log.Printf("import job %d: %v", jobID, err)
		}
	}
	last := time.Now()
	st.onNote = func() {
		if time.Since(last) > time.Second {
			save(true)
			last = time.Now()
		}
	}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("import job %d panicked: %v", jobID, p)
			st.Errors = append(st.Errors, "the import stopped because of an internal error")
		}
		save(false)
	}()
	for i, name := range names {
		err := s.importSaved(ctx, wsID, folderID, userID, filepath.Join(dir, fmt.Sprintf("%d-%s", i, name)), name, &st)
		if err == errUnsupported {
			st.Skipped = append(st.Skipped, name)
		} else if err != nil {
			log.Printf("import %s: %v", name, err)
			st.Errors = append(st.Errors, name+": "+err.Error())
		}
	}
}

func (s *Server) importSaved(ctx context.Context, wsID int64, folderID *int64, userID int64, file, name string, st *importStats) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return s.importFile(ctx, wsID, folderID, userID, name, f, info.Size(), st)
}

// recoverImports marks jobs cut short by a restart and removes their files.
func recoverImports(ctx context.Context, db *pgxpool.Pool) {
	os.RemoveAll(importDir)
	if _, err := db.Exec(ctx, `
		UPDATE import_jobs SET running = false,
			stats = jsonb_set(stats, '{Errors}', coalesce(stats->'Errors', '[]') || '["interrupted by a server restart"]')
		WHERE running`); err != nil {
		log.Printf("recover imports: %v", err)
	}
}

func (s *Server) importJobPage(w http.ResponseWriter, r *http.Request) {
	data, ok := s.sidebar(w, r)
	if !ok {
		return
	}
	var running bool
	var stats []byte
	err := s.db.QueryRow(r.Context(), `SELECT running, stats FROM import_jobs WHERE id = $1 AND workspace_id = $2 AND user_id = $3`,
		pathID(r, "job"), pathID(r, "id"), currentUser(r).ID).Scan(&running, &stats)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	var st importStats
	json.Unmarshal(stats, &st)
	data["Storage"] = s.files != nil
	data["Page"] = "import"
	data["Stats"] = &st
	data["Running"] = running
	data["JobURL"] = r.URL.Path
	s.render(w, "import.html", data)
}

var errUnsupported = fmt.Errorf("unsupported file type")

// upload is an uploaded file: multipart files and *os.File both qualify.
type upload interface {
	io.Reader
	io.ReaderAt
}

// importFile imports one uploaded file.
func (s *Server) importFile(ctx context.Context, wsID int64, folderID *int64, userID int64, filename string, f upload, size int64, st *importStats) error {
	ext := strings.ToLower(path.Ext(filename))
	name := strings.TrimSuffix(filename, path.Ext(filename))
	switch ext {
	case ".zip":
		return s.importZip(ctx, wsID, folderID, userID, name, f, size, st)
	case ".enex":
		return s.importENEX(ctx, wsID, folderID, userID, name, f, st)
	}
	src, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return s.importDoc(ctx, wsID, folderID, userID, ext, name, src, nil, st)
}

// importENEX imports an Evernote notebook into a new folder named after it.
func (s *Server) importENEX(ctx context.Context, wsID int64, folderID *int64, userID int64, name string, r io.Reader, st *importStats) error {
	notebook, err := s.newFolder(ctx, wsID, folderID, name)
	if err != nil {
		return err
	}
	return importer.ParseENEX(r, func(n importer.Note) error {
		return s.storeImported(ctx, wsID, &notebook, userID, n, st)
	})
}

func (s *Server) newFolder(ctx context.Context, wsID int64, parentID *int64, name string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO folders (workspace_id, parent_id, name) VALUES ($1, $2, $3) RETURNING id`,
		wsID, parentID, name).Scan(&id)
	return id, err
}

// importDoc imports one Markdown, HTML or text document.
func (s *Server) importDoc(ctx context.Context, wsID int64, folderID *int64, userID int64, ext, name string, src []byte,
	files func(string) (importer.Resource, bool), st *importStats) error {
	n := importer.Note{Title: name, Files: files}
	var err error
	switch ext {
	case ".md", ".markdown":
		n.Body, err = importer.MarkdownToHTML(src)
	case ".html", ".htm":
		var title string
		n.Body, title, err = importer.ParseHTML(bytes.NewReader(src))
		n.Title = cmp.Or(title, name)
	case ".txt":
		n.Body, err = importer.TextToHTML(src)
	default:
		return errUnsupported
	}
	if err != nil {
		return err
	}
	return s.storeImported(ctx, wsID, folderID, userID, n, st)
}

const maxArchive = 2 << 30 // uncompressed bytes per zip

// importZip imports an archive of notes (e.g. an Obsidian or Notion export) into a new folder,
// mirroring its directories. Images and files the notes reference are stored as attachments.
func (s *Server) importZip(ctx context.Context, wsID int64, folderID *int64, userID int64, name string, f io.ReaderAt, size int64, st *importStats) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return err
	}
	byPath := map[string]*zip.File{}
	byName := map[string][]string{} // base name -> paths, for Obsidian-style links by name
	var total uint64
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || strings.HasPrefix(zf.Name, "__MACOSX/") || strings.HasPrefix(path.Base(zf.Name), ".") {
			continue
		}
		if total += zf.UncompressedSize64; total > maxArchive {
			return fmt.Errorf("archive is larger than 2 GB uncompressed")
		}
		byPath[zf.Name] = zf
		byName[path.Base(zf.Name)] = append(byName[path.Base(zf.Name)], zf.Name)
	}
	// Exports often wrap everything in one directory; the import folder already plays that role.
	strip := ""
	for p := range byPath {
		top, _, nested := strings.Cut(p, "/")
		if !nested || (strip != "" && strip != top+"/") {
			strip = ""
			break
		}
		strip = top + "/"
	}
	root, err := s.newFolder(ctx, wsID, folderID, name)
	if err != nil {
		return err
	}
	folders := map[string]int64{".": root}
	var folderFor func(dir string) (int64, error)
	folderFor = func(dir string) (int64, error) {
		if id, ok := folders[dir]; ok {
			return id, nil
		}
		parent, err := folderFor(path.Dir(dir))
		if err != nil {
			return 0, err
		}
		id, err := s.newFolder(ctx, wsID, &parent, path.Base(dir))
		folders[dir] = id
		return id, err
	}
	for _, p := range slices.Sorted(maps.Keys(byPath)) {
		ext := strings.ToLower(path.Ext(p))
		if !importer.NoteExts[ext] {
			continue // an asset; stored when a note references it
		}
		rel := strings.TrimPrefix(p, strip)
		dirID, err := folderFor(path.Dir(rel))
		if err != nil {
			return err
		}
		if err := s.importZipEntry(ctx, wsID, &dirID, userID, byPath, byName, p, st); err != nil {
			st.Errors = append(st.Errors, rel+": "+err.Error())
		}
	}
	return nil
}

func (s *Server) importZipEntry(ctx context.Context, wsID int64, folderID *int64, userID int64,
	byPath map[string]*zip.File, byName map[string][]string, p string, st *importStats) error {
	rc, err := byPath[p].Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	ext := strings.ToLower(path.Ext(p))
	name := strings.TrimSuffix(path.Base(p), path.Ext(p))
	if ext == ".enex" {
		return s.importENEX(ctx, wsID, folderID, userID, name, rc, st)
	}
	src, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	files := func(ref string) (importer.Resource, bool) {
		target := importer.ResolveRef(path.Dir(p), ref)
		if byPath[target] == nil && len(byName[path.Base(target)]) == 1 {
			target = byName[path.Base(target)][0]
		}
		zf := byPath[target]
		if zf == nil || importer.NoteExts[strings.ToLower(path.Ext(target))] || zf.UncompressedSize64 > maxUpload {
			return importer.Resource{}, false
		}
		r, err := zf.Open()
		if err != nil {
			return importer.Resource{}, false
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return importer.Resource{}, false
		}
		return importer.Resource{Name: path.Base(target), Mime: importer.MimeOf(target, data), Data: data}, true
	}
	return s.importDoc(ctx, wsID, folderID, userID, ext, name, src, files, st)
}
