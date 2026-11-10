package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
}

type inboxItem struct {
	ID         int64
	Source     string
	Raw        string
	Title      string
	Details    string
	DueOn      *time.Time
	ReceivedAt time.Time
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
	if sg.Title = strings.TrimSpace(sg.Title); sg.Title == "" {
		sg.Title = "Untitled"
	}
	if r := []rune(sg.Title); len(r) > 200 {
		sg.Title = string(r[:200]) + "…"
	}
	return sg
}

// addToInbox stores what a source received for its user, unless the inbox is full.
func (s *Server) addToInbox(ctx context.Context, userID, sourceID int64, raw string) error {
	raw = strings.ReplaceAll(strings.ToValidUTF8(raw, "�"), "\x00", "") // Postgres text refuses both
	sg := suggest(raw)
	var full bool
	if err := s.db.QueryRow(ctx, `SELECT count(*) >= $2 FROM inbox_items WHERE user_id = $1 AND status = 'new'`,
		userID, inboxMax).Scan(&full); err != nil {
		return err
	}
	if full {
		return errInboxFull
	}
	_, err := s.db.Exec(ctx, `INSERT INTO inbox_items (user_id, source_id, external_id, raw, title, details, due_on)
		VALUES ($1, $2, nullif($3, ''), $4, $5, $6, $7) ON CONFLICT (source_id, external_id) DO NOTHING`,
		userID, sourceID, sg.ExternalID, raw, sg.Title, sg.Details, sg.DueOn)
	return err
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
	switch err := s.addToInbox(ctx, userID, sourceID, string(body)); {
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
		SELECT i.id, s.name, i.raw, i.title, i.details, i.due_on, i.received_at
		FROM inbox_items i JOIN task_sources s ON s.id = i.source_id
		WHERE i.user_id = $1 AND i.status = 'new' ORDER BY i.received_at DESC, i.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[inboxItem])
}

// acceptItem turns an inbox item into a task, with the title the user may have edited.
func (s *Server) acceptItem(w http.ResponseWriter, r *http.Request) {
	var taskID int64
	err := s.db.QueryRow(r.Context(), `
		WITH i AS (
			UPDATE inbox_items SET status = 'accepted', raw = '' WHERE id = @id AND user_id = @user AND status = 'new'
			RETURNING title, details, due_on
		)
		INSERT INTO tasks (user_id, title, details, due_on)
		SELECT @user, coalesce(nullif(@title, ''), title), details, due_on FROM i RETURNING id`,
		pgx.NamedArgs{"id": pathID(r, "id"), "user": currentUser(r).ID, "title": strings.TrimSpace(r.FormValue("title"))}).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, http.StatusNotFound, "")
		return
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

func (s *Server) renderSources(w http.ResponseWriter, r *http.Request, newURL string, newID int64) {
	rows, err := s.db.Query(r.Context(), `
		SELECT s.id, s.kind, s.name, s.created_at,
			(SELECT count(*)::int FROM inbox_items i WHERE i.source_id = s.id AND i.status = 'new')
		FROM task_sources s WHERE s.user_id = $1 ORDER BY s.created_at, s.id`, currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sources, err := pgx.CollectRows(rows, pgx.RowToStructByPos[source])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, "sources.html", map[string]any{"User": currentUser(r), "Loc": userLoc(r), "Sources": sources,
		"NewURL": newURL, "NewID": newID})
}

func (s *Server) sourcesPage(w http.ResponseWriter, r *http.Request) { s.renderSources(w, r, "", 0) }

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
	s.renderSources(w, r, url, id)
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
	s.renderSources(w, r, url, pathID(r, "id"))
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `DELETE FROM task_sources WHERE id = $1 AND user_id = $2`,
		pathID(r, "id"), currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}
