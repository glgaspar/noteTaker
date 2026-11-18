package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A language model turns inbox items into tasks. It is any OpenAI-compatible chat API:
// Open WebUI (http://host:3024/api), Ollama (http://host:11434/v1), or a hosted one.

// LLMConfig is a chat API; the server's default comes from the LLM_* variables.
type LLMConfig struct {
	URL   string // base URL; /chat/completions is appended
	Key   string // bearer token, if the API needs one
	Model string
}

var llmClient = &http.Client{Timeout: 90 * time.Second}

// chat sends one system and one user message and returns the answer's text.
func (c LLMConfig) chat(ctx context.Context, system, user string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model": c.Model, "stream": false,
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	res, err := llmClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the model's API answered %d: %.200s", res.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("unexpected answer from the model's API: %.200s", raw)
	}
	return out.Choices[0].Message.Content, nil
}

const taskPrompt = `You sort what a person received (an email, a webhook call, an API record) before it reaches their to-do inbox. They receive far too much and want to see only what they have to act on.
Answer with JSON only, no markdown, exactly in this shape:
{"kind": "task", "reason": "", "parent": "", "title": "", "details": "", "due": "", "tags": [], "subtasks": []}
- kind: "task" when the person has to do something about it (reply, decide, pay, fix, attend, review, deliver). "noise" when not: newsletters, marketing, automatic notifications and receipts, FYIs and copies that need no action, confirmations of things already done. "repeat" when it asks for the same thing as one of the open tasks or inbox items listed below and adds nothing new to do (a reminder, a resent alert, a duplicate). When unsure, "task".
- reason: for noise or a repeat, a few words on why (for example "newsletter", "automatic build notification" or "same Sanofi portal alert again"); otherwise "".
- parent: for a repeat, the code of what it repeats. For a task that is clearly a new step in, a follow-up to or a reply about one of the open tasks or inbox items listed below, its code (for example "T12" or "I40"). Otherwise "".
- title: always, even for noise: the action to take, short and imperative (at most 80 characters), in the language of the message.
- details: the context needed to do it (who, what, amounts, links, references), brief plain text. Empty if the title says it all.
- due: a deadline as YYYY-MM-DD, only if the message states or clearly implies one; otherwise "".
- tags: 0 to 3 short lowercase tags. Prefer the existing ones when they fit.
- subtasks: separate steps only when the message clearly lists them, at most 5; [] when parent is set.`

// triageContext is what the model knows about the user when it reads an item.
type triageContext struct {
	Today     time.Time
	Notes     string   // the user's own words on what matters
	Tags      []string // existing tag names
	Tasks     []string // open top-level tasks, as "T<id>: title"
	Items     []string // other items waiting in the inbox, as "I<id>: title"
	Accepted  []string // titles the user recently accepted
	Discarded []string // and discarded
}

func (c triageContext) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Today is %s.", c.Today.Format("Monday, 2006-01-02"))
	if c.Notes != "" {
		fmt.Fprintf(&b, "\n\nWhat the person says matters to them (follow it):\n%s", c.Notes)
	}
	list := func(title string, items []string) {
		if len(items) > 0 {
			fmt.Fprintf(&b, "\n\n%s:\n%s", title, strings.Join(items, "\n"))
		}
	}
	if len(c.Tags) > 0 {
		fmt.Fprintf(&b, "\n\nExisting tags: %s", strings.Join(c.Tags, ", "))
	}
	list("Open tasks", c.Tasks)
	list("Items already in the inbox", c.Items)
	list("Recently accepted as tasks", c.Accepted)
	list("Recently discarded by the person (similar things are probably noise)", c.Discarded)
	return b.String()
}

// reading is what the model made of an item.
type reading struct {
	suggestion
	Tags, Subtasks []string
	Noise          bool
	Repeat         bool // noise because it repeats its parent
	Reason         string
	ParentTask     int64 // an open task it belongs to
	ParentItem     int64 // or another inbox item
}

// maxLLMInput limits how much of an item is sent to the model.
const maxLLMInput = 12000

