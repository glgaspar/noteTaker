package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type noteItem struct {
	ID        int64
	Title     string
	Snippet   string
	UpdatedAt time.Time
	Tags      []string
}

type note struct {
	ID         int64
	FolderID   int64 // 0 when the note is at the workspace root
	Title      string
	Content    string // TipTap JSON
	ImportHTML string // HTML of an imported note not yet opened in an editor
	Author     string
	UpdatedAt  time.Time
	Role       string
	Seeded     bool // the note has a live (Yjs) document
}

func (n note) CanEdit() bool { return canEdit(n.Role) }

func (f folder) Indent() string { return strings.Repeat("\u00a0\u00a0\u00a0", f.Depth) }

func orZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// workspaceURL builds the workspace page URL for a folder (0 = all notes) and an optional note.
func workspaceURL(wsID, folderID, noteID int64) string {
	u := fmt.Sprintf("/w/%d", wsID)
	sep := "?"
	if folderID != 0 {
		u += fmt.Sprintf("?folder=%d", folderID)
		sep = "&"
	}
	if noteID != 0 {
		u += fmt.Sprintf("%snote=%d", sep, noteID)
	}
	return u
}

// noteList lists the notes of a workspace the user can see, optionally only those directly
// inside a folder or with a tag.
func (s *Server) noteList(ctx context.Context, userID, wsID, folderID, tagID int64) ([]noteItem, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, title, left(content_text, 120), updated_at,
			ARRAY(SELECT t.name FROM note_tags nt JOIN tags t ON t.id = nt.tag_id WHERE nt.note_id = n.id ORDER BY lower(t.name))
		FROM notes n
		WHERE workspace_id = $1 AND ($2 = 0 OR folder_id = $2)
		AND ($3 = 0 OR EXISTS (SELECT 1 FROM note_tags nt WHERE nt.note_id = n.id AND nt.tag_id = $3))
		AND effective_role($4, workspace_id, folder_id) IS NOT NULL
		ORDER BY updated_at DESC`, wsID, folderID, tagID, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[noteItem])
}

// loadNote returns a note of the workspace, or nil if there is none the user can see with that id.
func (s *Server) loadNote(ctx context.Context, userID, wsID, noteID int64) (*note, error) {
	n := note{ID: noteID}
	err := s.db.QueryRow(ctx, `
		SELECT coalesce(n.folder_id, 0), n.title, coalesce(n.content::text, ''), coalesce(n.import_html, ''),
			coalesce(u.name, ''), n.updated_at,
			coalesce(effective_role($3, n.workspace_id, n.folder_id), ''),
			EXISTS (SELECT 1 FROM note_updates WHERE note_id = n.id)
		FROM notes n LEFT JOIN users u ON u.id = n.author_id
		WHERE n.id = $1 AND n.workspace_id = $2`, noteID, wsID, userID).
		Scan(&n.FolderID, &n.Title, &n.Content, &n.ImportHTML, &n.Author, &n.UpdatedAt, &n.Role, &n.Seeded)
	if err == pgx.ErrNoRows || n.Role == "" {
		return nil, nil
	}
	return &n, err
}

func (s *Server) createNote(w http.ResponseWriter, r *http.Request) {
	folderID := formID(r, "folder_id")
	ws, ok := s.container(w, r, pathID(r, "id"), folderID)
	if !ok {
		return
	}
	var id int64
	if err := s.db.QueryRow(r.Context(), `INSERT INTO notes (workspace_id, folder_id, author_id) VALUES ($1, $2, $3) RETURNING id`,
		ws.ID, folderID, currentUser(r).ID).Scan(&id); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, workspaceURL(ws.ID, orZero(folderID), id))
}

func (s *Server) saveNote(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, noteRole); !ok {
		return
	}
	var body struct {
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
		Text    string          `json:"text"`
		Seed    bool            `json:"seed"` // first save after seeding: not a user edit
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&body); err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid note")
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		UPDATE notes SET title = $1, content = $2, content_text = $3, import_html = NULL,
			updated_at = CASE WHEN $5 THEN updated_at ELSE now() END
		WHERE id = $4`,
		strings.TrimSpace(body.Title), body.Content, body.Text, pathID(r, "id"), body.Seed); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) moveNote(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, noteRole)
	if !ok {
		return
	}
	id := pathID(r, "id")
	folderID := formID(r, "folder_id")
	target, ok := s.container(w, r, ws.ID, folderID)
	if !ok {
		return
	}
	if target.ID != ws.ID {
		s.fail(w, r, http.StatusBadRequest, "folder is in another workspace")
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE notes SET folder_id = $1 WHERE id = $2`, folderID, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.hub.KickNotes([]int64{id}) // who may access it can change with the folder
	redirect(w, r, workspaceURL(ws.ID, orZero(folderID), id))
}

func (s *Server) deleteNote(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, noteRole)
	if !ok {
		return
	}
	var folderID *int64
	if err := s.db.QueryRow(r.Context(), `DELETE FROM notes WHERE id = $1 RETURNING folder_id`, pathID(r, "id")).Scan(&folderID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, workspaceURL(ws.ID, orZero(folderID), 0))
}
