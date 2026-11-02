package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type member struct {
	UserID int64
	Name   string
	Email  string
	Role   string
}

// sharePage is the data for share.html, used for both workspace members and folder shares.
type sharePage struct {
	User      *User
	Title     string
	Subject   string // what is being shared
	Action    string // POST here to add or change a member; Action/{user}/delete removes one
	Back      string
	Members   []member
	CanManage bool
	Error     string
}

func (s *server) members(r *http.Request, query string, id int64) ([]member, error) {
	rows, err := s.db.Query(r.Context(), query, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[member])
}

// shareTarget resolves the email and role posted to a share form. msg explains to the user
// why the input is not acceptable.
func (s *server) shareTarget(r *http.Request) (userID int64, role, msg string, err error) {
	role = r.FormValue("role")
	if role != "editor" && role != "viewer" {
		return 0, "", "Pick a role.", nil
	}
	err = s.db.QueryRow(r.Context(), `SELECT id FROM users WHERE email = $1 AND disabled_at IS NULL`, normalizeEmail(r.FormValue("email"))).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", "No user with that email. Ask an admin to invite them first.", nil
	}
	return userID, role, "", err
}

const workspaceMembersQuery = `
	SELECT u.id, u.name, u.email, m.role FROM workspace_members m JOIN users u ON u.id = m.user_id
	WHERE m.workspace_id = $1 ORDER BY m.role = 'owner' DESC, lower(u.name)`

func (s *server) renderWorkspaceMembers(w http.ResponseWriter, r *http.Request, msg string) {
	user := currentUser(r)
	found, err := s.userWorkspaces(r.Context(), user.ID, pathID(r, "id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if len(found) == 0 || found[0].Role == "" {
		http.NotFound(w, r)
		return
	}
	ws := found[0]
	members, err := s.members(r, workspaceMembersQuery, ws.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "share.html", sharePage{
		User: user, Title: "Workspace members", Subject: ws.Name, Members: members, Error: msg,
		Action: fmt.Sprintf("/w/%d/members", ws.ID), Back: fmt.Sprintf("/w/%d", ws.ID), CanManage: ws.Role == "owner",
	})
}

func (s *server) workspaceMembers(w http.ResponseWriter, r *http.Request) {
	s.renderWorkspaceMembers(w, r, "")
}

func (s *server) addWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, workspaceRole)
	if !ok {
		return
	}
	if ws.Role != "owner" {
		http.Error(w, "only the owner can manage members", http.StatusForbidden)
		return
	}
	userID, role, msg, err := s.shareTarget(r)
	if err != nil {
		serverError(w, err)
		return
	}
	if msg == "" && userID == currentUser(r).ID {
		msg = "You are the owner of this workspace."
	}
	if msg != "" {
		s.renderWorkspaceMembers(w, r, msg)
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = $3 WHERE workspace_members.role <> 'owner'`,
		ws.ID, userID, role); err != nil {
		serverError(w, err)
		return
	}
	s.hub.kick(userID)
	redirect(w, r, fmt.Sprintf("/w/%d/members", ws.ID))
}

// removeWorkspaceMember lets the owner remove anyone but themselves, and any member leave.
func (s *server) removeWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	ws, err := s.roleOf(r.Context(), user.ID, workspaceRole, pathID(r, "id"))
	if err != nil {
		serverError(w, err)
		return
	}
	target := pathID(r, "user")
	leaving := target == user.ID
	if ws.Role == "" || (!leaving && ws.Role != "owner") || (leaving && ws.Role == "owner") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2 AND role <> 'owner'`,
		ws.ID, target); err != nil {
		serverError(w, err)
		return
	}
	s.hub.kick(target)
	if leaving {
		redirect(w, r, "/")
		return
	}
	redirect(w, r, fmt.Sprintf("/w/%d/members", ws.ID))
}

const folderMembersQuery = `
	SELECT u.id, u.name, u.email, m.role FROM folder_members m JOIN users u ON u.id = m.user_id
	WHERE m.folder_id = $1 ORDER BY lower(u.name)`

func (s *server) renderFolderShares(w http.ResponseWriter, r *http.Request, msg string) {
	ws, ok := s.editable(w, r, folderWorkspaceRole)
	if !ok {
		return
	}
	id := pathID(r, "id")
	var name string
	if err := s.db.QueryRow(r.Context(), `SELECT name FROM folders WHERE id = $1`, id).Scan(&name); err != nil {
		serverError(w, err)
		return
	}
	members, err := s.members(r, folderMembersQuery, id)
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "share.html", sharePage{
		User: currentUser(r), Title: "Share folder", Subject: name, Members: members, Error: msg, CanManage: true,
		Action: fmt.Sprintf("/folders/%d/share", id), Back: fmt.Sprintf("/w/%d?folder=%d", ws.ID, id),
	})
}

func (s *server) folderShares(w http.ResponseWriter, r *http.Request) {
	s.renderFolderShares(w, r, "")
}

func (s *server) addFolderShare(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, folderWorkspaceRole); !ok {
		return
	}
	userID, role, msg, err := s.shareTarget(r)
	if err != nil {
		serverError(w, err)
		return
	}
	if msg != "" {
		s.renderFolderShares(w, r, msg)
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO folder_members (folder_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (folder_id, user_id) DO UPDATE SET role = $3`, pathID(r, "id"), userID, role); err != nil {
		serverError(w, err)
		return
	}
	s.hub.kick(userID)
	redirect(w, r, fmt.Sprintf("/folders/%d/share", pathID(r, "id")))
}

func (s *server) removeFolderShare(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, folderWorkspaceRole); !ok {
		return
	}
	target := pathID(r, "user")
	if _, err := s.db.Exec(r.Context(), `DELETE FROM folder_members WHERE folder_id = $1 AND user_id = $2`,
		pathID(r, "id"), target); err != nil {
		serverError(w, err)
		return
	}
	s.hub.kick(target)
	redirect(w, r, fmt.Sprintf("/folders/%d/share", pathID(r, "id")))
}
