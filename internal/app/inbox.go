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
	Items     int // unhandled
	// mailboxes only
	Host      string
	Port      int
	Security  string
	Username  string
	Folder    string
	CheckedAt *time.Time
	LastError string
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
	if err := s.db.QueryRow(ctx, `SELECT count(*) >= $2 FROM inbox_items WHERE user_id = $1 AND status = 'new'`,
		userID, inboxMax).Scan(&full); err != nil {
		return err
	}
	if full {
		return errInboxFull
	}
	// the guess shows at once; the inbox reader (llm.go) then asks the user's model
	if _, err := s.db.Exec(ctx, `INSERT INTO inbox_items (user_id, source_id, external_id, raw, title, details, due_on, parse_state)
		VALUES ($1, $2, nullif($3, ''), $4, $5, $6, $7, 'pending') ON CONFLICT (source_id, external_id) DO NOTHING`,
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

func (s *Server) inboxItems(ctx context.Context, userID int64) ([]inboxItem, error) {
	rows, err := s.db.Query(ctx, `
		SELECT i.id, s.name, i.raw, i.title, i.details, i.due_on, i.received_at, i.tags, i.subtasks, i.parse_state, i.parse_error
		FROM inbox_items i JOIN task_sources s ON s.id = i.source_id
		WHERE i.user_id = $1 AND i.status = 'new' ORDER BY i.received_at DESC, i.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[inboxItem])
}

// acceptItem turns an inbox item into a task, with the title the user may have edited, and
// the subtasks and tags the model suggested.
func (s *Server) acceptItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := currentUser(r).ID
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	var taskID int64
	var tags, subtasks []string
	err = tx.QueryRow(ctx, `
		WITH i AS (
			UPDATE inbox_items SET status = 'accepted', raw = '' WHERE id = @id AND user_id = @user AND status = 'new'
			RETURNING title, details, due_on, tags, subtasks
		)
		INSERT INTO tasks (user_id, title, details, due_on)
		SELECT @user, coalesce(nullif(@title, ''), title), details, due_on FROM i
		RETURNING id, (SELECT tags FROM i), (SELECT subtasks FROM i)`,
		pgx.NamedArgs{"id": pathID(r, "id"), "user": userID, "title": strings.TrimSpace(r.FormValue("title"))}).Scan(&taskID, &tags, &subtasks)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO tasks (user_id, parent_id, title) SELECT $1, $2, unnest($3::text[])`, userID, taskID, subtasks)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `
			WITH g AS (
				INSERT INTO task_tags (user_id, name) SELECT $1, unnest($3::text[])
				ON CONFLICT (user_id, name) DO UPDATE SET name = excluded.name RETURNING id
			)
			INSERT INTO task_tag_links (user_id, task_id, tag_id) SELECT $1, $2, id FROM g`, userID, taskID, tags)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks?view=inbox")
}

func (s *Server) discardItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `UPDATE inbox_items SET status = 'discarded', raw = ''
		WHERE id = $1 AND user_id = $2 AND status = 'new'`, pathID(r, "id"), currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks?view=inbox")
}

// ---- managing sources ----

// renderSources shows the sources page; extra adds messages, a new webhook URL or a form to redo.
func (s *Server) renderSources(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	rows, err := s.db.Query(r.Context(), `
		SELECT s.id, s.kind, s.name, s.created_at,
			(SELECT count(*)::int FROM inbox_items i WHERE i.source_id = s.id AND i.status = 'new'),
			coalesce(a.host, ''), coalesce(a.port, 0), coalesce(a.security, ''), coalesce(a.username, ''),
			coalesce(a.folder, ''), coalesce(a.checked_at, p.checked_at), coalesce(a.last_error, p.last_error, ''),
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
		"APIFormID": int64(-1), "AI": ai, "DefaultModel": s.llm.Model, "DefaultHost": defaultHost, "CanSaveKeys": s.secrets != nil}
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
	if err := s.db.QueryRow(r.Context(), `INSERT INTO task_sources (user_id, kind, name, token_hash)
		VALUES ($1, 'webhook', $2, $3) RETURNING id`, currentUser(r).ID, name, hash).Scan(&id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderSources(w, r, map[string]any{"NewURL": url, "NewID": id})
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
