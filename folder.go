package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type folder struct {
	ID       int64
	ParentID *int64
	Name     string
	Role     string
	Manage   bool // may rename or delete it (needs edit rights on the parent)
	Depth    int
}

func (f folder) CanEdit() bool { return canEdit(f.Role) }

// folderTree returns the folders the user can see in depth-first order, each with its
// depth among the visible folders.
func (s *server) folderTree(ctx context.Context, ws workspace, userID int64) ([]folder, error) {
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
	return out, rows.Err()
}

func (s *server) createFolder(w http.ResponseWriter, r *http.Request) {
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

func (s *server) renameFolder(w http.ResponseWriter, r *http.Request) {
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

func (s *server) deleteFolder(w http.ResponseWriter, r *http.Request) {
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
