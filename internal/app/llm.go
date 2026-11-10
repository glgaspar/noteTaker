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

const taskPrompt = `You turn something a person received (an email, a webhook call, an API record) into one item for their to-do list.
Answer with JSON only, no markdown, exactly in this shape:
{"title": "", "details": "", "due": "", "tags": [], "subtasks": []}
- title: the action to take, short and imperative (at most 80 characters), in the language of the message.
- details: the context needed to do it (who, what, amounts, links, references), brief plain text. Empty if the title says it all.
- due: a deadline as YYYY-MM-DD, only if the message states or clearly implies one; otherwise "". Today is %s.
- tags: 0 to 3 short lowercase tags. Prefer these existing ones when they fit: %s.
- subtasks: separate steps only when the message clearly lists them, at most 5; otherwise [].
If nothing needs doing, still write the most useful title (for example "Read: <subject>").`

// maxLLMInput limits how much of an item is sent to the model.
const maxLLMInput = 12000

// readTask asks the model for a task from raw, given the user's tag names.
func (c LLMConfig) readTask(ctx context.Context, raw string, tags []string, today time.Time) (suggestion, []string, []string, error) {
	if r := []rune(raw); len(r) > maxLLMInput {
		raw = string(r[:maxLLMInput]) + "\n[…cut]"
	}
	existing := "none yet"
	if len(tags) > 0 {
		existing = strings.Join(tags, ", ")
	}
	answer, err := c.chat(ctx, fmt.Sprintf(taskPrompt, today.Format("Monday, 2006-01-02"), existing), raw)
	if err != nil {
		return suggestion{}, nil, nil, err
	}
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return suggestion{}, nil, nil, fmt.Errorf("the model answered without JSON: %.200s", answer)
	}
	var out struct {
		Title, Details, Due string
		Tags, Subtasks      []string
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &out); err != nil {
		return suggestion{}, nil, nil, fmt.Errorf("the model's JSON is invalid: %v", err)
	}
	sg := suggestion{Title: strings.TrimSpace(out.Title), Details: strings.TrimSpace(out.Details), DueOn: parseDue(out.Due)}
	if sg.Title == "" {
		return suggestion{}, nil, nil, errors.New("the model gave no title")
	}
	if r := []rune(sg.Title); len(r) > 200 {
		sg.Title = string(r[:200]) + "…"
	}
	return sg, cleanList(out.Tags, 3, true), cleanList(out.Subtasks, 5, false), nil
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
}

func (s *Server) loadLLMSettings(ctx context.Context, userID int64) (llmSettings, []byte, error) {
	st := llmSettings{Enabled: true}
	var key []byte
	err := s.db.QueryRow(ctx, `SELECT enabled, api_url, model, api_key FROM llm_settings WHERE user_id = $1`, userID).
		Scan(&st.Enabled, &st.URL, &st.Model, &key)
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
	var sg suggestion
	var tags, subtasks []string
	if err == nil {
		var names []string
		rows, qerr := s.db.Query(ctx, `SELECT name FROM task_tags WHERE user_id = $1 ORDER BY lower(name) LIMIT 50`, userID)
		if qerr != nil {
			return qerr
		}
		if names, qerr = pgx.CollectRows(rows, pgx.RowTo[string]); qerr != nil {
			return qerr
		}
		y, m, d := time.Now().Date()
		sg, tags, subtasks, err = cfg.readTask(ctx, raw, names, time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
	}
	if err != nil {
		msg := err.Error()
		if r := []rune(msg); len(r) > 300 {
			msg = string(r[:300])
		}
		_, dberr := s.db.Exec(ctx, `UPDATE inbox_items SET parse_state = 'failed', parse_error = $2 WHERE id = $1`, id, msg)
		return dberr
	}
	_, err = s.db.Exec(ctx, `UPDATE inbox_items SET parse_state = 'done', parse_error = '',
		title = $2, details = $3, due_on = $4, tags = $5, subtasks = $6 WHERE id = $1 AND status = 'new'`,
		id, sg.Title, sg.Details, sg.DueOn, cleanList(append(sourceTags, tags...), len(sourceTags)+len(tags), false), subtasks)
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
	if _, err := s.db.Exec(ctx, `INSERT INTO llm_settings (user_id, enabled, api_url, model, api_key) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET enabled = $2, api_url = $3, model = $4, api_key = $5`,
		userID, r.FormValue("enabled") != "", apiURL, strings.TrimSpace(r.FormValue("model")), key); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}
