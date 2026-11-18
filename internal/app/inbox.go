package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Sources feed the task inbox; the user accepts or discards what arrives. Webhooks post to
// /hooks/{token}, where the token is a secret shown once and stored hashed.

const (
	hookMaxBody = 64 << 10 // bytes per webhook call
	inboxMax    = 500      // unhandled items per user; more are refused until some are handled
)

type source struct {
	ID        int64
	Kind      string
	Name      string
	CreatedAt time.Time
	Tags      []string // every item from it starts with these
	Items     int      // unhandled
	// mailboxes only
	Host      string
	Port      int
	Security  string
	Username  string
	Folder    string
	CheckedAt *time.Time
	LastError string
	Auth      string // "password" or "microsoft"
	// APIs only
	URL        string
	ItemsPath  string
	IDField    string
	Every      int
	HasHeaders bool
}

type inboxItem struct {
	ID         int64
	Source     string
	Raw        string
	Title      string
	Details    string
	DueOn      *time.Time
	ReceivedAt time.Time
	Tags       []string
	Subtasks   []string
	ParseState string
	ParseError string
	Noise      bool
	Reason     string // why it was filtered out
	ParentItem *int64
	ParentTask string      // the title of the open task it will be added to
	Children   []inboxItem `db:"-"` // items that will become its subtasks
}

// inbox is what the inbox shows: items to decide on, with related items under them, and what
// the model filtered out. Items the model is still reading are only counted.
type inbox struct {
	Items    []inboxItem
	Filtered []inboxItem
	Pending  int
}

// find returns the item with id, wherever it shows, or nil.
func (in inbox) find(id int64) *inboxItem {
	for _, list := range [][]inboxItem{in.Items, in.Filtered} {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
			for j := range list[i].Children {
				if list[i].Children[j].ID == id {
					return &list[i].Children[j]
				}
			}
		}
	}
	return nil
}

// suggestion is a task read from what a source received.
type suggestion struct {
	ExternalID string // lets a sender resend an item without duplicating it
	Title      string
	Details    string
	DueOn      *time.Time
}

// suggest reads a task from a payload: a JSON object with fields such as title/subject,
// body/description and due (yyyy-mm-dd), or plain text whose first line is the title.
func suggest(raw string) suggestion {
	var obj map[string]any
	var sg suggestion
	if json.Unmarshal([]byte(raw), &obj) == nil {
		field := func(keys ...string) string {
			for _, k := range keys {
				switch v := obj[k].(type) {
				case string:
					if v = strings.TrimSpace(v); v != "" {
						return v
					}
				case float64:
					return fmt.Sprint(v)
				}
			}
			return ""
		}
		sg = suggestion{ExternalID: field("id", "external_id", "message_id"), Title: field("title", "subject", "name", "summary"),
			Details: field("body", "description", "details", "text", "message", "content")}
		if due := field("due", "due_on", "due_date"); len(due) >= 10 {
			sg.DueOn = parseDue(due[:10])
		}
	} else {
		sg.Details = strings.TrimSpace(raw)
	}
	if sg.Title == "" { // the first line of the text
		sg.Title, sg.Details, _ = strings.Cut(sg.Details, "\n")
		sg.Details = strings.TrimSpace(sg.Details)
	}
	sg.Title = strings.TrimSpace(sg.Title)
	return sg
}

