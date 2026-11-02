package app

import (
	"net/http"

	"github.com/coder/websocket"
)

// noteSocket connects an editor to the live document of a note (see package collab).
func (s *Server) noteSocket(w http.ResponseWriter, r *http.Request) {
	noteID := pathID(r, "id")
	ws, err := s.roleOf(r.Context(), currentUser(r).ID, noteRole, noteID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ws.Role == "" {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	conn, err := websocket.Accept(w, r, nil) // rejects cross-origin requests
	if err != nil {
		return
	}
	user := currentUser(r)
	s.hub.Serve(r.Context(), conn, s.db, noteID, user.ID, user.Name, string(avatarColor(user.ID)), ws.CanEdit())
}
