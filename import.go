package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Imports are converted to HTML on the server and stored in notes.import_html. The first
// editor to open the note seeds its live document from that HTML with TipTap's own parser,
// so we never have to produce TipTap JSON here.

type importedNote struct {
	title     string
	body      *html.Node // children are the note content
	created   *time.Time
	updated   *time.Time
	tags      []string
	resources map[string]resource // by md5 hash, referenced from <en-media hash="...">
	// files resolves a relative image or link path to a file next to the note (zip imports).
	files func(ref string) (resource, bool)
}

type resource struct {
	name, mime string
	data       []byte
}

type importStats struct {
	Notes   int
	Files   int      // attachments stored
	Missing int      // attachments skipped (no storage, or upload failed)
	Skipped []string // input files with an unsupported type
	Errors  []string
}

// parseHTML returns the <body> of an HTML document and its <title>.
func parseHTML(r io.Reader) (body *html.Node, title string, err error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, "", err
	}
	for n := range doc.Descendants() {
		switch {
		case n.DataAtom == atom.Title && title == "":
			title = strings.TrimSpace(textOf(n))
		case n.DataAtom == atom.Body:
			body = n
		}
	}
	return body, title, nil
}

// wikiEmbed matches Obsidian embeds like ![[image.png]] or ![[image.png|300]].
var wikiEmbed = regexp.MustCompile(`!\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)

func markdownToHTML(src []byte) (*html.Node, error) {
	src = wikiEmbed.ReplaceAll(src, []byte("![](<$1>)"))
	var buf bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert(src, &buf); err != nil {
		return nil, err
	}
	body, _, err := parseHTML(&buf)
	return body, err
}

func textToHTML(src []byte) (*html.Node, error) {
	var buf strings.Builder
	for line := range strings.Lines(string(src)) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			buf.WriteString("<p>" + html.EscapeString(line) + "</p>")
		}
	}
	body, _, err := parseHTML(strings.NewReader(buf.String()))
	return body, err
}

type enexNote struct {
	Title     string   `xml:"title"`
	Content   string   `xml:"content"`
	Created   string   `xml:"created"`
	Updated   string   `xml:"updated"`
	Tags      []string `xml:"tag"`
	Resources []struct {
		Data     string `xml:"data"`
		Mime     string `xml:"mime"`
		FileName string `xml:"resource-attributes>file-name"`
	} `xml:"resource"`
}

func enexTime(s string) *time.Time {
	t, err := time.Parse("20060102T150405Z", strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &t
}

// parseENEX streams the notes of an Evernote export to fn, one at a time.
func parseENEX(r io.Reader, fn func(importedNote) error) error {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "note" {
			continue
		}
		var en enexNote
		if err := dec.DecodeElement(&en, &start); err != nil {
			return err
		}
		body, _, err := parseHTML(strings.NewReader(en.Content))
		if err != nil {
			return err
		}
		n := importedNote{
			title: strings.TrimSpace(en.Title), body: body, tags: en.Tags,
			created: enexTime(en.Created), updated: enexTime(en.Updated), resources: map[string]resource{},
		}
		for _, res := range en.Resources {
			data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(res.Data), ""))
			if err != nil {
				continue
			}
			sum := md5.Sum(data)
			n.resources[hex.EncodeToString(sum[:])] = resource{name: res.FileName, mime: res.Mime, data: data}
		}
		if err := fn(n); err != nil {
			return err
		}
	}
}

func element(a atom.Atom, attrs ...string) *html.Node {
	n := &html.Node{Type: html.ElementNode, DataAtom: a, Data: a.String()}
	for i := 0; i+1 < len(attrs); i += 2 {
		n.Attr = append(n.Attr, html.Attribute{Key: attrs[i], Val: attrs[i+1]})
	}
	return n
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

// styleHas reports whether an element's inline style contains a declaration such as
// "--en-todo:true", ignoring spaces.
func styleHas(n *html.Node, decl string) bool {
	return strings.Contains(strings.ReplaceAll(attr(n, "style"), " ", ""), decl)
}

// textOf returns the text of a node, with line breaks between block elements.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.DataAtom == atom.Br:
			b.WriteString("\n")
		case n.DataAtom == atom.Script || n.DataAtom == atom.Style:
			return
		}
		for c := range n.ChildNodes() {
			walk(c)
		}
		switch n.DataAtom {
		case atom.P, atom.Div, atom.Li, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Pre, atom.Tr, atom.Blockquote:
			if !strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n")
			}
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func elements(root *html.Node) []*html.Node {
	var nodes []*html.Node
	for n := range root.Descendants() {
		if n.Type == html.ElementNode {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// unwrap moves a node's children in front of it. The HTML parser does not honor
// self-closing tags like <en-todo/>, so whatever follows them ends up inside.
func unwrap(n *html.Node) {
	for c := n.FirstChild; c != nil; c = n.FirstChild {
		n.RemoveChild(c)
		n.Parent.InsertBefore(c, n)
	}
}

// normalize rewrites Evernote and Markdown markup into what the editor understands, then
// sanitizes it: checklists become TipTap task lists, Evernote code blocks become
// <pre><code>, and <en-media> becomes whatever media returns (nil drops it).
//
// embed may replace the src of images and the href of links (to store embedded files); an
// empty result leaves the URL as it is.
func normalize(root *html.Node, media func(hash string) *html.Node, embed func(url string) string) {
	for _, n := range elements(root) {
		if key := map[atom.Atom]string{atom.Img: "src", atom.A: "href"}[n.DataAtom]; key != "" && embed != nil {
			for i, a := range n.Attr {
				if a.Key == key {
					if u := embed(a.Val); u != "" {
						n.Attr[i].Val = u
					}
				}
			}
		}
		switch {
		case n.Data == "en-media":
			unwrap(n)
			if m := media(attr(n, "hash")); m != nil {
				n.Parent.InsertBefore(m, n)
			}
			n.Parent.RemoveChild(n)

		case n.Data == "en-todo": // legacy checklist: <div><en-todo checked="true"/>text</div>
			unwrap(n)
			block := n.Parent
			block.RemoveChild(n)
			li := element(atom.Li, "data-type", "taskItem", "data-checked", fmt.Sprint(attr(n, "checked") == "true"))
			p := element(atom.P)
			for c := block.FirstChild; c != nil; c = block.FirstChild {
				block.RemoveChild(c)
				p.AppendChild(c)
			}
			li.AppendChild(p)
			ul := element(atom.Ul, "data-type", "taskList")
			ul.AppendChild(li)
			block.AppendChild(ul)

		case styleHas(n, "-en-codeblock:true"):
			pre, code := element(atom.Pre), element(atom.Code)
			code.AppendChild(&html.Node{Type: html.TextNode, Data: textOf(n)})
			pre.AppendChild(code)
			n.Parent.InsertBefore(pre, n)
			n.Parent.RemoveChild(n)

		case n.DataAtom == atom.Ul && styleHas(n, "--en-todo:true"): // current Evernote checklists
			setAttr(n, "data-type", "taskList")
			for li := range n.ChildNodes() {
				if li.DataAtom == atom.Li {
					setAttr(li, "data-type", "taskItem")
					setAttr(li, "data-checked", fmt.Sprint(styleHas(li, "--en-checked:true")))
				}
			}

		case n.DataAtom == atom.Li: // Markdown task list item: <li><input type="checkbox" checked> text</li>
			box := n.FirstChild
			for box != nil && box.Type == html.TextNode && strings.TrimSpace(box.Data) == "" {
				box = box.NextSibling
			}
			if box == nil || box.DataAtom != atom.Input || attr(box, "type") != "checkbox" {
				continue
			}
			checked := hasAttr(box, "checked")
			n.RemoveChild(box)
			setAttr(n, "data-type", "taskItem")
			setAttr(n, "data-checked", fmt.Sprint(checked))
			if n.Parent != nil && n.Parent.DataAtom == atom.Ul && attr(n.Parent, "data-type") == "" {
				setAttr(n.Parent, "data-type", "taskList")
			}
		}
	}
	sanitize(root)
}

// sanitize drops active content and every attribute the editor does not use. The editor
// parses imports inertly and keeps only what its schema knows, so this is defense in depth.
func sanitize(root *html.Node) {
	for _, n := range elements(root) {
		switch n.DataAtom {
		case atom.Script, atom.Style, atom.Iframe, atom.Object, atom.Embed, atom.Form:
			n.Parent.RemoveChild(n)
			continue
		}
		var kept []html.Attribute
		for _, a := range n.Attr {
			switch a.Key {
			case "alt", "data-type", "data-checked", "start":
			case "href", "src":
				if !safeURL(a.Val) {
					continue
				}
			case "class":
				if !strings.HasPrefix(a.Val, "language-") {
					continue
				}
			default:
				continue
			}
			kept = append(kept, a)
		}
		n.Attr = kept
	}
}

func safeURL(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	for _, prefix := range []string{"https://", "http://", "mailto:", "/files/"} {
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	return false
}

// plainText is the note text for listing and search, without blank lines.
func plainText(n *html.Node) string {
	var lines []string
	for line := range strings.Lines(textOf(n)) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func renderChildren(n *html.Node) (string, error) {
	var b strings.Builder
	for c := range n.ChildNodes() {
		if err := html.Render(&b, c); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// storeImported saves one imported note, uploading its attachments.
func (s *server) storeImported(ctx context.Context, wsID int64, folderID *int64, authorID int64, n importedNote, st *importStats) error {
	var id int64
	if err := s.db.QueryRow(ctx, `
		INSERT INTO notes (workspace_id, folder_id, title, author_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, coalesce($5, now()), coalesce($6, $5, now())) RETURNING id`,
		wsID, folderID, n.title, authorID, n.created, n.updated).Scan(&id); err != nil {
		return err
	}
	if n.body == nil {
		n.body = element(atom.Body)
	}
	stored := map[string]string{} // reference -> attachment URL, so repeats upload once
	store := func(ref string, res resource) string {
		if u, ok := stored[ref]; ok {
			return u
		}
		attID, err := s.putAttachment(ctx, id, res.name, res.mime, res.data)
		if err != nil {
			st.Missing++
			return ""
		}
		st.Files++
		stored[ref] = fmt.Sprintf("/files/%d", attID)
		return stored[ref]
	}
	embed := func(ref string) string {
		if res, ok := dataURI(ref); ok {
			return store(ref, res)
		}
		if n.files != nil && isRelative(ref) {
			if res, ok := n.files(ref); ok {
				return store(ref, res)
			}
		}
		return ""
	}
	normalize(n.body, func(hash string) *html.Node {
		res, ok := n.resources[hash]
		if !ok {
			return nil
		}
		url := store(hash, res)
		if url == "" {
			return nil
		}
		if strings.HasPrefix(res.mime, "image/") {
			return element(atom.Img, "src", url, "alt", res.name)
		}
		a := element(atom.A, "href", url)
		a.AppendChild(&html.Node{Type: html.TextNode, Data: cmp.Or(res.name, "attachment")})
		p := element(atom.P)
		p.AppendChild(a)
		return p
	}, embed)
	body, err := renderChildren(n.body)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `UPDATE notes SET import_html = $1, content_text = $2 WHERE id = $3`,
		body, plainText(n.body), id); err != nil {
		return err
	}
	for _, t := range n.tags {
		if t = strings.TrimSpace(t); t != "" {
			if err := s.tagNote(ctx, wsID, id, t); err != nil {
				return err
			}
		}
	}
	st.Notes++
	return nil
}

func (s *server) importPage(w http.ResponseWriter, r *http.Request) {
	data, ok := s.sidebar(w, r)
	if !ok {
		return
	}
	data["Storage"] = s.files != nil
	s.render(w, "import.html", data)
}

func (s *server) importNotes(w http.ResponseWriter, r *http.Request) {
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
	var st importStats
	for _, fh := range r.MultipartForm.File["files"] {
		f, err := fh.Open()
		if err != nil {
			serverError(w, err)
			return
		}
		err = s.importFile(ctx, ws.ID, folderID, user.ID, fh.Filename, f, fh.Size, &st)
		f.Close()
		if err == errUnsupported {
			st.Skipped = append(st.Skipped, fh.Filename)
		} else if err != nil {
			log.Printf("import %s: %v", fh.Filename, err)
			st.Errors = append(st.Errors, fh.Filename+": "+err.Error())
		}
	}
	data, ok := s.sidebar(w, r)
	if !ok {
		return
	}
	data["Storage"] = s.files != nil
	data["Stats"] = &st
	s.render(w, "import.html", data)
}

var errUnsupported = fmt.Errorf("unsupported file type")

// upload is an uploaded file: multipart files and *os.File both qualify.
type upload interface {
	io.Reader
	io.ReaderAt
}

// importFile imports one uploaded file.
func (s *server) importFile(ctx context.Context, wsID int64, folderID *int64, userID int64, filename string, f upload, size int64, st *importStats) error {
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
func (s *server) importENEX(ctx context.Context, wsID int64, folderID *int64, userID int64, name string, r io.Reader, st *importStats) error {
	notebook, err := s.newFolder(ctx, wsID, folderID, name)
	if err != nil {
		return err
	}
	return parseENEX(r, func(n importedNote) error {
		return s.storeImported(ctx, wsID, &notebook, userID, n, st)
	})
}

func (s *server) newFolder(ctx context.Context, wsID int64, parentID *int64, name string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO folders (workspace_id, parent_id, name) VALUES ($1, $2, $3) RETURNING id`,
		wsID, parentID, name).Scan(&id)
	return id, err
}