// addToInbox stores what a source received for its user, with a first guess at the task,
// unless the inbox is full.
func (s *Server) addToInbox(ctx context.Context, userID, sourceID int64, raw string, sg suggestion) error {
	clean := func(s string) string { return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "") } // Postgres text refuses both
	raw, sg.Title, sg.Details, sg.ExternalID = clean(raw), clean(sg.Title), clean(sg.Details), clean(sg.ExternalID)
	if sg.Title = strings.TrimSpace(sg.Title); sg.Title == "" {
		sg.Title = "Untitled"
	}
	if r := []rune(sg.Title); len(r) > 200 {
		sg.Title = string(r[:200]) + "…"
	}
	var full bool
	// filtered-out noise doesn't count: it expires by itself
	if err := s.db.QueryRow(ctx, `SELECT count(*) >= $2 FROM inbox_items WHERE user_id = $1 AND status = 'new' AND verdict = 'task'`,
		userID, inboxMax).Scan(&full); err != nil {
		return err
	}
	if full {
		return errInboxFull
	}
	// the guess shows at once; the inbox reader (llm.go) then asks the user's model
	if _, err := s.db.Exec(ctx, `INSERT INTO inbox_items (user_id, source_id, external_id, raw, title, details, due_on, parse_state, tags)
		VALUES ($1, $2, nullif($3, ''), $4, $5, $6, $7, 'pending', (SELECT tags FROM task_sources WHERE id = $2))
		ON CONFLICT (source_id, external_id) DO NOTHING`,
		userID, sourceID, sg.ExternalID, raw, sg.Title, sg.Details, sg.DueOn); err != nil {
		return err
	}
	s.wakeInboxReader()
	return nil
}

var errInboxFull = errors.New("the inbox is full")

func (s *Server) receiveHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var sourceID, userID int64
	err := s.db.QueryRow(ctx, `SELECT id, user_id FROM task_sources WHERE kind = 'webhook' AND token_hash = $1`,
		hashToken(r.PathValue("token"))).Scan(&sourceID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	key := fmt.Sprint(sourceID)
	if blocked, _ := s.hookRate.blocked(key); blocked {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	s.hookRate.fail(key) // counts every call
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hookMaxBody))
	if err != nil {
		http.Error(w, "the body is limited to 64 KB", http.StatusRequestEntityTooLarge)
		return
	}
	if strings.TrimSpace(string(body)) == "" || !utf8.Valid(body) {
		http.Error(w, "send a JSON object or UTF-8 text", http.StatusBadRequest)
		return
	}
	switch err := s.addToInbox(ctx, userID, sourceID, string(body), suggest(string(body))); {
	case errors.Is(err, errInboxFull):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case err != nil:
		s.serverError(w, r, err)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Server) inbox(ctx context.Context, userID int64) (inbox, error) {
	rows, err := s.db.Query(ctx, `
		SELECT i.id, s.name, i.raw, i.title, i.details, i.due_on, i.received_at, i.tags, i.subtasks, i.parse_state, i.parse_error,
			i.verdict = 'noise', i.reason, i.parent_item_id,
			coalesce((SELECT t.title FROM tasks t WHERE t.id = i.parent_task_id AND t.done_at IS NULL AND t.parent_id IS NULL), '')
		FROM inbox_items i JOIN task_sources s ON s.id = i.source_id
		WHERE i.user_id = $1 AND i.status = 'new' ORDER BY i.received_at DESC, i.id DESC`, userID)
	if err != nil {
		return inbox{}, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[inboxItem])
	if err != nil {
		return inbox{}, err
	}
	var in inbox
	top := map[int64]int{} // item id -> index in in.Items
	for _, it := range all {
		switch {
		case it.ParseState == "pending":
			in.Pending++
		case it.Noise:
			in.Filtered = append(in.Filtered, it)
		case it.ParentItem == nil:
			top[it.ID] = len(in.Items)
			in.Items = append(in.Items, it)
		}
	}
	for _, it := range all {
		if it.ParseState != "pending" && !it.Noise && it.ParentItem != nil {
			if i, ok := top[*it.ParentItem]; ok {
				in.Items[i].Children = append(in.Items[i].Children, it)
			} else {
				in.Items = append(in.Items, it)
			}
		}
	}
	return in, nil
}

