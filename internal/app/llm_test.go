package app

import (
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

// fakeLLM is an OpenAI-compatible chat API that answers with reply(user message), and
// records the Authorization header of each call.
type fakeLLM struct {
	*httptest.Server
	mu      sync.Mutex
	auths   []string
	systems []string // the system prompt of each call
}

func newFakeLLM(t *testing.T, reply func(user string) (int, string)) *fakeLLM {
	f := &fakeLLM{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		if len(req.Messages) > 0 {
			f.systems = append(f.systems, req.Messages[0].Content)
		}
		f.mu.Unlock()
		if r.URL.Path != "/api/chat/completions" || len(req.Messages) != 2 || req.Model == "" {
			http.Error(w, "bad request", 400)
			return
		}
		code, content := reply(req.Messages[1].Content)
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLLM) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

func itemState(t *testing.T, id int64) (state, title string) {
	t.Helper()
	if err := testDB.QueryRow(context.Background(), `SELECT parse_state, title FROM inbox_items WHERE id = $1`, id).Scan(&state, &title); err != nil {
		t.Fatal(err)
	}
	return state, title
}

func TestInboxAI(t *testing.T) {
	a := newApp(t, false)
	ctx := context.Background()
	llm := newFakeLLM(t, func(user string) (int, string) {
		if strings.Contains(user, "breaks the model") {
			return 503, "busy"
		}
		return 200, "```json\n" + `{"title": "Pay invoice 2291", "details": "R$ 300 to ACME", "due": "2026-11-20",
			"tags": ["Bills", "#bills", "work", "x", "y"], "subtasks": ["Check the amount", " ", "Pay"]}` + "\n```"
	})
	a.s.llm = LLMConfig{URL: llm.URL + "/api", Key: "server-key", Model: "haiku"}
	a.s.secrets = newSealer("test secret")
	ann := a.user("Ann", false)
	hook := ann.hook("Mail")

	send(t, hook, "application/json", `{"subject": "Invoice 2291 overdue"}`)
	id := dbInt(t, `SELECT max(id) FROM inbox_items`)
	if state, title := itemState(t, id); state != "pending" || title != "Invoice 2291 overdue" {
		t.Errorf("on arrival: %s %q, want pending with the guessed title", state, title)
	}
	if err := a.s.readPending(ctx); err != nil {
		t.Fatal(err)
	}
	if state, title := itemState(t, id); state != "done" || title != "Pay invoice 2291" {
		t.Errorf("after reading: %s %q", state, title)
	}
	if got := llm.calls(); len(got) != 1 || got[0] != "Bearer server-key" {
		t.Errorf("calls to the default model: %v", got)
	}

	// accepting brings the subtasks and tags along
	ann.post(fmt.Sprintf("/tasks/inbox/%d/accept", id), nil)
	task := dbInt(t, `SELECT id FROM tasks WHERE user_id = $1 AND parent_id IS NULL AND title = 'Pay invoice 2291' AND due_on = '2026-11-20'`, ann.ID)
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE parent_id = $1`, task); n != 2 {
		t.Errorf("%d subtasks, want 2", n)
	}
	var tags string
	testDB.QueryRow(ctx, `SELECT string_agg(g.name, ',' ORDER BY g.name) FROM task_tag_links l JOIN task_tags g ON g.id = l.tag_id WHERE l.task_id = $1`, task).Scan(&tags)
	if tags != "bills,work,x" {
		t.Errorf("tags %q, want bills,work,x (lowercase, no repeats, at most 3)", tags)
	}

	// a failing model keeps the guess, shows why, and can be retried
	send(t, hook, "text/plain", "This one breaks the model")
	id = dbInt(t, `SELECT max(id) FROM inbox_items`)
	a.s.readPending(ctx)
	if state, title := itemState(t, id); state != "failed" || title != "This one breaks the model" {
		t.Errorf("after a failure: %s %q", state, title)
	}
	if body := ann.get(fmt.Sprintf("/tasks?view=inbox&item=%d", id)).body; !strings.Contains(body, "answered 503") || !strings.Contains(body, "Retry") {
		t.Error("the opened item should show the model's error and a retry button")
	}
	ann.post(fmt.Sprintf("/tasks/inbox/%d/retry", id), nil)
	if state, _ := itemState(t, id); state != "pending" {
		t.Errorf("after retry: %s", state)
	}

	// turned off: items keep their guess and nothing is sent
	ann.post("/tasks/ai", url.Values{})
	before := len(llm.calls())
	send(t, hook, "text/plain", "Water the plants")
	a.s.readPending(ctx)
	if state, title := itemState(t, dbInt(t, `SELECT max(id) FROM inbox_items`)); state != "done" || title != "Water the plants" || len(llm.calls()) != before {
		t.Errorf("with AI off: %s %q, %d new calls", state, title, len(llm.calls())-before)
	}

	// a user's own API: their key (stored encrypted) goes there, never the server's
	own := newFakeLLM(t, func(string) (int, string) { return 200, `{"title": "From my model"}` })
	ben := a.user("Ben", false)
	ben.post("/tasks/ai", url.Values{"enabled": {"1"}, "api_url": {own.URL + "/api"}, "model": {"llama"}, "api_key": {"ben-key"}})
	var stored []byte
	testDB.QueryRow(ctx, `SELECT api_key FROM llm_settings WHERE user_id = $1`, ben.ID).Scan(&stored)
	if stored == nil || strings.Contains(string(stored), "ben-key") {
		t.Errorf("Ben's key should be stored encrypted, got %q", stored)
	}
	send(t, ben.hook("Ben's"), "text/plain", "something")
	a.s.readPending(ctx)
	if got := own.calls(); len(got) != 1 || got[0] != "Bearer ben-key" {
		t.Errorf("calls to Ben's model: %v", got)
	}
	// moving to another address drops the key
	ben.post("/tasks/ai", url.Values{"enabled": {"1"}, "api_url": {"http://elsewhere.example/v1"}, "model": {"llama"}})
	if n := dbInt(t, `SELECT count(*) FROM llm_settings WHERE user_id = $1 AND api_key IS NULL`, ben.ID); n != 1 {
		t.Error("the key followed Ben to a new address")
	}
	if res := ben.post("/tasks/ai", url.Values{"api_url": {"file:///etc/passwd"}}); !strings.Contains(res.body, "must start with http") {
		t.Error("a non-http address was accepted")
	}
	a.s.secrets = nil
	if res := ben.post("/tasks/ai", url.Values{"api_url": {own.URL}, "model": {"m"}, "api_key": {"k"}}); !strings.Contains(res.body, "SECRET_KEY") {
		t.Error("a key was accepted without SECRET_KEY")
	}
}

func TestSealer(t *testing.T) {
	s := newSealer("one")
	sealed := s.seal("hunter2")
	if got, err := s.open(sealed); err != nil || got != "hunter2" {
		t.Errorf("open: %q %v", got, err)
	}
	if _, err := newSealer("two").open(sealed); err == nil {
		t.Error("opened with the wrong key")
	}
	if newSealer("") != nil {
		t.Error("a sealer without a secret")
	}
}

func TestInboxTriage(t *testing.T) {
	a := newApp(t, false)
	ctx := context.Background()
	var invoice, annTask, benTask int64
	llm := newFakeLLM(t, func(user string) (int, string) {
		switch {
		case strings.Contains(user, "webinar"): // models tend to leave the title out of noise
			return 200, `{"kind": "noise", "reason": "marketing"}`
		case strings.Contains(user, "newsletter"):
			return 200, `{"kind": "noise", "reason": "newsletter", "title": "Read the weekly newsletter"}`
		case strings.Contains(user, "Reminder: invoice 77"):
			return 200, fmt.Sprintf(`{"kind": "repeat", "parent": "I%d", "reason": "same invoice reminder", "title": "Pay invoice 77"}`, invoice)
		case strings.Contains(user, "made-up repeat"):
			return 200, `{"kind": "repeat", "parent": "I999999", "title": "Real work"}`
		case strings.Contains(user, "Re: invoice 77"):
			return 200, fmt.Sprintf(`{"kind": "task", "parent": "I%d", "title": "Pay to the new account", "subtasks": ["dropped"]}`, invoice)
		case strings.Contains(user, "invoice 77"):
			return 200, `{"kind": "task", "title": "Pay invoice 77", "subtasks": ["Check the amount"], "tags": ["bills"]}`
		case strings.Contains(user, "login still broken"):
			return 200, fmt.Sprintf(`{"kind": "task", "parent": "t%d", "title": "Check the Safari case", "details": "on iOS"}`, annTask)
		case strings.Contains(user, "sneaky"):
			return 200, fmt.Sprintf(`{"kind": "task", "parent": "T%d", "title": "Sneaky"}`, benTask)
		}
		return 200, `{"kind": "task", "title": "Something else"}`
	})
	a.s.llm = LLMConfig{URL: llm.URL + "/api", Model: "haiku"}
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	ann.post("/tasks/ai", url.Values{"enabled": {"1"}, "notes": {"Anything from my boss is a task."}})
	ann.post("/tasks", url.Values{"title": {"Fix the login bug"}})
	ben.post("/tasks", url.Values{"title": {"Ben's task"}})
	annTask = dbInt(t, `SELECT id FROM tasks WHERE user_id = $1`, ann.ID)
	benTask = dbInt(t, `SELECT id FROM tasks WHERE user_id = $1`, ben.ID)
	hook := ann.hook("Mail")
	arrive := func(text string) int64 {
		t.Helper()
		send(t, hook, "text/plain", text)
		if err := a.s.readPending(ctx); err != nil {
			t.Fatal(err)
		}
		return dbInt(t, `SELECT max(id) FROM inbox_items`)
	}
	if state, title := itemState(t, arrive("Join our webinar")); state != "done" || title != "Join our webinar" {
		t.Errorf("noise without a title: %s %q, want done with the guessed title", state, title)
	}
	news := arrive("The weekly newsletter is here")
	invoice = arrive("Please pay invoice 77")
	reply := arrive("Re: invoice 77, we changed banks")
	// a repeat is filtered out, unless what it repeats doesn't exist
	reminder := arrive("Reminder: invoice 77 is due")
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND verdict = 'noise' AND reason = 'same invoice reminder' AND parent_item_id IS NULL`, reminder); n != 1 {
		t.Error("the reminder should be filtered out as a repeat")
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND verdict = 'task'`, arrive("a made-up repeat")); n != 1 {
		t.Error("a repeat of something that isn't there should stay a task")
	}
	arrive("The login still broken on Safari")
	sneaky := arrive("sneaky one")

	// the model sees the open tasks, the inbox, the user's notes, and only Ann's things
	system := llm.systems[len(llm.systems)-1]
	for _, want := range []string{fmt.Sprintf("T%d: Fix the login bug", annTask), fmt.Sprintf("I%d: Pay invoice 77", invoice), "Anything from my boss is a task."} {
		if !strings.Contains(system, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	if strings.Contains(system, "Ben's task") || strings.Contains(system, "weekly newsletter") {
		t.Error("the prompt shows another user's task or a filtered-out item")
	}

	// noise is filtered out: not counted, shown apart, restorable
	body := ann.get("/tasks?view=inbox").body
	if !strings.Contains(body, "3 filtered out") || !strings.Contains(body, "newsletter") {
		t.Error("the filtered-out item should be listed apart with its reason")
	}
	if !strings.Contains(body, `4 to decide on`) {
		t.Error("want 4 items to decide on: the invoice (with its reply), the Safari case, the sneaky one and the made-up repeat")
	}
	if !strings.Contains(body, "Adds a subtask to “Fix the login bug”") {
		t.Error("the Safari item should say which task it adds to")
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND parent_task_id IS NULL AND parent_item_id IS NULL`, sneaky); n != 1 {
		t.Error("an item was attached to another user's task")
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND parent_item_id = $2 AND subtasks = '{}'`, reply, invoice); n != 1 {
		t.Error("the reply should sit under the invoice, without subtasks of its own")
	}
	ann.post(fmt.Sprintf("/tasks/inbox/%d/restore", news), nil)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND verdict = 'task'`, news); n != 1 {
		t.Error("restore did not bring the item back")
	}
	ann.post(fmt.Sprintf("/tasks/inbox/%d/discard", news), nil)

	// accepting the invoice brings the reply along as a subtask
	ann.post(fmt.Sprintf("/tasks/inbox/%d/accept", invoice), nil)
	task := dbInt(t, `SELECT id FROM tasks WHERE user_id = $1 AND title = 'Pay invoice 77' AND parent_id IS NULL`, ann.ID)
	var subs string
	testDB.QueryRow(ctx, `SELECT string_agg(title, ',' ORDER BY id) FROM tasks WHERE parent_id = $1`, task).Scan(&subs)
	if subs != "Check the amount,Pay to the new account" {
		t.Errorf("subtasks %q", subs)
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND status = 'accepted'`, reply); n != 1 {
		t.Error("the reply stayed in the inbox")
	}

	// accept all: the Safari item joins the open task, untagged
	ann.post("/tasks/inbox/accept", nil)
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE parent_id = $1 AND title = 'Check the Safari case' AND details = 'on iOS'`, annTask); n != 1 {
		t.Error("the Safari item should be a subtask of the login task")
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND status = 'new' AND verdict = 'task'`, ann.ID); n != 0 {
		t.Errorf("%d items left after accepting all", n)
	}

	// what the user accepted and discarded is fed back to the model
	arrive("anything")
	system = llm.systems[len(llm.systems)-1]
	if !strings.Contains(system, "Recently accepted as tasks:\nSneaky") || !strings.Contains(system, "Recently discarded by the person (similar things are probably noise):\nRead the weekly newsletter") {
		t.Errorf("the prompt lacks the user's recent choices:\n%s", system)
	}

	// discarding an item lets the items under it go; noise expires after 14 days
	parent := dbInt(t, `SELECT max(id) FROM inbox_items`)
	child := arrive("child")
	testDB.Exec(ctx, `UPDATE inbox_items SET parent_item_id = $1 WHERE id = $2`, parent, child)
	ann.post(fmt.Sprintf("/tasks/inbox/%d/discard", parent), nil)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND status = 'new' AND parent_item_id IS NULL`, child); n != 1 {
		t.Error("discarding the parent should leave the child at the top")
	}
	old := arrive("old newsletter")
	testDB.Exec(ctx, `UPDATE inbox_items SET received_at = now() - interval '15 days' WHERE id = $1`, old)
	if err := a.s.expireNoise(ctx); err != nil {
		t.Fatal(err)
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE id = $1 AND status = 'discarded' AND raw = ''`, old); n != 1 {
		t.Error("old noise was not discarded")
	}
}