// readTask asks the model what raw is, given what it knows about the user.
func (c LLMConfig) readTask(ctx context.Context, raw string, tc triageContext) (reading, error) {
	if r := []rune(raw); len(r) > maxLLMInput {
		raw = string(r[:maxLLMInput]) + "\n[…cut]"
	}
	answer, err := c.chat(ctx, taskPrompt+"\n\n"+tc.String(), raw)
	if err != nil {
		return reading{}, err
	}
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return reading{}, fmt.Errorf("the model answered without JSON: %.200s", answer)
	}
	var out struct {
		Kind, Reason, Parent, Title, Details, Due string
		Tags, Subtasks                            []string
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &out); err != nil {
		return reading{}, fmt.Errorf("the model's JSON is invalid: %v", err)
	}
	rd := reading{suggestion: suggestion{Title: strings.TrimSpace(out.Title), Details: strings.TrimSpace(out.Details), DueOn: parseDue(out.Due)},
		Tags: cleanList(out.Tags, 3, true), Subtasks: cleanList(out.Subtasks, 5, false),
		Reason: strings.TrimSpace(out.Reason)}
	switch strings.ToLower(strings.TrimSpace(out.Kind)) {
	case "noise":
		rd.Noise = true
	case "repeat":
		rd.Noise, rd.Repeat = true, true
	}
	if r := []rune(rd.Title); len(r) > 200 {
		rd.Title = string(r[:200]) + "…"
	}
	if r := []rune(rd.Reason); len(r) > 100 {
		rd.Reason = string(r[:100])
	}
	if p := strings.ToUpper(strings.TrimSpace(out.Parent)); len(p) > 1 {
		id, _ := strconv.ParseInt(p[1:], 10, 64)
		switch p[0] {
		case 'T':
			rd.ParentTask = id
		case 'I':
			rd.ParentItem = id
		}
	}
	if rd.Noise || rd.ParentTask != 0 || rd.ParentItem != 0 {
		rd.Subtasks = []string{} // subtasks are one level deep, and noise has none
	}
	return rd, nil
}

// cleanList trims, drops empty and repeated entries (ignoring case; the first spelling stays)
// and keeps at most n.
func cleanList(items []string, n int, lower bool) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range items {
		v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "#"))
		if lower {
			v = strings.ToLower(v)
		}
		if key := strings.ToLower(v); v != "" && !seen[key] && len(out) < n {
			seen[key] = true
			out = append(out, v)
		}
	}
	return out
}

// ---- which model a user's items go to ----

type llmSettings struct {
	Enabled bool
	URL     string
	Model   string
	HasKey  bool
	Notes   string // what the user tells the model about what matters to them
}

