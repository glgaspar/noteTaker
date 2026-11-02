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
		serverError(w, err)
		return
	}
	if ws.Role == "" {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil) // rejects cross-origin requests
	if err != nil {
		return
	}
	s.hub.Serve(r.Context(), conn, s.db, noteID, currentUser(r).ID, ws.CanEdit())
}
