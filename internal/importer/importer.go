package importer

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Package importer turns Evernote, Markdown, HTML and text documents into clean HTML the editor can load.

// MaxFile is the largest embedded file an import keeps.
const MaxFile = 25 << 20

// Note is an imported document, ready to be stored.
type Note struct {
	Title     string
	Body      *html.Node // children are the note content
	Created   *time.Time
	Updated   *time.Time
	Tags      []string
	Resources map[string]Resource // by md5 hash, referenced from <en-media hash="...">
	// files resolves a relative image or link path to a file next to the note (zip imports).
	Files func(ref string) (Resource, bool)
}

// Resource is a file embedded in or referenced by a note.
type Resource struct {
	Name, Mime string
	Data       []byte
}

// ParseHTML returns the <body> of an HTML document and its <title>.
func ParseHTML(r io.Reader) (body *html.Node, title string, err error) {
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

func MarkdownToHTML(src []byte) (*html.Node, error) {
	src = wikiEmbed.ReplaceAll(src, []byte("![](<$1>)"))
	var buf bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert(src, &buf); err != nil {
		return nil, err
	}
	body, _, err := ParseHTML(&buf)
	return body, err
}

func TextToHTML(src []byte) (*html.Node, error) {
	var buf strings.Builder
	for line := range strings.Lines(string(src)) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			buf.WriteString("<p>" + html.EscapeString(line) + "</p>")
		}
	}
	body, _, err := ParseHTML(strings.NewReader(buf.String()))
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

// ParseENEX streams the notes of an Evernote export to fn, one at a time.
func ParseENEX(r io.Reader, fn func(Note) error) error {
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
		body, _, err := ParseHTML(strings.NewReader(en.Content))
		if err != nil {
			return err
		}
		n := Note{
			Title: strings.TrimSpace(en.Title), Body: body, Tags: en.Tags,
			Created: enexTime(en.Created), Updated: enexTime(en.Updated), Resources: map[string]Resource{},
		}
		for _, res := range en.Resources {
			if len(res.Data) > MaxFile*4/3+MaxFile/50 { // base64 of a file over MaxFile, with line breaks
				continue
			}
			data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(res.Data), ""))
			if err != nil {
				continue
			}
			sum := md5.Sum(data)
			n.Resources[hex.EncodeToString(sum[:])] = Resource{Name: res.FileName, Mime: res.Mime, Data: data}
		}
		if err := fn(n); err != nil {
			return err
		}
	}
}

func Element(a atom.Atom, attrs ...string) *html.Node {
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

// styleHas reports whether an Element's inline style contains a declaration such as
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

// Normalize rewrites Evernote and Markdown markup into what the editor understands, then
// sanitizes it: checklists become TipTap task lists, Evernote code blocks become
// <pre><code>, and <en-media> becomes whatever media returns (nil drops it).
//
// embed may replace the src of images and the href of links (to store embedded files); an
// empty result leaves the URL as it is.
func Normalize(root *html.Node, media func(hash string) *html.Node, embed func(url string) string) {
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
			li := Element(atom.Li, "data-type", "taskItem", "data-checked", fmt.Sprint(attr(n, "checked") == "true"))
			p := Element(atom.P)
			for c := block.FirstChild; c != nil; c = block.FirstChild {
				block.RemoveChild(c)
				p.AppendChild(c)
			}
			li.AppendChild(p)
			ul := Element(atom.Ul, "data-type", "taskList")
			ul.AppendChild(li)
			block.AppendChild(ul)

		case styleHas(n, "-en-codeblock:true"):
			pre, code := Element(atom.Pre), Element(atom.Code)
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

// PlainText is the note text for listing and search, without blank lines.
func PlainText(n *html.Node) string {
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

func RenderChildren(n *html.Node) (string, error) {
	var b strings.Builder
	for c := range n.ChildNodes() {
		if err := html.Render(&b, c); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

var NoteExts = map[string]bool{".md": true, ".markdown": true, ".html": true, ".htm": true, ".txt": true, ".enex": true}

// ResolveRef turns a relative URL found in a note at dir into an archive path.
func ResolveRef(dir, ref string) string {
	ref, _, _ = strings.Cut(ref, "#")
	ref, _, _ = strings.Cut(ref, "?")
	if unescaped, err := url.PathUnescape(ref); err == nil {
		ref = unescaped
	}
	return path.Join(dir, ref)
}

func IsRelative(ref string) bool {
	u, err := url.Parse(ref)
	return err == nil && u.Scheme == "" && u.Host == "" && !strings.HasPrefix(ref, "/") && !strings.HasPrefix(ref, "#")
}

func MimeOf(name string, data []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(data)
}

// DataURI decodes an inline base64 file such as data:image/png;base64,....
func DataURI(ref string) (Resource, bool) {
	rest, ok := strings.CutPrefix(ref, "data:")
	if !ok {
		return Resource{}, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	mimeType, isBase64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isBase64 {
		return Resource{}, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) > MaxFile {
		return Resource{}, false
	}
	name := "image"
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		name += exts[0]
	}
	return Resource{Name: name, Mime: mimeType, Data: data}, true
}