func (s *Server) loadLLMSettings(ctx context.Context, userID int64) (llmSettings, []byte, error) {
	st := llmSettings{Enabled: true}
	var key []byte
	err := s.db.QueryRow(ctx, `SELECT enabled, api_url, model, api_key, notes FROM llm_settings WHERE user_id = $1`, userID).
		Scan(&st.Enabled, &st.URL, &st.Model, &key, &st.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	st.HasKey = key != nil
	return st, key, err
}

// llmFor is the model to read a user's items with, or ok=false when they don't use one.
// The server's key only ever goes to the server's URL.
func (s *Server) llmFor(ctx context.Context, userID int64) (cfg LLMConfig, ok bool, err error) {
	st, key, err := s.loadLLMSettings(ctx, userID)
	if err != nil || !st.Enabled {
		return cfg, false, err
	}
	cfg = s.llm
	if st.URL != "" {
		cfg = LLMConfig{URL: st.URL}
		if key != nil {
			if cfg.Key, err = s.secrets.open(key); err != nil {
				return cfg, false, err
			}
		}
	}
	if st.Model != "" {
		cfg.Model = st.Model
	}
	return cfg, cfg.URL != "" && cfg.Model != "", nil
}

// ---- the background reader ----

// readInboxEvery reads pending items as they arrive (s.wakeReader) and at least every interval.
func (s *Server) readInboxEvery(interval time.Duration) {
	for {
		if err := s.readPending(context.Background()); err != nil {
			log.Printf("inbox reader: %v", err)
		}
		if err := s.expireNoise(context.Background()); err != nil {
			log.Printf("inbox reader: %v", err)
		}
		select {
		case <-s.wakeReader:
		case <-time.After(interval):
		}
	}
}

func (s *Server) wakeInboxReader() {
	select {
	case s.wakeReader <- struct{}{}:
	default: // already awake
	}
}

// readPending reads every pending item, oldest first, one at a time.
func (s *Server) readPending(ctx context.Context) error {
	for {
		var id, userID int64
		var raw string
		var sourceTags []string
		err := s.db.QueryRow(ctx, `SELECT i.id, i.user_id, i.raw, s.tags FROM inbox_items i JOIN task_sources s ON s.id = i.source_id
			WHERE i.parse_state = 'pending' ORDER BY i.id LIMIT 1`).Scan(&id, &userID, &raw, &sourceTags)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.readItem(ctx, id, userID, raw, sourceTags); err != nil {
			return err
		}
	}
}

// readItem asks the user's model about one item; its tags follow the source's. A model's
// failure is kept on the item (which keeps its guessed title); only database errors are returned.
func (s *Server) readItem(ctx context.Context, id, userID int64, raw string, sourceTags []string) error {
	cfg, ok, err := s.llmFor(ctx, userID)
	if err == nil && !ok {
		_, err = s.db.Exec(ctx, `UPDATE inbox_items SET parse_state = 'done' WHERE id = $1`, id)
		return err
	}
	var rd reading
	if err == nil {
		tc, dberr := s.triageContext(ctx, userID, id)
		if dberr != nil {
			return dberr
		}
		rd, err = cfg.readTask(ctx, raw, tc)
	}
	if err != nil {
		msg := err.Error()
		if r := []rune(msg); len(r) > 300 {
			msg = string(r[:300])
		}
		_, dberr := s.db.Exec(ctx, `UPDATE inbox_items SET parse_state = 'failed', parse_error = $2 WHERE id = $1`, id, msg)
		return dberr
	}
	if rd.Repeat { // only a repeat of something that is really there is filtered out
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT FROM tasks WHERE id = $2 AND user_id = $1 AND done_at IS NULL)
			OR EXISTS (SELECT FROM inbox_items WHERE id = $3 AND id <> $4 AND user_id = $1 AND status = 'new' AND verdict = 'task')`,
			userID, rd.ParentTask, rd.ParentItem, id).Scan(&exists); err != nil {
			return err
		}
		rd.Noise = exists
		if rd.Reason == "" {
			rd.Reason = "repeat"
		}
	}
	verdict := map[bool]string{false: "task", true: "noise"}[rd.Noise]
	// A parent must still be open and top-level, and an item with items of its own under it
	// stays top-level, so everything stays one level deep. Noise lets go of items under it.
	_, err = s.db.Exec(ctx, `
		WITH me AS (
			UPDATE inbox_items SET parse_state = 'done', parse_error = '', title = coalesce(nullif(@title, ''), title), details = @details, due_on = @due,
				tags = @tags, subtasks = @subtasks, verdict = @verdict, reason = @reason,
				parent_task_id = (SELECT id FROM tasks WHERE id = @ptask AND user_id = @user AND parent_id IS NULL AND done_at IS NULL AND @verdict = 'task'),
				parent_item_id = (SELECT p.id FROM inbox_items p WHERE p.id = @pitem AND p.id <> @id AND p.user_id = @user AND p.status = 'new'
					AND p.verdict = 'task' AND p.parent_item_id IS NULL AND @verdict = 'task'
					AND NOT EXISTS (SELECT FROM inbox_items c WHERE c.parent_item_id = @id AND c.status = 'new'))
			WHERE id = @id AND status = 'new' RETURNING id
		)
		UPDATE inbox_items SET parent_item_id = NULL WHERE parent_item_id = (SELECT id FROM me) AND @verdict = 'noise'`,
		pgx.NamedArgs{"id": id, "user": userID, "title": rd.Title, "details": rd.Details, "due": rd.DueOn,
			"tags": cleanList(append(sourceTags, rd.Tags...), len(sourceTags)+len(rd.Tags), false), "subtasks": rd.Subtasks,
			"verdict": verdict, "reason": rd.Reason, "ptask": rd.ParentTask, "pitem": rd.ParentItem})
	return err
}

// triageContext gathers what the model should know to sort item for the user.
func (s *Server) triageContext(ctx context.Context, userID, itemID int64) (triageContext, error) {
	y, m, d := time.Now().Date()
	tc := triageContext{Today: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
	err := s.db.QueryRow(ctx, `SELECT coalesce((SELECT notes FROM llm_settings WHERE user_id = $1), '')`, userID).Scan(&tc.Notes)
	lists := []struct {
		into  *[]string
		query string
	}{
		{&tc.Tags, `SELECT name FROM task_tags WHERE user_id = @user ORDER BY lower(name) LIMIT 50`},
		{&tc.Tasks, `SELECT 'T' || id || ': ' || left(title, 100) FROM tasks
			WHERE user_id = @user AND parent_id IS NULL AND done_at IS NULL ORDER BY id DESC LIMIT 80`},
		{&tc.Items, `SELECT 'I' || id || ': ' || left(title, 100) FROM inbox_items
			WHERE user_id = @user AND id <> @item AND status = 'new' AND parse_state = 'done' AND verdict = 'task' AND parent_item_id IS NULL
			ORDER BY id DESC LIMIT 80`},
		{&tc.Accepted, `SELECT left(title, 100) FROM inbox_items WHERE user_id = @user AND status = 'accepted' ORDER BY id DESC LIMIT 15`},
		// noise that expired unseen was not the user's choice
		{&tc.Discarded, `SELECT left(title, 100) FROM inbox_items WHERE user_id = @user AND status = 'discarded' AND verdict = 'task' ORDER BY id DESC LIMIT 15`},
	}
	for _, l := range lists {
		if err != nil {
			break
		}
		var rows pgx.Rows
		if rows, err = s.db.Query(ctx, l.query, pgx.NamedArgs{"user": userID, "item": itemID}); err == nil {
			*l.into, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
	}
	return tc, err
}

// expireNoise discards what was filtered out and not restored within 14 days.
func (s *Server) expireNoise(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `UPDATE inbox_items SET status = 'discarded', raw = ''
		WHERE status = 'new' AND verdict = 'noise' AND received_at < now() - interval '14 days'`)
	return err
}

// retryItem asks the model again about an item it failed on.
func (s *Server) retryItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `UPDATE inbox_items SET parse_state = 'pending', parse_error = ''
		WHERE id = $1 AND user_id = $2 AND status = 'new'`, pathID(r, "id"), currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.wakeInboxReader()
	redirect(w, r, "/tasks?view=inbox")
}

// saveLLMSettings stores the user's choice. An empty key keeps the stored one unless the URL
// changes or "clear_key" is set; the server's URL needs no key from the user.
func (s *Server) saveLLMSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := currentUser(r).ID
	apiURL := strings.TrimSpace(r.FormValue("api_url"))
	if apiURL != "" {
		if u, err := url.Parse(apiURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			s.renderSources(w, r, map[string]any{"AIError": "The API address must start with http:// or https://."})
			return
		}
	}
	old, oldKey, err := s.loadLLMSettings(ctx, userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	key := oldKey
	if apiURL != old.URL || r.FormValue("clear_key") != "" || apiURL == "" {
		key = nil // a key never follows the user to another address
	}
	if k := strings.TrimSpace(r.FormValue("api_key")); k != "" && apiURL != "" {
		if s.secrets == nil {
			s.renderSources(w, r, map[string]any{"AIError": "API keys can't be saved: the server has no SECRET_KEY set."})
			return
		}
		key = s.secrets.seal(k)
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	if r := []rune(notes); len(r) > 2000 {
		notes = string(r[:2000])
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO llm_settings (user_id, enabled, api_url, model, api_key, notes) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET enabled = $2, api_url = $3, model = $4, api_key = $5, notes = $6`,
		userID, r.FormValue("enabled") != "", apiURL, strings.TrimSpace(r.FormValue("model")), key, notes); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}
