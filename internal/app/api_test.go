package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestJSONPath(t *testing.T) {
	var doc any
	json.Unmarshal([]byte(`{"data": {"items": [{"id": 1}, {"id": 2}]}, "n": 3}`), &doc)
	for _, c := range []struct {
		path string
		want string
		ok   bool
	}{
		{"", `map[data:map[items:[map[id:1] map[id:2]]] n:3]`, true},
		{"data.items.1.id", "2", true},
		{"data.items.5", "<nil>", false},
		{"n.x", "<nil>", false},
		{"missing", "<nil>", false},
	} {
		got, ok := jsonPath(doc, c.path)
		if fmt.Sprint(got) != c.want || ok != c.ok {
			t.Errorf("jsonPath(%q) = %v, %v", c.path, got, ok)
		}
	}
}

// fakeAPI serves body (changeable) to requests carrying "Authorization: Bearer t".
type fakeAPI struct {
	*httptest.Server
	mu   sync.Mutex
	body string
	code int
}

func newFakeAPI(t *testing.T, body string) *fakeAPI {
	f := &fakeAPI{body: body, code: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "who are you", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.code)
		w.Write([]byte(f.body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) set(code int, body string) {
	f.mu.Lock()
	f.code, f.body = code, body
	f.mu.Unlock()
}

func TestAPISources(t *testing.T) {
	a := newApp(t, false)
	a.s.secrets = newSealer("test secret")
	ctx := context.Background()
	api := newFakeAPI(t, `{"data": {"items": [{"id": 1, "title": "Old one"}, {"id": 2, "title": "Old two"}]}}`)
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	t.Cleanup(func() { testDB.Exec(ctx, `DELETE FROM task_sources WHERE user_id = $1`, ann.ID) })
	form := func(name, path, headers string, extra ...string) url.Values {
		v := url.Values{"name": {name}, "url": {api.URL + "/tasks"}, "items_path": {path}, "id_field": {"id"}, "every": {"15"}, "headers": {headers}}
		for i := 0; i+1 < len(extra); i += 2 {
			v.Set(extra[i], extra[i+1])
		}
		return v
	}
	items := func(source int64, status string) string {
		rows, _ := testDB.Query(ctx, `SELECT title FROM inbox_items WHERE source_id = $1 AND status = $2 ORDER BY id`, source, status)
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}

	for _, c := range []struct {
		form url.Values
		want string
	}{
		{form("X", "data.items", ""), "answered 401"},
		{form("X", "data.nope", "Authorization: Bearer t"), "nothing at"},
		{form("X", "data.items", "Authorization Bearer t"), "Name: value"},
		{form("X", "data.items", "", "every", "1"), "every 5 minutes"},
		{form("X", "data.items", "", "url", "ftp://example.com"), "http://"},
	} {
		if res := ann.post("/tasks/apis", c.form); !strings.Contains(res.body, c.want) {
			t.Errorf("adding with %v: want %q in the page (status %d)", c.form, c.want, res.code)
		}
	}
	if n := dbInt(t, `SELECT count(*) FROM task_sources WHERE user_id = $1`, ann.ID); n != 0 {
		t.Fatalf("%d sources added by failed attempts", n)
	}

	// added without importing: what is listed now is only noted
	if res := ann.post("/tasks/apis", form("Tracker", "data.items", "Authorization: Bearer t")); res.code != http.StatusSeeOther {
		t.Fatalf("add API: %d %.300s", res.code, res.body)
	}
	tracker := dbInt(t, `SELECT id FROM task_sources WHERE user_id = $1 AND name = 'Tracker'`, ann.ID)
	if got := items(tracker, "new"); got != "" {
		t.Errorf("existing records were imported: %s", got)
	}
	var stored []byte
	testDB.QueryRow(ctx, `SELECT headers FROM api_sources WHERE source_id = $1`, tracker).Scan(&stored)
	if len(stored) == 0 || bytes.Contains(stored, []byte("Bearer")) {
		t.Error("headers should be stored encrypted")
	}

	// new records arrive once, through the scheduled check
	api.set(200, `{"data": {"items": [{"id": 1, "title": "Old one"}, {"id": 2, "title": "Old two"}, {"id": 3, "title": "Fix the gate", "body": "it squeaks"}]}}`)
	testDB.Exec(ctx, `UPDATE api_sources SET checked_at = now() - interval '20 minutes'`)
	if err := a.s.checkDueAPIs(ctx); err != nil {
		t.Fatal(err)
	}
	a.s.checkAPINow(ctx, tracker)
	if got := items(tracker, "new"); got != "Fix the gate" {
		t.Errorf("after new records: %q", got)
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE source_id = $1 AND details = 'it squeaks' AND parse_state = 'pending'`, tracker); n != 1 {
		t.Error("the new record's details or AI reading are missing")
	}

	// a second API, importing what it lists; records without an id are told apart by content
	if res := ann.post("/tasks/apis", form("Chores", "", "Authorization: Bearer t", "id_field", "", "import", "1")); res.code != http.StatusSeeOther {
		t.Fatalf("add second API: %d %.300s", res.code, res.body)
	}
	chores := dbInt(t, `SELECT id FROM task_sources WHERE user_id = $1 AND name = 'Chores'`, ann.ID)
	api.set(200, `[{"title": "Water plants"}, {"title": "Water plants"}, {"title": "Feed cat"}]`)
	a.s.checkAPINow(ctx, chores)
	a.s.checkAPINow(ctx, chores)
	if got := items(chores, "new"); !strings.Contains(got, "Water plants, Feed cat") || strings.Count(got, "Water plants") != 1 {
		t.Errorf("records without ids: %q", got)
	}

	// edit: empty headers keep the saved ones; a broken edit changes nothing
	if res := ann.post(fmt.Sprintf("/tasks/apis/%d/edit", chores), form("House", "", "", "id_field", "")); res.code != http.StatusSeeOther {
		t.Errorf("edit keeping headers: %d %.300s", res.code, res.body)
	}
	if res := ann.post(fmt.Sprintf("/tasks/apis/%d/edit", chores), form("Broken", "", "", "url", api.URL+"/x", "clear_headers", "1")); !strings.Contains(res.body, "answered 401") {
		t.Error("an edit that can't be read should be refused")
	}
	if n := dbInt(t, `SELECT count(*) FROM task_sources s JOIN api_sources p ON p.source_id = s.id WHERE s.id = $1 AND s.name = 'House' AND p.headers IS NOT NULL`, chores); n != 1 {
		t.Error("the edits were not kept as expected")
	}

	// failures and limits show on the page
	api.set(500, "down")
	a.s.checkAPINow(ctx, tracker)
	api.set(200, `[`+strings.Repeat(`{"title": "x"},`, apiMaxBody/14)+`{}]`)
	a.s.checkAPINow(ctx, chores)
	body := ann.get("/tasks/sources").body
	if !strings.Contains(body, "answered 500") || !strings.Contains(body, "over 1 MB") {
		t.Error("the sources page should show both failed checks")
	}

	// API sources are private
	for _, p := range []string{"/edit", "/check"} {
		if res := ben.post(fmt.Sprintf("/tasks/apis/%d%s", tracker, p), form("Mine", "", "")); res.code != http.StatusNotFound {
			t.Errorf("Ben POST %s on Ann's API: %d", p, res.code)
		}
	}
	if strings.Contains(ben.get("/tasks/sources").body, api.URL) {
		t.Error("Ben sees Ann's APIs")
	}
}
