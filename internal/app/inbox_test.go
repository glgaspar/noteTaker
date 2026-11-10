package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSuggest(t *testing.T) {
	cases := []struct{ raw, id, title, details, due string }{
		{`{"title": "Renew passport", "body": "before the trip", "due": "2026-12-01T10:00:00Z", "id": 42}`, "42", "Renew passport", "before the trip", "2026-12-01"},
		{`{"subject": "Re: invoice", "text": "please pay"}`, "", "Re: invoice", "please pay", ""},
		{`{"message": "Water the plants\nthe ones outside"}`, "", "Water the plants", "the ones outside", ""},
		{"Call the bank\n\nabout the card", "", "Call the bank", "about the card", ""},
		{`{"due": "soon"}`, "", "", "", ""}, // addToInbox names it "Untitled"
	}
	for _, c := range cases {
		sg := suggest(c.raw)
		due := ""
		if sg.DueOn != nil {
			due = sg.DueOn.Format(time.DateOnly)
		}
		if sg.ExternalID != c.id || sg.Title != c.title || sg.Details != c.details || due != c.due {
			t.Errorf("suggest(%q) = %+v, due %q", c.raw, sg, due)
		}
	}
}

var hookInPage = regexp.MustCompile(`value="(http[^"]*/hooks/[^"]+)"`)

// hook creates a webhook source and returns its URL.
func (u *user) hook(name string) string {
	u.t.Helper()
	m := hookInPage.FindStringSubmatch(u.post("/tasks/sources", url.Values{"name": {name}}).body)
	if m == nil {
		u.t.Fatal("no webhook URL shown after creating it")
	}
	return m[1]
}