// accept turns a top-level inbox item into a task, with the title the user may have edited,
// the subtasks and tags the model suggested, and the items under it as subtasks. An item that
// belongs to an open task becomes one of its subtasks instead, and so do the items under it.
func accept(ctx context.Context, tx pgx.Tx, userID, itemID int64, title string) error {
	var taskID int64
	var parentID *int64
	var tags, subtasks []string
	err := tx.QueryRow(ctx, `
		WITH i AS (
			UPDATE inbox_items i SET status = 'accepted', raw = ''
			WHERE id = @id AND user_id = @user AND status = 'new' AND verdict = 'task' AND parent_item_id IS NULL
			RETURNING title, details, due_on, tags, subtasks, (SELECT t.id FROM tasks t
				WHERE t.id = i.parent_task_id AND t.user_id = @user AND t.parent_id IS NULL AND t.done_at IS NULL) AS parent
		)
		INSERT INTO tasks (user_id, parent_id, title, details, due_on)
		SELECT @user, parent, coalesce(nullif(@title, ''), title), details, due_on FROM i
		RETURNING id, parent_id, (SELECT tags FROM i), (SELECT subtasks FROM i)`,
		pgx.NamedArgs{"id": itemID, "user": userID, "title": strings.TrimSpace(title)}).Scan(&taskID, &parentID, &tags, &subtasks)
	if err != nil {
		return err
	}
	under := taskID
	if parentID != nil {
		under, tags = *parentID, nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO tasks (user_id, parent_id, title) SELECT $1, $2, unnest($3::text[])`, userID, under, subtasks); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		WITH c AS (
			UPDATE inbox_items SET status = 'accepted', raw = '' WHERE parent_item_id = $3 AND user_id = $1 AND status = 'new'
			RETURNING id, title, details, due_on
		)
		INSERT INTO tasks (user_id, parent_id, title, details, due_on) SELECT $1, $2, title, details, due_on FROM c ORDER BY id`,
		userID, under, itemID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		WITH g AS (
			INSERT INTO task_tags (user_id, name) SELECT $1, unnest($3::text[])
			ON CONFLICT (user_id, name) DO UPDATE SET name = excluded.name RETURNING id
		)
		INSERT INTO task_tag_links (user_id, task_id, tag_id) SELECT $1, $2, id FROM g`, userID, taskID, tags)
	return err
}

// inTx runs f in a transaction and then goes back to the inbox; no rows means not found.
func (s *Server) inTx(w http.ResponseWriter, r *http.Request, f func(pgx.Tx) error) {
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	if err = f(tx); err == nil {
		err = tx.Commit(ctx)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.fail(w, r, http.StatusNotFound, "")
	case err != nil:
		s.serverError(w, r, err)
	default:
		redirect(w, r, "/tasks?view=inbox")
	}
}

func (s *Server) acceptItem(w http.ResponseWriter, r *http.Request) {
	s.inTx(w, r, func(tx pgx.Tx) error {
		return accept(r.Context(), tx, currentUser(r).ID, pathID(r, "id"), r.FormValue("title"))
	})
}

// acceptAll accepts every item the model has read and kept, with what's under them.
func (s *Server) acceptAll(w http.ResponseWriter, r *http.Request) {
	ctx, userID := r.Context(), currentUser(r).ID
	s.inTx(w, r, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM inbox_items WHERE user_id = $1 AND status = 'new' AND verdict = 'task'
			AND parse_state = 'done' AND parent_item_id IS NULL ORDER BY id`, userID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		for _, id := range ids {
			if err == nil {
				err = accept(ctx, tx, userID, id, "")
			}
		}
		return err
	})
}

// discardItem drops an item; items under it go back to the top of the inbox.
func (s *Server) discardItem(w http.ResponseWriter, r *http.Request) {
	s.inboxUpdate(w, r, `
		WITH d AS (UPDATE inbox_items SET status = 'discarded', raw = '' WHERE id = $1 AND user_id = $2 AND status = 'new' RETURNING id)
		UPDATE inbox_items SET parent_item_id = NULL WHERE parent_item_id = (SELECT id FROM d)`)
}

// restoreItem brings back an item the model filtered out.
func (s *Server) restoreItem(w http.ResponseWriter, r *http.Request) {
	s.inboxUpdate(w, r, `UPDATE inbox_items SET verdict = 'task', reason = '' WHERE id = $1 AND user_id = $2 AND status = 'new'`)
}

// detachItem makes an item its own task instead of a subtask of another.
func (s *Server) detachItem(w http.ResponseWriter, r *http.Request) {
	s.inboxUpdate(w, r, `UPDATE inbox_items SET parent_item_id = NULL, parent_task_id = NULL WHERE id = $1 AND user_id = $2 AND status = 'new'`)
}