// importDoc imports one Markdown, HTML or text document.
func (s *server) importDoc(ctx context.Context, wsID int64, folderID *int64, userID int64, ext, name string, src []byte,
	files func(string) (resource, bool), st *importStats) error {
	n := importedNote{title: name, files: files}
	var err error
	switch ext {
	case ".md", ".markdown":
		n.body, err = markdownToHTML(src)
	case ".html", ".htm":
		var title string
		n.body, title, err = parseHTML(bytes.NewReader(src))
		n.title = cmp.Or(title, name)
	case ".txt":
		n.body, err = textToHTML(src)
	default:
		return errUnsupported
	}
	if err != nil {
		return err
	}
	return s.storeImported(ctx, wsID, folderID, userID, n, st)
}

var noteExts = map[string]bool{".md": true, ".markdown": true, ".html": true, ".htm": true, ".txt": true, ".enex": true}

const maxArchive = 2 << 30 // uncompressed bytes per zip

// importZip imports an archive of notes (e.g. an Obsidian or Notion export) into a new folder,
// mirroring its directories. Images and files the notes reference are stored as attachments.
func (s *server) importZip(ctx context.Context, wsID int64, folderID *int64, userID int64, name string, f io.ReaderAt, size int64, st *importStats) error {
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
		if !noteExts[ext] {
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

func (s *server) importZipEntry(ctx context.Context, wsID int64, folderID *int64, userID int64,
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
	files := func(ref string) (resource, bool) {
		target := resolveRef(path.Dir(p), ref)
		if byPath[target] == nil && len(byName[path.Base(target)]) == 1 {
			target = byName[path.Base(target)][0]
		}
		zf := byPath[target]
		if zf == nil || noteExts[strings.ToLower(path.Ext(target))] || zf.UncompressedSize64 > maxUpload {
			return resource{}, false
		}
		r, err := zf.Open()
		if err != nil {
			return resource{}, false
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return resource{}, false
		}
		return resource{name: path.Base(target), mime: mimeOf(target, data), data: data}, true
	}
	return s.importDoc(ctx, wsID, folderID, userID, ext, name, src, files, st)
}

// resolveRef turns a relative URL found in a note at dir into an archive path.
func resolveRef(dir, ref string) string {
	ref, _, _ = strings.Cut(ref, "#")
	ref, _, _ = strings.Cut(ref, "?")
	if unescaped, err := url.PathUnescape(ref); err == nil {
		ref = unescaped
	}
	return path.Join(dir, ref)
}

func isRelative(ref string) bool {
	u, err := url.Parse(ref)
	return err == nil && u.Scheme == "" && u.Host == "" && !strings.HasPrefix(ref, "/") && !strings.HasPrefix(ref, "#")
}

func mimeOf(name string, data []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(data)
}

// dataURI decodes an inline base64 file such as data:image/png;base64,....
func dataURI(ref string) (resource, bool) {
	rest, ok := strings.CutPrefix(ref, "data:")
	if !ok {
		return resource{}, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	mimeType, isBase64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isBase64 {
		return resource{}, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) > maxUpload {
		return resource{}, false
	}
	name := "image"
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		name += exts[0]
	}
	return resource{name: name, mime: mimeType, data: data}, true
}
