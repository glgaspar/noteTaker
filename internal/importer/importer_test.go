package importer

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func normalized(t *testing.T, src string) string {
	t.Helper()
	body, _, err := ParseHTML(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	Normalize(body, func(string) *html.Node { return nil }, nil)
	out, err := RenderChildren(body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNormalize(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"legacy evernote todo keeps its text",
			`<div><en-todo checked="true"/>Buy milk</div>`,
			`<div><ul data-type="taskList"><li data-type="taskItem" data-checked="true"><p>Buy milk</p></li></ul></div>`},
		{"current evernote checklist",
			`<ul style="--en-todo:true;"><li style="--en-checked:false;">Open</li></ul>`,
			`<ul data-type="taskList"><li data-type="taskItem" data-checked="false">Open</li></ul>`},
		{"evernote code block",
			`<div style="-en-codeblock:true;"><div>a := 1</div><div>b := 2</div></div>`,
			"<pre><code>a := 1\nb := 2</code></pre>"},
		{"markdown task item",
			`<ul><li><input type="checkbox" checked disabled> done</li></ul>`,
			`<ul data-type="taskList"><li data-type="taskItem" data-checked="true"> done</li></ul>`},
		{"scripts, handlers and unsafe links are removed",
			`<p onclick="x()">Hi <a href="javascript:alert(1)">a</a><a href="https://ok.example">b</a></p><script>x()</script><iframe></iframe>`,
			`<p>Hi <a>a</a><a href="https://ok.example">b</a></p>`},
		{"code language is kept, other classes are not",
			`<pre class="x"><code class="language-go">x</code></pre>`,
			`<pre><code class="language-go">x</code></pre>`},
		{"missing en-media is dropped with what follows kept",
			`<div><en-media hash="abc"/>after</div>`,
			`<div>after</div>`},
	}
	for _, c := range cases {
		if got := normalized(t, c.in); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestImportHelpers(t *testing.T) {
	if got := ResolveRef("notes/daily", "../img/a%20b.png#x"); got != "notes/img/a b.png" {
		t.Errorf("ResolveRef = %q", got)
	}
	for ref, want := range map[string]bool{"img/a.png": true, "../a.pdf": true, "https://x/a.png": false, "/files/1": false, "#top": false, "data:x": false} {
		if IsRelative(ref) != want {
			t.Errorf("IsRelative(%q) = %v", ref, !want)
		}
	}
	res, ok := DataURI("data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png!")))
	if !ok || res.Mime != "image/png" || string(res.Data) != "png!" {
		t.Errorf("DataURI = %+v %v", res, ok)
	}
	body, err := MarkdownToHTML([]byte("![[logo.png|100]]"))
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := RenderChildren(body); !strings.Contains(out, `<img src="logo.png"`) {
		t.Errorf("wiki embed not converted: %s", out)
	}
}

func TestSplitFrontMatter(t *testing.T) {
	fm, body := SplitFrontMatter([]byte("---\ntitle: \"Say \\\"hi\\\"\"\ntags: [\"a b\", \"c\"]\ncreated: 2024-02-10T15:30:00Z\nupdated: 2024-03-01\n---\n\n# Body\n"))
	if fm.Title != `Say "hi"` || len(fm.Tags) != 2 || fm.Tags[0] != "a b" || fm.Created == nil || fm.Updated == nil || string(body) != "# Body\n" {
		t.Errorf("ours: %+v %q", fm, body)
	}
	fm, body = SplitFrontMatter([]byte("---\ntitle: Plain title\ntags:\n  - work\n  - '#idea'\n---\ntext"))
	if fm.Title != "Plain title" || len(fm.Tags) != 2 || fm.Tags[1] != "idea" || string(body) != "text" {
		t.Errorf("obsidian: %+v %q", fm, body)
	}
	if _, body := SplitFrontMatter([]byte("no front matter\n---\n")); string(body) != "no front matter\n---\n" {
		t.Error("text without front matter changed")
	}
}
