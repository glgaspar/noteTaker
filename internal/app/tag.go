package app

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

// workspaceTags lists the tags on notes the user can see, with their note counts.
func (s *Server) workspaceTags(ctx context.Context, userID, wsID int64) ([]tag, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.name, count(*)::int FROM tags t
		JOIN note_tags nt ON nt.tag_id = t.id JOIN notes n ON n.id = nt.note_id
		WHERE t.workspace_id = $1 AND effective_role($2, n.workspace_id, n.folder_id) IS NOT NULL
		GROUP BY t.id ORDER BY lower(t.name)`, wsID, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[tag])
}

func (s *Server) noteTags(ctx context.Context, noteID int64, canEdit bool) (noteTags, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.name, 0 FROM tags t JOIN note_tags nt ON nt.tag_id = t.id
		WHERE nt.note_id = $1 ORDER BY lower(t.name)`, noteID)
	if err != nil {
		return noteTags{}, err
	}
	tags, err := pgx.CollectRows(rows, pgx.RowToStructByPos[tag])
	return noteTags{NoteID: noteID, CanEdit: canEdit, Tags: tags}, err
}

func (s *Server) renderNoteTags(w http.ResponseWriter, r *http.Request, noteID int64) {
	nt, err := s.noteTags(r.Context(), noteID, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	nt.Focus = true
	s.renderPartial(w, "workspace.html", "note_tags", nt)
}

// tagNote adds a tag to a note, creating the tag in the workspace if needed.
func (s *Server) tagNote(ctx context.Context, wsID, noteID int64, name string) error {
	_, err := s.db.Exec(ctx, `
		WITH t AS (
			INSERT INTO tags (workspace_id, name) VALUES ($1, $2)
			ON CONFLICT (workspace_id, name) DO UPDATE SET name = excluded.name
			RETURNING id
		)
		INSERT INTO note_tags (note_id, tag_id) SELECT $3, id FROM t ON CONFLICT DO NOTHING`,
		wsID, name, noteID)
	return err
}

func (s *Server) addTag(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, noteRole)
	if !ok {
		return
	}
	noteID := pathID(r, "id")
	if name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.FormValue("name")), "#")); name != "" {
		if err := s.tagNote(r.Context(), ws.ID, noteID, name); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.renderNoteTags(w, r, noteID)
}

func (s *Server) removeTag(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, noteRole); !ok {
		return
	}
	noteID := pathID(r, "id")
	if _, err := s.db.Exec(r.Context(), `DELETE FROM note_tags WHERE note_id = $1 AND tag_id = $2`,
		noteID, pathID(r, "tag")); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderNoteTags(w, r, noteID)
}
