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
	mu    sync.Mutex
	auths []string
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
	if body := ann.get("/tasks?view=inbox").body; !strings.Contains(body, "answered 503") || !strings.Contains(body, "Retry") {
		t.Error("the inbox should show the model's error and a retry button")
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
