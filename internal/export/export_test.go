package export

import (
	"strings"
	"testing"
	"time"
)

func TestHTML(t *testing.T) {
	doc := `{"type":"doc","content":[
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Plan <b>"}]},
		{"type":"paragraph","content":[
			{"type":"text","text":"bold","marks":[{"type":"bold"}]},{"type":"text","text":" and "},
			{"type":"text","text":"link","marks":[{"type":"link","attrs":{"href":"/files/7"}}]},
			{"type":"text","text":" bad","marks":[{"type":"link","attrs":{"href":"javascript:alert(1)"}}]}]},
		{"type":"taskList","content":[
			{"type":"taskItem","attrs":{"checked":true},"content":[{"type":"paragraph","content":[{"type":"text","text":"done"}]}]},
			{"type":"taskItem","attrs":{"checked":false},"content":[{"type":"paragraph","content":[{"type":"text","text":"open"}]}]}]},
		{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"if a < b {}"}]},
		{"type":"image","attrs":{"src":"/files/8","alt":"pic"}},
		{"type":"orderedList","attrs":{"start":3},"content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"three"}]}]}]}
	]}`
	link := func(u string) string { return strings.Replace(u, "/files/", "attachments/", 1) }
	got, err := HTML([]byte(doc), link)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<h2>Plan &lt;b&gt;</h2>",
		"<strong>bold</strong>",
		`<a href="attachments/7">link</a>`,
		`<li><input type="checkbox" disabled checked>done`,
		`<pre><code class="language-go">if a &lt; b {}</code></pre>`,
		`<img src="attachments/8" alt="pic">`,
		`<ol start="3">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("HTML lacks %s\n%s", want, got)
		}
	}
	if strings.Contains(got, "javascript:") {
		t.Error("unsafe link kept")
	}

	md, err := Markdown(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Plan &lt;b&gt;", "**bold**", "[link](attachments/7)", "- [x] done", "- [ ] open", "```go\nif a < b {}\n```", "![pic](attachments/8)", "3. three"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q\n%s", want, md)
		}
	}
}

func TestRewriteHTML(t *testing.T) {
	got, err := RewriteHTML(`<p><img src="/files/1"><a href="javascript:x()">x</a></p>`, func(u string) string { return strings.Replace(u, "/files/", "a/", 1) })
	if err != nil {
		t.Fatal(err)
	}
	if got != `<p><img src="a/1"/><a href="">x</a></p>` {
		t.Errorf("got %s", got)
	}
}

func TestFrontMatterAndPage(t *testing.T) {
	when := time.Date(2024, 2, 10, 15, 30, 0, 0, time.UTC)
	fm := FrontMatter(Meta{Title: `Say "hi": now`, Tags: []string{"a b"}, Created: when, Updated: when})
	want := "---\ntitle: \"Say \\\"hi\\\": now\"\ntags: [\"a b\"]\ncreated: 2024-02-10T15:30:00Z\nupdated: 2024-02-10T15:30:00Z\n---\n\n"
	if fm != want {
		t.Errorf("front matter:\n%s", fm)
	}
	page := Page(Meta{Title: "<script>x</script>", Updated: when}, "<p>body</p>")
	if strings.Contains(page, "<script>x") {
		t.Error("title not escaped in page")
	}
}

func TestNames(t *testing.T) {
	if got := SafeName(` a/b:c*? `); got != "a-b-c--" {
		t.Errorf("SafeName = %q", got)
	}
	if SafeName("...") != "Untitled" {
		t.Error("empty name not replaced")
	}
	n := Names{}
	for i, want := range []string{"d/Note.md", "d/Note (2).md", "d/Note (3).md"} {
		base := []string{"Note", "note", "NOTE"}[i] // differ only in case
		if got := n.Unique("d", base, ".md"); !strings.EqualFold(got, want) {
			t.Errorf("Unique = %q, want %q", got, want)
		}
	}
	if PathURL("a b/c#d.png") != "a%20b/c%23d.png" {
		t.Errorf("PathURL = %q", PathURL("a b/c#d.png"))
	}
}
