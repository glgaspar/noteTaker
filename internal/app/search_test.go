package app

import (
	"context"
	"fmt"
	"regexp"
	"testing"
)

var resultTitle = regexp.MustCompile(`class="font-medium">([^<]*)</div>`)

func (u *user) search(ws int64, query string) []string {
	u.t.Helper()
	res := u.get(fmt.Sprintf("/w/%d/search?%s", ws, query))
	if res.code != 200 {
		u.t.Fatalf("search %q: %d", query, res.code)
	}
	var titles []string
	for _, m := range resultTitle.FindAllStringSubmatch(res.body, -1) {
		titles = append(titles, m[1])
	}
	return titles
}

func TestSearch(t *testing.T) {
	a := newApp(t, false)
	ann := a.user("Ann", false)
	ws := ann.workspace("W")
	work := ann.folder(ws, "Work", 0)
	deep := ann.folder(ws, "Deep", work)
	ann.note(ws, work, "Plano de ação", "Precisamos definir a ação principal")
	ann.note(ws, deep, "Project kickoff", "The project starts monday")
	ann.note(ws, 0, "Go snippet", `func main() { fmt.Println("hello") }`)

	cases := []struct {
		query string
		want  []string
	}{
		{"q=acao", []string{"Plano de ação"}},                                          // accents ignored
		{"q=proj", []string{"Project kickoff"}},                                        // prefix match
		{"q=Println", []string{"Go snippet"}},                                          // words inside code
		{"q=fmt.Println", []string{"Go snippet"}},                                      // the same, typed as code
		{"q=kick%27%29%7C%21%28", []string{"Project kickoff"}},                         // tsquery syntax is not injected
		{"title=kick", []string{"Project kickoff"}},                                    // title filter
		{"title=%25", nil},                                                             // LIKE wildcards are literal
		{fmt.Sprintf("folder=%d", work), []string{"Project kickoff", "Plano de ação"}}, // includes subfolders
	}
	for _, c := range cases {
		if got := ann.search(ws, c.query); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v, want %v", c.query, got, c.want)
		}
	}
}

func TestSearchPagingAndScope(t *testing.T) {
	a := newApp(t, false)
	ann := a.user("Ann", false)
	ws := ann.workspace("Mine")
	if _, err := testDB.Exec(context.Background(), `
		INSERT INTO notes (workspace_id, title, content_text, author_id)
		SELECT $1, 'Bulk ' || g, 'zebra ' || g, $2 FROM generate_series(1, 120) g`, ws, ann.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	first := ann.get(fmt.Sprintf("/w/%d/search?q=zebra", ws))
	for page := 0; ; page++ {
		res := first
		if page > 0 {
			res = ann.get(fmt.Sprintf("/w/%d/search?q=zebra&page=%d", ws, page))
		}
		matches := resultTitle.FindAllStringSubmatch(res.body, -1)
		if len(matches) == 0 {
			break
		}
		for _, m := range matches {
			seen[m[1]] = true
		}
	}
	if len(seen) != 120 {
		t.Errorf("paging returned %d distinct notes, want 120", len(seen))
	}

	// "All workspaces" covers every workspace the user can access, and nothing else.
	ben := a.user("Ben", false)
	other := ben.workspace("Ben's")
	ben.note(other, 0, "Ben private", "zebra private")
	if got := ann.search(ws, "q=private&all=1"); len(got) != 0 {
		t.Errorf("Ann found Ben's private note: %v", got)
	}
	ben.share(fmt.Sprintf("/w/%d/members", other), ann.Email, "viewer")
	if got := ann.search(ws, "q=private&all=1"); fmt.Sprint(got) != "[Ben private]" {
		t.Errorf("all-workspace search after sharing: %v", got)
	}
}