// send posts a payload to a webhook as another server would: no cookies, no Origin.
func send(t *testing.T, hookURL, contentType, body string) int {
	t.Helper()
	res, err := http.Post(hookURL, contentType, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func inboxCount(t *testing.T, userID int64) int64 {
	return dbInt(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND status = 'new'`, userID)
}

func TestInbox(t *testing.T) {
	a := newApp(t, false)
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	hook := ann.hook("Home Assistant")

	if code := send(t, hook, "application/json", `{"id": "a1", "title": "Take out the trash", "body": "it's Tuesday", "due": "2026-12-01"}`); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	send(t, hook, "application/json", `{"id": "a1", "title": "Take out the trash"}`) // resent: ignored
	send(t, hook, "text/plain", "Buy milk\nand eggs")
	if n := inboxCount(t, ann.ID); n != 2 {
		t.Errorf("%d inbox items, want 2", n)
	}
	for _, c := range []struct {
		url, body string
		want      int
	}{
		{hook, "  ", http.StatusBadRequest},
		{hook, strings.Repeat("x", hookMaxBody+1), http.StatusRequestEntityTooLarge},
		{a.url + "/hooks/nope", "hi", http.StatusNotFound},
	} {
		if code := send(t, c.url, "text/plain", c.body); code != c.want {
			t.Errorf("webhook with %.20q: %d, want %d", c.body, code, c.want)
		}
	}

	body := ann.get("/tasks?view=inbox").body
	if !strings.Contains(body, "Take out the trash") || !strings.Contains(body, "Buy milk") || !strings.Contains(body, "Home Assistant") {
		t.Error("the inbox should list both items and their source")
	}
	trash := dbInt(t, `SELECT id FROM inbox_items WHERE external_id = 'a1'`)
	milk := dbInt(t, `SELECT id FROM inbox_items WHERE title = 'Buy milk'`)

	// another user can't act on Ann's items or sources
	if res := ben.post(fmt.Sprintf("/tasks/inbox/%d/accept", trash), nil); res.code != http.StatusNotFound {
		t.Errorf("Ben accepting Ann's item: %d", res.code)
	}
	ben.post(fmt.Sprintf("/tasks/inbox/%d/discard", trash), nil)
	source := dbInt(t, `SELECT id FROM task_sources WHERE user_id = $1`, ann.ID)
	if res := ben.post(fmt.Sprintf("/tasks/sources/%d/token", source), nil); res.code != http.StatusNotFound {
		t.Errorf("Ben replacing Ann's webhook URL: %d", res.code)
	}
	ben.post(fmt.Sprintf("/tasks/sources/%d/delete", source), nil)
	if inboxCount(t, ann.ID) != 2 || strings.Contains(ben.get("/tasks?view=inbox").body, "Buy milk") {
		t.Error("Ben changed or saw Ann's inbox")
	}

	// accepting makes a task (with the edited title); discarding just drops the item
	ann.post(fmt.Sprintf("/tasks/inbox/%d/accept", trash), url.Values{"title": {"Trash out"}})
	ann.post(fmt.Sprintf("/tasks/inbox/%d/discard", milk), nil)
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE user_id = $1 AND title = 'Trash out' AND details = 'it''s Tuesday' AND due_on = '2026-12-01'`, ann.ID); n != 1 {
		t.Error("accepting did not create the task")
	}
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE user_id = $1`, ann.ID); n != 1 || inboxCount(t, ann.ID) != 0 {
		t.Errorf("%d tasks and %d items after accept and discard", n, inboxCount(t, ann.ID))
	}
	if res := ann.post(fmt.Sprintf("/tasks/inbox/%d/accept", trash), nil); res.code != http.StatusNotFound {
		t.Errorf("accepting twice: %d", res.code)
	}
	send(t, hook, "application/json", `{"id": "a1", "title": "Take out the trash"}`) // handled ids stay deduplicated
	if inboxCount(t, ann.ID) != 0 {
		t.Error("a handled item came back")
	}

	// a new URL replaces the old one
	newHook := hookInPage.FindStringSubmatch(ann.post(fmt.Sprintf("/tasks/sources/%d/token", source), nil).body)[1]
	if code := send(t, hook, "text/plain", "old"); code != http.StatusNotFound {
		t.Errorf("old URL after replacing it: %d", code)
	}
	if code := send(t, newHook, "text/plain", "new"); code != http.StatusAccepted {
		t.Errorf("new URL: %d", code)
	}

	// limits: calls per minute per source, and unhandled items per user
	a.s.hookRate = newLimiter(1, time.Minute)
	send(t, newHook, "text/plain", "one")
	if code := send(t, newHook, "text/plain", "two"); code != http.StatusTooManyRequests {
		t.Errorf("over the rate limit: %d", code)
	}
	a.s.hookRate = newLimiter(60, time.Minute)
	if _, err := testDB.Exec(context.Background(), `INSERT INTO inbox_items (user_id, source_id, raw, title)
		SELECT $1, $2, 'x', 'x' FROM generate_series(1, $3)`, ann.ID, source, inboxMax); err != nil {
		t.Fatal(err)
	}
	if code := send(t, newHook, "text/plain", "too many"); code != http.StatusTooManyRequests {
		t.Errorf("full inbox: %d", code)
	}

	// deleting the source deletes its items
	ann.post(fmt.Sprintf("/tasks/sources/%d/delete", source), nil)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1`, ann.ID); n != 0 {
		t.Errorf("%d items left after deleting their source", n)
	}
}

func TestSourceTags(t *testing.T) {
	a := newApp(t, false)
	ctx := context.Background()
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	t.Cleanup(func() { testDB.Exec(ctx, `DELETE FROM task_sources WHERE user_id = $1`, ann.ID) })
	m := hookInPage.FindStringSubmatch(ann.post("/tasks/sources", url.Values{"name": {"Home Assistant"}, "tags": {"Home, #chores, home, "}}).body)
	hook := m[1]
	source := dbInt(t, `SELECT id FROM task_sources WHERE user_id = $1`, ann.ID)
	itemTags := func() string {
		var tags []string
		testDB.QueryRow(ctx, `SELECT tags FROM inbox_items WHERE source_id = $1 ORDER BY id DESC LIMIT 1`, source).Scan(&tags)
		return strings.Join(tags, ",")
	}

	// without a model, items keep the source's tags (repeats dropped, first spelling kept)
	send(t, hook, "text/plain", "Water the plants")
	a.s.readPending(ctx)
	if got := itemTags(); got != "Home,chores" {
		t.Errorf("item tags %q, want Home,chores", got)
	}

	// with one, its tags follow the source's
	llm := newFakeLLM(t, func(string) (int, string) { return 200, `{"title": "Pay the bill", "tags": ["home", "bills"]}` })
	a.s.llm = LLMConfig{URL: llm.URL + "/api", Model: "m"}
	send(t, hook, "text/plain", "Electricity bill")
	a.s.readPending(ctx)
	if got := itemTags(); got != "Home,chores,bills" {
		t.Errorf("item tags with the model %q, want Home,chores,bills", got)
	}
	ann.post(fmt.Sprintf("/tasks/inbox/%d/accept", dbInt(t, `SELECT max(id) FROM inbox_items WHERE source_id = $1`, source)), nil)
	var taskTags string
	testDB.QueryRow(ctx, `SELECT string_agg(g.name, ',' ORDER BY lower(g.name)) FROM task_tag_links l JOIN task_tags g ON g.id = l.tag_id
		JOIN tasks k ON k.id = l.task_id WHERE k.user_id = $1 AND k.title = 'Pay the bill'`, ann.ID).Scan(&taskTags)
	if taskTags != "bills,chores,Home" {
		t.Errorf("accepted task tags %q", taskTags)
	}

	// changing them affects new items; only the owner can
	a.s.llm = LLMConfig{}
	ann.post(fmt.Sprintf("/tasks/sources/%d/tags", source), url.Values{"tags": {"work"}})
	send(t, hook, "text/plain", "Send the report")
	if got := itemTags(); got != "work" {
		t.Errorf("after changing the source's tags: %q", got)
	}
	if res := ben.post(fmt.Sprintf("/tasks/sources/%d/tags", source), url.Values{"tags": {"hacked"}}); res.code != http.StatusNotFound {
		t.Errorf("Ben changing Ann's source tags: %d", res.code)
	}
	if n := dbInt(t, `SELECT count(*) FROM task_sources WHERE id = $1 AND tags = '{work}'`, source); n != 1 {
		t.Error("Ann's source tags changed")
	}
}
