package main

import (
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
	"net/http"
	"path"
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

func markdownToHTML(src []byte) (*html.Node, error) {
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
func normalize(root *html.Node, media func(hash string) *html.Node) {
	for _, n := range elements(root) {
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
	normalize(n.body, func(hash string) *html.Node {
		res, ok := n.resources[hash]
		if !ok {
			return nil
		}
		attID, err := s.putAttachment(ctx, id, res.name, res.mime, res.data)
		if err != nil {
			st.Missing++
			return nil
		}
		st.Files++
		url := fmt.Sprintf("/files/%d", attID)
		if strings.HasPrefix(res.mime, "image/") {
			return element(atom.Img, "src", url, "alt", res.name)
		}
		a := element(atom.A, "href", url)
		a.AppendChild(&html.Node{Type: html.TextNode, Data: cmp.Or(res.name, "attachment")})
		p := element(atom.P)
		p.AppendChild(a)
		return p
	})
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
		ext := strings.ToLower(path.Ext(fh.Filename))
		name := strings.TrimSuffix(fh.Filename, path.Ext(fh.Filename))
		f, err := fh.Open()
		if err != nil {
			serverError(w, err)
			return
		}
		err = s.importFile(ctx, ws.ID, folderID, user.ID, ext, name, f, &st)
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

// importFile imports one uploaded file. Each .enex file is a notebook, so its notes go into a
// new folder named after it.
func (s *server) importFile(ctx context.Context, wsID int64, folderID *int64, userID int64, ext, name string, f io.Reader, st *importStats) error {
	if ext == ".enex" {
		var notebook int64
		if err := s.db.QueryRow(ctx, `INSERT INTO folders (workspace_id, parent_id, name) VALUES ($1, $2, $3) RETURNING id`,
			wsID, folderID, name).Scan(&notebook); err != nil {
			return err
		}
		return parseENEX(f, func(n importedNote) error {
			return s.storeImported(ctx, wsID, &notebook, userID, n, st)
		})
	}
	src, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	n := importedNote{title: name}
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
