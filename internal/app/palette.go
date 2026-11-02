package app

import (
	"net/http"

	"github.com/jackc/pgx/v5"
)

type paletteFolder struct {
	ID   int64
	Name string
}

// palette renders the command palette results: matching notes (recent ones without a
// query) and folders of the workspace.
func (s *Server) palette(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	found, err := s.userWorkspaces(ctx, user.ID, pathID(r, "id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if len(found) == 0 {
		http.NotFound(w, r)
		return
	}
	ws := found[0]
	notes, err := s.search(ctx, user.ID, ws.ID, 0, r)
	if err != nil {
		serverError(w, err)
		return
	}
	var folders []paletteFolder
	if q := r.FormValue("q"); q != "" {
		rows, err := s.db.Query(ctx, `
			SELECT id, name FROM folders
			WHERE workspace_id = $1 AND name ILIKE '%' || $2 || '%' AND effective_role($3, workspace_id, id) IS NOT NULL
			ORDER BY lower(name) LIMIT 5`, ws.ID, likeEscaper.Replace(q), user.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		if folders, err = pgx.CollectRows(rows, pgx.RowToStructByPos[paletteFolder]); err != nil {
			serverError(w, err)
			return
		}
	}
	s.renderPartial(w, "workspace.html", "palette_results", map[string]any{
		"Workspace": ws, "Notes": notes[:min(len(notes), 8)], "Folders": folders, "Query": r.FormValue("q"), "Loc": userLoc(r),
	})
}
