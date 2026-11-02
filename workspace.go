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
	Role string // "" when the user only sees it through shared folders
}

func canEdit(role string) bool { return role == "owner" || role == "editor" }

func (ws workspace) CanEdit() bool { return canEdit(ws.Role) }

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

// Queries returning (workspace_id, role) of the user ($1) for an object ($2).
// Roles come from effective_role (see migrations/007_sharing.sql); "" means no access.
const (
	workspaceRole       = `SELECT id, coalesce(effective_role($1, id, NULL), '') FROM workspaces WHERE id = $2`
	folderRole          = `SELECT workspace_id, coalesce(effective_role($1, workspace_id, id), '') FROM folders WHERE id = $2`
	folderParentRole    = `SELECT workspace_id, coalesce(effective_role($1, workspace_id, parent_id), '') FROM folders WHERE id = $2`
	folderWorkspaceRole = `SELECT workspace_id, coalesce(effective_role($1, workspace_id, NULL), '') FROM folders WHERE id = $2`
	noteRole            = `SELECT workspace_id, coalesce(effective_role($1, workspace_id, folder_id), '') FROM notes WHERE id = $2`
)

func (s *server) roleOf(ctx context.Context, userID int64, query string, id int64) (workspace, error) {
	var ws workspace
	err := s.db.QueryRow(ctx, query, userID, id).Scan(&ws.ID, &ws.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace{}, nil
	}
	return ws, err
}

// check verifies the user may edit the object; on failure it writes the response and returns false.
func (s *server) check(w http.ResponseWriter, r *http.Request, query string, id int64) (workspace, bool) {
	ws, err := s.roleOf(r.Context(), currentUser(r).ID, query, id)
	switch {
	case err != nil:
		serverError(w, err)
	case ws.Role == "":
		http.NotFound(w, r)
	case !ws.CanEdit():
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		return ws, true
	}
	return ws, false
}

// editable is check for the object in the {id} path value.
func (s *server) editable(w http.ResponseWriter, r *http.Request, query string) (workspace, bool) {
	return s.check(w, r, query, pathID(r, "id"))
}

// container checks the user may add to a folder, or to the workspace root when folderID is nil.
func (s *server) container(w http.ResponseWriter, r *http.Request, wsID int64, folderID *int64) (workspace, bool) {
	if folderID != nil {
		return s.check(w, r, folderRole, *folderID)
	}
	return s.check(w, r, workspaceRole, wsID)
}

// userWorkspaces lists the workspaces a user is a member of or has shared folders in.
// With wsID != 0 it returns at most that workspace.
func (s *server) userWorkspaces(ctx context.Context, userID, wsID int64) ([]workspace, error) {
	rows, err := s.db.Query(ctx, `
		SELECT w.id, w.name, coalesce(m.role, '') FROM workspaces w
		LEFT JOIN workspace_members m ON m.workspace_id = w.id AND m.user_id = $1
		WHERE ($2 = 0 OR w.id = $2) AND (m.user_id IS NOT NULL OR EXISTS (
			SELECT 1 FROM folder_members fm JOIN folders f ON f.id = fm.folder_id
			WHERE f.workspace_id = w.id AND fm.user_id = $1))
		ORDER BY lower(w.name)`, userID, wsID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[workspace])
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.userWorkspaces(r.Context(), currentUser(r).ID, 0)
	if err != nil {
		serverError(w, err)
		return
	}
	if len(workspaces) > 0 {
		redirect(w, r, fmt.Sprintf("/w/%d", workspaces[0].ID))
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
	ws, ok := s.editable(w, r, workspaceRole)
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
	ws, ok := s.editable(w, r, workspaceRole)
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

// sidebar loads the workspace in the {id} path value and the data the sidebar partial needs.
// On failure it writes the response and returns ok=false.
func (s *server) sidebar(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	ctx := r.Context()
	user := currentUser(r)
	found, err := s.userWorkspaces(ctx, user.ID, pathID(r, "id"))
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	if len(found) == 0 {
		http.NotFound(w, r)
		return nil, false
	}
	ws := found[0]
	workspaces, err := s.userWorkspaces(ctx, user.ID, 0)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	folders, err := s.folderTree(ctx, ws, user.ID)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	tags, err := s.workspaceTags(ctx, user.ID, ws.ID)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	return map[string]any{
		"User": user, "Workspace": ws, "Workspaces": workspaces, "Folders": folders, "Tags": tags,
		"FolderID": orZero(formID(r, "folder")), "TagID": orZero(formID(r, "tag")), "Loc": userLoc(r),
	}, true
}

func (s *server) workspacePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	data, ok := s.sidebar(w, r)
	if !ok {
		return
	}
	ws := data["Workspace"].(workspace)
	folderID, tagID := data["FolderID"].(int64), data["TagID"].(int64)
	notes, err := s.noteList(ctx, user.ID, ws.ID, folderID, tagID)
	if err != nil {
		serverError(w, err)
		return
	}
	n, err := s.loadNote(ctx, user.ID, ws.ID, orZero(formID(r, "note")))
	if err != nil {
		serverError(w, err)
		return
	}
	var nt noteTags
	if n != nil {
		if nt, err = s.noteTags(ctx, n.ID, n.CanEdit()); err != nil {
			serverError(w, err)
			return
		}
	}
	// New notes go into the selected folder, or the workspace root.
	canCreate := ws.CanEdit()
	for _, f := range data["Folders"].([]folder) {
		if f.ID == folderID {
			canCreate = f.CanEdit()
		}
	}
	data["Notes"], data["Note"], data["NoteTags"], data["CanCreate"] = notes, n, nt, canCreate
	s.render(w, "workspace.html", data)
}
