package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type folder struct {
	ID       int64
	ParentID *int64
	Name     string
	Role     string
	Manage   bool // may rename or delete it (needs edit rights on the parent)
	Depth    int
	// HasChildren: visible subfolders follow it in the tree
	HasChildren bool
}

// folderPath returns the visible folders from the top of the tree down to id.
func folderPath(folders []folder, id int64) []folder {
	byID := map[int64]folder{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	var path []folder
	for f, ok := byID[id]; ok; f, ok = byID[orZero(f.ParentID)] {
		path = append([]folder{f}, path...)
	}
	return path
}

func (f folder) CanEdit() bool { return canEdit(f.Role) }

// folderTree returns the folders the user can see in depth-first order, each with its
// depth among the visible folders.
func (s *Server) folderTree(ctx context.Context, ws workspace, userID int64) ([]folder, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, parent_id, name, coalesce(effective_role($2, workspace_id, id), '')
		FROM folders WHERE workspace_id = $1 ORDER BY lower(name)`, ws.ID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := map[int64][]folder{}
	for rows.Next() {
		var f folder
		if err := rows.Scan(&f.ID, &f.ParentID, &f.Name, &f.Role); err != nil {
			return nil, err
		}
		children[orZero(f.ParentID)] = append(children[orZero(f.ParentID)], f)
	}
	var out []folder
	var walk func(parent int64, depth int, parentRole string)
	walk = func(parent int64, depth int, parentRole string) {
		for _, f := range children[parent] {
			if f.Role == "" { // hidden, but a subfolder may be shared
				walk(f.ID, depth, "")
				continue
			}
			f.Depth = depth
			f.Manage = canEdit(parentRole)
			out = append(out, f)
			walk(f.ID, depth+1, f.Role)
		}
	}
	walk(0, 0, ws.Role)
	for i := range out {
		out[i].HasChildren = i+1 < len(out) && out[i+1].Depth > out[i].Depth
	}
	return out, rows.Err()
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.container(w, r, pathID(r, "id"), formID(r, "parent_id"))
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirect(w, r, fmt.Sprintf("/w/%d", ws.ID))
		return
	}
	var id int64
	if err := s.db.QueryRow(r.Context(), `INSERT INTO folders (workspace_id, parent_id, name) VALUES ($1, $2, $3) RETURNING id`,
		ws.ID, formID(r, "parent_id"), name).Scan(&id); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/w/%d?folder=%d", ws.ID, id))
}

func (s *Server) renameFolder(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, folderParentRole)
	if !ok {
		return
	}
	id := pathID(r, "id")
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if _, err := s.db.Exec(r.Context(), `UPDATE folders SET name = $1 WHERE id = $2`, name, id); err != nil {
			serverError(w, err)
			return
		}
	}
	redirect(w, r, fmt.Sprintf("/w/%d?folder=%d", ws.ID, id))
}

func (s *Server) deleteFolder(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, folderParentRole)
	if !ok {
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM folders WHERE id = $1`, pathID(r, "id")); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/w/%d", ws.ID))
}

// moveFolderPage lists where a folder can go: folders the user can edit, except the folder
// itself and its subfolders.
func (s *Server) moveFolderPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, folderParentRole)
	if !ok {
		return
	}
	user := currentUser(r)
	found, err := s.userWorkspaces(r.Context(), user.ID, ws.ID)
	if err != nil || len(found) == 0 {
		serverError(w, fmt.Errorf("workspace %d: %v", ws.ID, err))
		return
	}
	folders, err := s.folderTree(r.Context(), found[0], user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	id := pathID(r, "id")
	var current folder
	var targets []folder
	skipDepth := -1 // while >= 0, we are inside the moved folder's subtree
	for _, f := range folders {
		if skipDepth >= 0 && f.Depth > skipDepth {
			continue
		}
		skipDepth = -1
		if f.ID == id {
			current, skipDepth = f, f.Depth
			continue
		}
		if f.CanEdit() {
			targets = append(targets, f)
		}
	}
	s.render(w, "move.html", map[string]any{
		"User": user, "Folder": current, "ParentID": orZero(current.ParentID), "Targets": targets, "CanRoot": found[0].CanEdit(),
		"Back": fmt.Sprintf("/w/%d?folder=%d", ws.ID, id),
	})
}

func (s *Server) moveFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, ok := s.editable(w, r, folderParentRole)
	if !ok {
		return
	}
	id := pathID(r, "id")
	parentID := formID(r, "parent_id")
	target, ok := s.container(w, r, ws.ID, parentID)
	if !ok {
		return
	}
	if target.ID != ws.ID {
		http.Error(w, "folder is in another workspace", http.StatusBadRequest)
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	// Serialize moves within the workspace so two concurrent moves cannot form a cycle.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ws.ID); err != nil {
		serverError(w, err)
		return
	}
	const subtree = `WITH RECURSIVE sub AS (
		SELECT id FROM folders WHERE id = $1
		UNION ALL
		SELECT c.id FROM folders c JOIN sub ON c.parent_id = sub.id
	)`
	var cycle bool
	if err := tx.QueryRow(ctx, subtree+` SELECT EXISTS (SELECT 1 FROM sub WHERE id = $2)`, id, parentID).Scan(&cycle); err != nil {
		serverError(w, err)
		return
	}
	if cycle {
		http.Error(w, "a folder cannot be moved into itself", http.StatusBadRequest)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE folders SET parent_id = $1 WHERE id = $2`, parentID, id); err != nil {
		serverError(w, err)
		return
	}
	rows, err := tx.Query(ctx, subtree+` SELECT n.id FROM notes n JOIN sub ON n.folder_id = sub.id`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	notes, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	s.hub.KickNotes(notes) // shares inherited from the old or new parents changed
	redirect(w, r, fmt.Sprintf("/w/%d?folder=%d", ws.ID, id))
}
