package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type tag struct {
	ID    int64
	Name  string
	Count int
}

// noteTags is the data for the "note_tags" partial.
type noteTags struct {
	NoteID  int64
	CanEdit bool
	Focus   bool
	Tags    []tag
}

// workspaceTags lists the tags in use in a workspace with their note counts.
func (s *server) workspaceTags(ctx context.Context, wsID int64) ([]tag, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.name, count(*)::int FROM tags t JOIN note_tags nt ON nt.tag_id = t.id
		WHERE t.workspace_id = $1 GROUP BY t.id ORDER BY lower(t.name)`, wsID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[tag])
}

func (s *server) noteTags(ctx context.Context, noteID int64, canEdit bool) (noteTags, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.name, 0 FROM tags t JOIN note_tags nt ON nt.tag_id = t.id
		WHERE nt.note_id = $1 ORDER BY lower(t.name)`, noteID)
	if err != nil {
		return noteTags{}, err
	}
	tags, err := pgx.CollectRows(rows, pgx.RowToStructByPos[tag])
	return noteTags{NoteID: noteID, CanEdit: canEdit, Tags: tags}, err
}

func (s *server) renderNoteTags(w http.ResponseWriter, r *http.Request, noteID int64) {
	nt, err := s.noteTags(r.Context(), noteID, true)
	if err != nil {
		serverError(w, err)
		return
	}
	nt.Focus = true
	s.renderPartial(w, "workspace.html", "note_tags", nt)
}

func (s *server) addTag(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "notes")
	if !ok {
		return
	}
	noteID := pathID(r, "id")
	if name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.FormValue("name")), "#")); name != "" {
		_, err := s.db.Exec(r.Context(), `
			WITH t AS (
				INSERT INTO tags (workspace_id, name) VALUES ($1, $2)
				ON CONFLICT (workspace_id, name) DO UPDATE SET name = excluded.name
				RETURNING id
			)
			INSERT INTO note_tags (note_id, tag_id) SELECT $3, id FROM t ON CONFLICT DO NOTHING`,
			ws.ID, name, noteID)
		if err != nil {
			serverError(w, err)
			return
		}
	}
	s.renderNoteTags(w, r, noteID)
}

func (s *server) removeTag(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, "notes"); !ok {
		return
	}
	noteID := pathID(r, "id")
	if _, err := s.db.Exec(r.Context(), `DELETE FROM note_tags WHERE note_id = $1 AND tag_id = $2`,
		noteID, pathID(r, "tag")); err != nil {
		serverError(w, err)
		return
	}
	s.renderNoteTags(w, r, noteID)
}
