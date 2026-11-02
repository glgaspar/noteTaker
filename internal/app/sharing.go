package app

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	Known     []member // people to suggest in the email field
}

// knownUsers lists people the user already works with: members of their workspaces and people
// with folder shares in them. Share forms suggest only these, so the user list stays private.
func (s *Server) knownUsers(r *http.Request) []member {
	rows, err := s.db.Query(r.Context(), `
		WITH mine AS (SELECT workspace_id FROM workspace_members WHERE user_id = $1)
		SELECT u.id, u.name, u.email, '' FROM users u
		WHERE u.id <> $1 AND u.disabled_at IS NULL AND u.id IN (
			SELECT user_id FROM workspace_members WHERE workspace_id IN (SELECT workspace_id FROM mine)
			UNION
			SELECT fm.user_id FROM folder_members fm JOIN folders f ON f.id = fm.folder_id
			WHERE f.workspace_id IN (SELECT workspace_id FROM mine))
		ORDER BY lower(u.name)`, currentUser(r).ID)
	if err != nil {
		log.Printf("known users: %v", err)
		return nil
	}
	known, _ := pgx.CollectRows(rows, pgx.RowToStructByPos[member])
	return known
}

func (s *Server) members(r *http.Request, query string, id int64) ([]member, error) {
	rows, err := s.db.Query(r.Context(), query, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[member])
}

// shareTarget resolves the email and role posted to a share form. msg explains to the user
// why the input is not acceptable.
func (s *Server) shareTarget(r *http.Request) (userID int64, role, msg string, err error) {
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

func (s *Server) renderWorkspaceMembers(w http.ResponseWriter, r *http.Request, msg string) {
	user := currentUser(r)
	found, err := s.userWorkspaces(r.Context(), user.ID, pathID(r, "id"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(found) == 0 || found[0].Role == "" {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	ws := found[0]
	members, err := s.members(r, workspaceMembersQuery, ws.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, "share.html", sharePage{
		User: user, Title: "Workspace members", Subject: ws.Name, Members: members, Error: msg,
		Action: fmt.Sprintf("/w/%d/members", ws.ID), Back: fmt.Sprintf("/w/%d", ws.ID), CanManage: ws.Role == "owner",
		Known: s.knownUsers(r),
	})
}

func (s *Server) workspaceMembers(w http.ResponseWriter, r *http.Request) {
	s.renderWorkspaceMembers(w, r, "")
}

func (s *Server) addWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.editable(w, r, workspaceRole)
	if !ok {
		return
	}
	if ws.Role != "owner" {
		s.fail(w, r, http.StatusForbidden, "only the owner can manage members")
		return
	}
	userID, role, msg, err := s.shareTarget(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if msg == "" && userID == currentUser(r).ID {
		msg = "You are the owner of this workspace."
	}
	if msg != "" {
		s.renderWorkspaceMembers(w, r, msg)
		return
	}
	var added bool // xmax = 0 only for a freshly inserted row
	err = s.db.QueryRow(r.Context(), `
		INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = $3 WHERE workspace_members.role <> 'owner'
		RETURNING xmax = 0`, ws.ID, userID, role).Scan(&added)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.serverError(w, r, err)
		return
	}
	s.hub.Kick(userID)
	if added {
		var name string
		s.db.QueryRow(r.Context(), `SELECT name FROM workspaces WHERE id = $1`, ws.ID).Scan(&name)
		s.notifyShare(userID, currentUser(r).Name, fmt.Sprintf("the workspace %q", name), role, fmt.Sprintf("/w/%d", ws.ID))
	}
	redirect(w, r, fmt.Sprintf("/w/%d/members", ws.ID))
}

// removeWorkspaceMember lets the owner remove anyone but themselves, and any member leave.
func (s *Server) removeWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	ws, err := s.roleOf(r.Context(), user.ID, workspaceRole, pathID(r, "id"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	target := pathID(r, "user")
	leaving := target == user.ID
	if ws.Role == "" || (!leaving && ws.Role != "owner") || (leaving && ws.Role == "owner") {
		s.fail(w, r, http.StatusForbidden, "forbidden")
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2 AND role <> 'owner'`,
		ws.ID, target); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.hub.Kick(target)
	if leaving {
		redirect(w, r, "/")
		return
	}
	redirect(w, r, fmt.Sprintf("/w/%d/members", ws.ID))
}

const folderMembersQuery = `
	SELECT u.id, u.name, u.email, m.role FROM folder_members m JOIN users u ON u.id = m.user_id
	WHERE m.folder_id = $1 ORDER BY lower(u.name)`

func (s *Server) renderFolderShares(w http.ResponseWriter, r *http.Request, msg string) {
	ws, ok := s.editable(w, r, folderWorkspaceRole)
	if !ok {
		return
	}
	id := pathID(r, "id")
	var name string
	if err := s.db.QueryRow(r.Context(), `SELECT name FROM folders WHERE id = $1`, id).Scan(&name); err != nil {
		s.serverError(w, r, err)
		return
	}
	members, err := s.members(r, folderMembersQuery, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, "share.html", sharePage{
		User: currentUser(r), Title: "Share folder", Subject: name, Members: members, Error: msg, CanManage: true,
		Action: fmt.Sprintf("/folders/%d/share", id), Back: fmt.Sprintf("/w/%d?folder=%d", ws.ID, id),
		Known: s.knownUsers(r),
	})
}

func (s *Server) folderShares(w http.ResponseWriter, r *http.Request) {
	s.renderFolderShares(w, r, "")
}

func (s *Server) addFolderShare(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, folderWorkspaceRole); !ok {
		return
	}
	userID, role, msg, err := s.shareTarget(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if msg != "" {
		s.renderFolderShares(w, r, msg)
		return
	}
	var added bool // xmax = 0 only for a freshly inserted row
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO folder_members (folder_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (folder_id, user_id) DO UPDATE SET role = $3
		RETURNING xmax = 0`, pathID(r, "id"), userID, role).Scan(&added); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.hub.Kick(userID)
	if added {
		var name string
		var wsID int64
		s.db.QueryRow(r.Context(), `SELECT name, workspace_id FROM folders WHERE id = $1`, pathID(r, "id")).Scan(&name, &wsID)
		s.notifyShare(userID, currentUser(r).Name, fmt.Sprintf("the folder %q", name), role,
			fmt.Sprintf("/w/%d?folder=%d", wsID, pathID(r, "id")))
	}
	redirect(w, r, fmt.Sprintf("/folders/%d/share", pathID(r, "id")))
}

func (s *Server) removeFolderShare(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, folderWorkspaceRole); !ok {
		return
	}
	target := pathID(r, "user")
	if _, err := s.db.Exec(r.Context(), `DELETE FROM folder_members WHERE folder_id = $1 AND user_id = $2`,
		pathID(r, "id"), target); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.hub.Kick(target)
	redirect(w, r, fmt.Sprintf("/folders/%d/share", pathID(r, "id")))
}

// notifyShare emails a user that something was shared with them, without delaying the request.
func (s *Server) notifyShare(userID int64, sharer, what, role, path string) {
	go func() {
		var email string
		if err := s.db.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
			return
		}
		body := fmt.Sprintf("%s shared %s with you on NoteTaker. You can %s it.\n\nOpen it: %s%s\n",
			sharer, what, map[string]string{"editor": "edit", "viewer": "view"}[role], s.baseURL, path)
		if err := s.sendMail(email, sharer+" shared "+what+" with you", body); err != nil {
			log.Printf("share notification to %s: %v", email, err)
		}
	}()
}
