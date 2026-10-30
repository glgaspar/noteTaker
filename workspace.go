package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type workspace struct {
	ID   int64
	Name string
	Role string
}

func (ws workspace) CanEdit() bool { return ws.Role == "owner" || ws.Role == "editor" }

func pathID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

func formID(r *http.Request, name string) *int64 {
	id, err := strconv.ParseInt(r.FormValue(name), 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

// workspaceFor returns the workspace with the user's role, or ok=false if they are not a member.
func (s *server) workspaceFor(ctx context.Context, userID, wsID int64) (workspace, bool, error) {
	ws := workspace{ID: wsID}
	err := s.db.QueryRow(ctx, `
		SELECT w.name, m.role FROM workspaces w
		JOIN workspace_members m ON m.workspace_id = w.id AND m.user_id = $2
		WHERE w.id = $1`, wsID, userID).Scan(&ws.Name, &ws.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ws, false, nil
	}
	return ws, err == nil, err
}

// access resolves the workspace of a row in a workspace-owned table (folders, notes, ...).
func (s *server) access(ctx context.Context, userID int64, table string, id int64) (workspace, bool, error) {
	var wsID int64
	err := s.db.QueryRow(ctx, fmt.Sprintf(`SELECT workspace_id FROM %s WHERE id = $1`, table), id).Scan(&wsID)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace{}, false, nil
	}
	if err != nil {
		return workspace{}, false, err
	}
	return s.workspaceFor(ctx, userID, wsID)
}

// editable loads the workspace from the {id} path value and checks the user may edit it.
// On failure it writes the response and returns ok=false.
func (s *server) editable(w http.ResponseWriter, r *http.Request, table string) (workspace, bool) {
	var ws workspace
	var ok bool
	var err error
	if table == "workspaces" {
		ws, ok, err = s.workspaceFor(r.Context(), currentUser(r).ID, pathID(r, "id"))
	} else {
		ws, ok, err = s.access(r.Context(), currentUser(r).ID, table, pathID(r, "id"))
	}
	switch {
	case err != nil:
		serverError(w, err)
	case !ok:
		http.NotFound(w, r)
	case !ws.CanEdit():
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		return ws, true
	}
	return ws, false
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	var id int64
	err := s.db.QueryRow(r.Context(), `SELECT workspace_id FROM workspace_members WHERE user_id = $1 ORDER BY workspace_id LIMIT 1`,
		currentUser(r).ID).Scan(&id)
	if err == nil {
		redirect(w, r, fmt.Sprintf("/w/%d", id))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, err)
		return
	}
	s.render(w, "home.html", map[string]any{"User": currentUser(r)})
}

func (s *server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirect(w, r, "/")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO workspaces (name, created_by) VALUES ($1, $2) RETURNING id`,
		name, currentUser(r).ID).Scan(&id); err != nil {
		serverError(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		id, currentUser(r).ID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/w/%d", id))
}

func (s *server) renameWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "workspaces")
	if !ok {
		return
	}
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if _, err := s.db.Exec(r.Context(), `UPDATE workspaces SET name = $1 WHERE id = $2`, name, ws.ID); err != nil {
			serverError(w, err)
			return
		}
	}
	redirect(w, r, fmt.Sprintf("/w/%d", ws.ID))
}

func (s *server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "workspaces")
	if !ok {
		return
	}
	if ws.Role != "owner" {
		http.Error(w, "only the owner can delete a workspace", http.StatusForbidden)
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM workspaces WHERE id = $1`, ws.ID); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/")
}

func (s *server) workspacePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	ws, ok, err := s.workspaceFor(ctx, user.ID, pathID(r, "id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT w.id, w.name, m.role FROM workspaces w
		JOIN workspace_members m ON m.workspace_id = w.id
		WHERE m.user_id = $1 ORDER BY w.name`, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowToStructByPos[workspace])
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "workspace.html", map[string]any{
		"User": user, "Workspace": ws, "Workspaces": workspaces,
	})
}