func (s *Server) inboxUpdate(w http.ResponseWriter, r *http.Request, query string) {
	if _, err := s.db.Exec(r.Context(), query, pathID(r, "id"), currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	// from the item's own pane, keep it open
	if id := orZero(formID(r, "item")); id != 0 {
		redirect(w, r, fmt.Sprintf("/tasks?view=inbox&item=%d", id))
		return
	}
	redirect(w, r, "/tasks?view=inbox")
}

// ---- managing sources ----

// renderSources shows the sources page; extra adds messages, a new webhook URL or a form to redo.
func (s *Server) renderSources(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	rows, err := s.db.Query(r.Context(), `
		SELECT s.id, s.kind, s.name, s.created_at, s.tags,
			(SELECT count(*)::int FROM inbox_items i WHERE i.source_id = s.id AND i.status = 'new'),
			coalesce(a.host, ''), coalesce(a.port, 0), coalesce(a.security, ''), coalesce(a.username, ''),
			coalesce(a.folder, ''), coalesce(a.checked_at, p.checked_at), coalesce(a.last_error, p.last_error, ''), coalesce(a.auth, ''),
			coalesce(p.url, ''), coalesce(p.items_path, ''), coalesce(p.id_field, ''), coalesce(p.every_minutes, 0),
			p.headers IS NOT NULL
		FROM task_sources s LEFT JOIN imap_accounts a ON a.source_id = s.id LEFT JOIN api_sources p ON p.source_id = s.id
		WHERE s.user_id = $1 ORDER BY s.created_at, s.id`, currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sources, err := pgx.CollectRows(rows, pgx.RowToStructByPos[source])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ai, _, err := s.loadLLMSettings(r.Context(), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var defaultHost string
	if u, err := url.Parse(s.llm.URL); err == nil && s.llm.Model != "" {
		defaultHost = u.Host
	}
	data := map[string]any{"User": currentUser(r), "Loc": userLoc(r), "Sources": sources, "NewID": int64(0), "MailFormID": int64(-1),
		"APIFormID": int64(-1), "Microsoft": s.microsoft.ClientID != "", "AI": ai, "DefaultModel": s.llm.Model, "DefaultHost": defaultHost, "CanSaveKeys": s.secrets != nil}
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, "sources.html", data)
}

func (s *Server) sourcesPage(w http.ResponseWriter, r *http.Request) {
	s.renderSources(w, r, nil)
}

// hookURL makes a new secret for a webhook, returning its URL and the hash to store.
func (s *Server) hookURL() (string, []byte) {
	token := rand.Text()
	return s.baseURL + "/hooks/" + token, hashToken(token)
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "Webhook"
	}
	url, hash := s.hookURL()
	var id int64
	if err := s.db.QueryRow(r.Context(), `INSERT INTO task_sources (user_id, kind, name, token_hash, tags)
		VALUES ($1, 'webhook', $2, $3, $4) RETURNING id`, currentUser(r).ID, name, hash, formTags(r)).Scan(&id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderSources(w, r, map[string]any{"NewURL": url, "NewID": id})
}

// formTags reads the "tags" field: names separated by commas, "#" optional, at most 5.
func formTags(r *http.Request) []string {
	return cleanList(strings.Split(r.FormValue("tags"), ","), 5, false)
}

// setSourceTags changes the tags a source's new items start with.
func (s *Server) setSourceTags(w http.ResponseWriter, r *http.Request) {
	tag, err := s.db.Exec(r.Context(), `UPDATE task_sources SET tags = $1 WHERE id = $2 AND user_id = $3`,
		formTags(r), pathID(r, "id"), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	redirect(w, r, "/tasks/sources")
}

// newSourceToken replaces a webhook's URL, so the old one stops working.
func (s *Server) newSourceToken(w http.ResponseWriter, r *http.Request) {
	url, hash := s.hookURL()
	tag, err := s.db.Exec(r.Context(), `UPDATE task_sources SET token_hash = $1 WHERE id = $2 AND user_id = $3 AND kind = 'webhook'`,
		hash, pathID(r, "id"), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	s.renderSources(w, r, map[string]any{"NewURL": url, "NewID": pathID(r, "id")})
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `DELETE FROM task_sources WHERE id = $1 AND user_id = $2`,
		pathID(r, "id"), currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}
