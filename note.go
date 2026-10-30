package main

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
}

type note struct {
	ID        int64
	FolderID  int64 // 0 when the note is at the workspace root
	Title     string
	Content   string // TipTap JSON
	Author    string
	UpdatedAt time.Time
}

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

// noteList lists notes of a workspace, optionally only those directly inside a folder.
func (s *server) noteList(ctx context.Context, wsID, folderID int64) ([]noteItem, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, title, left(content_text, 120), updated_at FROM notes
		WHERE workspace_id = $1 AND ($2 = 0 OR folder_id = $2)
		ORDER BY updated_at DESC`, wsID, folderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[noteItem])
}

// loadNote returns a note of the workspace, or nil if there is none with that id.
func (s *server) loadNote(ctx context.Context, wsID, noteID int64) (*note, error) {
	n := note{ID: noteID}
	err := s.db.QueryRow(ctx, `
		SELECT coalesce(n.folder_id, 0), n.title, coalesce(n.content::text, ''), coalesce(u.name, ''), n.updated_at
		FROM notes n LEFT JOIN users u ON u.id = n.author_id
		WHERE n.id = $1 AND n.workspace_id = $2`, noteID, wsID).
		Scan(&n.FolderID, &n.Title, &n.Content, &n.Author, &n.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &n, err
}

func (s *server) createNote(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "workspaces")
	if !ok {
		return
	}
	folderID := formID(r, "folder_id")
	var id int64
	if err := s.db.QueryRow(r.Context(), `INSERT INTO notes (workspace_id, folder_id, author_id) VALUES ($1, $2, $3) RETURNING id`,
		ws.ID, folderID, currentUser(r).ID).Scan(&id); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, workspaceURL(ws.ID, orZero(folderID), id))
}

func (s *server) saveNote(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, "notes"); !ok {
		return
	}
	var body struct {
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
		Text    string          `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid note", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE notes SET title = $1, content = $2, content_text = $3, updated_at = now() WHERE id = $4`,
		strings.TrimSpace(body.Title), body.Content, body.Text, pathID(r, "id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) moveNote(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "notes")
	if !ok {
		return
	}
	id := pathID(r, "id")
	folderID := formID(r, "folder_id")
	if _, err := s.db.Exec(r.Context(), `UPDATE notes SET folder_id = $1 WHERE id = $2`, folderID, id); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, workspaceURL(ws.ID, orZero(folderID), id))
}

func (s *server) deleteNote(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "notes")
	if !ok {
		return
	}
	var folderID *int64
	if err := s.db.QueryRow(r.Context(), `DELETE FROM notes WHERE id = $1 RETURNING folder_id`, pathID(r, "id")).Scan(&folderID); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, workspaceURL(ws.ID, orZero(folderID), 0))
}
