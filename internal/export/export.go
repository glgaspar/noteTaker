// Package export turns notes into HTML and Markdown files.
//
// Notes are stored as TipTap (ProseMirror) JSON; HTML renders that JSON, and Markdown is
// converted from the HTML, so both formats share one renderer. Imported notes nobody opened
// yet only have HTML, which joins the same path.
package export

import (
	"cmp"
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/JohannesKaufmann/dom"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	xhtml "golang.org/x/net/html"
)

// Link rewrites the URL of an image or link, e.g. /files/12 to attachments/photo.png.
type Link func(url string) string

type node struct {
	Type    string         `json:"type"`
	Attrs   map[string]any `json:"attrs"`
	Content []node         `json:"content"`
	Text    string         `json:"text"`
	Marks   []struct {
		Type  string         `json:"type"`
		Attrs map[string]any `json:"attrs"`
	} `json:"marks"`
}

func (n node) attr(key string) string {
	switch v := n.Attrs[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// safeURL keeps links that cannot run code in the exported file: relative paths, http(s),
// mailto, and embedded files (data:) other than HTML.
func safeURL(u string) bool {
	lower := strings.ToLower(strings.TrimSpace(u))
	scheme, rest, found := strings.Cut(lower, ":")
	if !found || strings.ContainsAny(scheme, "/?#") {
		return true // relative
	}
	switch scheme {
	case "http", "https", "mailto":
		return true
	case "data":
		return !strings.HasPrefix(rest, "text/html")
	}
	return false
}

// HTML renders a TipTap JSON document.
func HTML(doc []byte, link Link) (string, error) {
	var root node
	if err := json.Unmarshal(doc, &root); err != nil {
		return "", err
	}
	var b strings.Builder
	render(&b, root, link)
	return b.String(), nil
}

func render(b *strings.Builder, n node, link Link) {
	children := func() {
		for _, c := range n.Content {
			render(b, c, link)
		}
	}
	wrap := func(open, close string) {
		b.WriteString(open)
		children()
		b.WriteString(close)
	}
	switch n.Type {
	case "text":
		renderText(b, n, link)
	case "paragraph":
		wrap("<p>", "</p>\n")
	case "heading":
		level, _ := strconv.Atoi(n.attr("level"))
		level = min(max(level, 1), 6)
		wrap(fmt.Sprintf("<h%d>", level), fmt.Sprintf("</h%d>\n", level))
	case "bulletList":
		wrap("<ul>\n", "</ul>\n")
	case "orderedList":
		if start := n.attr("start"); start != "" && start != "1" {
			wrap(`<ol start="`+html.EscapeString(start)+`">`+"\n", "</ol>\n")
		} else {
			wrap("<ol>\n", "</ol>\n")
		}
	case "listItem":
		wrap("<li>", "</li>\n")
	case "taskList":
		wrap(`<ul class="tasks">`+"\n", "</ul>\n")
	case "taskItem":
		box := `<input type="checkbox" disabled>`
		if n.Attrs["checked"] == true {
			box = `<input type="checkbox" disabled checked>`
		}
		// The checkbox goes inside the first paragraph so Markdown gets "- [x] text".
		b.WriteString("<li>")
		for i, c := range n.Content {
			if i == 0 && c.Type == "paragraph" {
				b.WriteString(box)
				for _, t := range c.Content {
					render(b, t, link)
				}
				b.WriteString("\n")
				continue
			}
			if i == 0 {
				b.WriteString(box)
			}
			render(b, c, link)
		}
		b.WriteString("</li>\n")
	case "blockquote":
		wrap("<blockquote>\n", "</blockquote>\n")
	case "codeBlock":
		class := ""
		if lang := n.attr("language"); lang != "" {
			class = ` class="language-` + html.EscapeString(lang) + `"`
		}
		b.WriteString("<pre><code" + class + ">")
		for _, c := range n.Content {
			b.WriteString(html.EscapeString(c.Text))
		}
		b.WriteString("</code></pre>\n")
	case "horizontalRule":
		b.WriteString("<hr>\n")
	case "hardBreak":
		b.WriteString("<br>")
	case "image":
		src := link(n.attr("src"))
		if safeURL(src) {
			fmt.Fprintf(b, `<p><img src="%s" alt="%s"></p>`+"\n", html.EscapeString(src), html.EscapeString(n.attr("alt")))
		}
	default:
		children()
	}
}

func renderText(b *strings.Builder, n node, link Link) {
	var open, close []string
	for _, m := range n.Marks {
		tag := map[string]string{"bold": "strong", "italic": "em", "strike": "s", "code": "code", "underline": "u"}[m.Type]
		switch {
		case tag != "":
			open, close = append(open, "<"+tag+">"), append([]string{"</" + tag + ">"}, close...)
		case m.Type == "link":
			href, _ := m.Attrs["href"].(string)
			if href = link(href); safeURL(href) {
				open, close = append(open, `<a href="`+html.EscapeString(href)+`">`), append([]string{"</a>"}, close...)
			}
		}
	}
	b.WriteString(strings.Join(open, "") + html.EscapeString(n.Text) + strings.Join(close, ""))
}

// RewriteHTML applies link to the images and links of stored HTML (imported notes), dropping
// URLs that are not safe in an exported file.
func RewriteHTML(src string, link Link) (string, error) {
	body, err := xhtml.Parse(strings.NewReader(src))
	if err != nil {
		return "", err
	}
	var content *xhtml.Node
	for n := range body.Descendants() {
		if n.Data == "body" {
			content = n
		}
		for i, a := range n.Attr {
			if (n.Data == "img" && a.Key == "src") || (n.Data == "a" && a.Key == "href") {
				if u := link(a.Val); safeURL(u) {
					n.Attr[i].Val = u
				} else {
					n.Attr[i].Val = ""
				}
			}
		}
	}
	var b strings.Builder
	for c := range content.ChildNodes() {
		if err := xhtml.Render(&b, c); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

var markdown = func() *converter.Converter {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), strikethrough.NewStrikethroughPlugin(), table.NewTablePlugin(),
	))
	// Checklists: <input type="checkbox"> becomes "[x]" / "[ ]" (the default drops it).
	conv.Register.RendererFor("input", converter.TagTypeInline, func(_ converter.Context, w converter.Writer, n *xhtml.Node) converter.RenderStatus {
		if dom.GetAttributeOr(n, "type", "") != "checkbox" {
			return converter.RenderTryNext
		}
		if _, checked := dom.GetAttribute(n, "checked"); checked {
			w.WriteString("[x] ")
		} else {
			w.WriteString("[ ] ")
		}
		return converter.RenderSuccess
	}, converter.PriorityEarly)
	return conv
}()

// Markdown converts note HTML to Markdown.
func Markdown(noteHTML string) (string, error) {
	return markdown.ConvertString(noteHTML)
}

// Meta is what an exported file says about its note.
type Meta struct {
	Title            string
	Tags             []string
	Created, Updated time.Time
}

// FrontMatter is the YAML header of an exported Markdown file. Values are JSON, which is
// valid YAML, so no escaping rules of our own are needed.
func FrontMatter(m Meta) string {
	q := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return fmt.Sprintf("---\ntitle: %s\ntags: %s\ncreated: %s\nupdated: %s\n---\n\n",
		q(m.Title), q(tags), m.Created.UTC().Format(time.RFC3339), m.Updated.UTC().Format(time.RFC3339))
}

// Page wraps note HTML in a standalone document.
func Page(m Meta, body string) string {
	var tags string
	for _, t := range m.Tags {
		tags += `<span class="tag">#` + html.EscapeString(t) + `</span>`
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(cmp.Or(m.Title, "Untitled")) + `</title><style>` + pageCSS + `</style></head>
<body><article><h1 class="title">` + html.EscapeString(cmp.Or(m.Title, "Untitled")) + `</h1>
<p class="meta">Updated ` + m.Updated.Format("Jan 2, 2006") + tags + `</p>
` + body + `</article></body></html>
`
}

const pageCSS = `body{margin:0;background:#fff;color:#2b2925;font:16px/1.65 system-ui,sans-serif}
article{max-width:46rem;margin:0 auto;padding:3rem 1.5rem}
h1.title{font-size:2rem;margin:0}.meta{color:#73706a;font-size:.85rem;margin:.25rem 0 2rem}
.tag{margin-left:.5rem;background:#f1f0ec;border-radius:1rem;padding:.05rem .5rem}
a{color:#5b5bd6}img{max-width:100%}blockquote{margin:0;padding-left:1rem;border-left:3px solid #ebe9e4;color:#73706a}
pre{background:#1f2328;color:#e6edf3;padding:1rem;border-radius:.5rem;overflow:auto}
code{font-family:ui-monospace,monospace;font-size:.9em}:not(pre)>code{background:#f1f0ec;padding:.1rem .3rem;border-radius:.25rem}
ul.tasks{list-style:none;padding-left:0}ul.tasks input{margin-right:.5rem}
@media (prefers-color-scheme:dark){body{background:#1a1a1a;color:#e8e5df}.meta,blockquote{color:#a5a199}
.tag,:not(pre)>code{background:#2b2b2b}a{color:#8e8ef5}}`

// IndexPage lists exported notes; entries are (relative path, title).
func IndexPage(title string, entries [][2]string) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, `<li><a href="%s">%s</a></li>`, html.EscapeString(PathURL(e[0])), html.EscapeString(e[1]))
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>` + html.EscapeString(title) +
		`</title><style>` + pageCSS + `</style></head><body><article><h1 class="title">` + html.EscapeString(title) +
		`</h1><p class="meta">` + strconv.Itoa(len(entries)) + ` notes</p><ul>` + b.String() + `</ul></article></body></html>`
}
