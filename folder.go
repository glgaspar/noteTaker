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
	Depth    int
}

// folderTree returns the workspace folders in depth-first order, each with its depth.
func (s *server) folderTree(ctx context.Context, wsID int64) ([]folder, error) {
	rows, err := s.db.Query(ctx, `SELECT id, parent_id, name FROM folders WHERE workspace_id = $1 ORDER BY lower(name)`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := map[int64][]folder{}
	for rows.Next() {
		var f folder
		if err := rows.Scan(&f.ID, &f.ParentID, &f.Name); err != nil {
			return nil, err
		}
		var parent int64
		if f.ParentID != nil {
			parent = *f.ParentID
		}
		children[parent] = append(children[parent], f)
	}
	var out []folder
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		for _, f := range children[parent] {
			f.Depth = depth
			out = append(out, f)
			walk(f.ID, depth+1)
		}
	}
	walk(0, 0)
	return out, rows.Err()
}

func (s *server) createFolder(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, "workspaces")
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
	ws, ok := s.editable(w, r, "folders")
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
	ws, ok := s.editable(w, r, "folders")
	if !ok {
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM folders WHERE id = $1`, pathID(r, "id")); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/w/%d", ws.ID))
}
