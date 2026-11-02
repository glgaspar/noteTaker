package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const inviteTTL = 7 * 24 * time.Hour

// adminMsg is the feedback shown on the admin page. Link is shown when SMTP is not
// configured, so the admin can pass it on by hand.
type adminMsg struct {
	Error, Notice, Link string
}

// sendLink emails a link and returns the feedback for the admin page.
func (s *server) sendLink(to, subject, body, link, notice string) adminMsg {
	if err := s.sendMail(to, subject, fmt.Sprintf(body, link)); err != nil {
		log.Printf("send mail to %s: %v", to, err)
		return adminMsg{Error: "The email could not be sent. Share this link instead:", Link: link}
	}
	if !s.mailConfigured() {
		return adminMsg{Notice: "Email is not configured. Share this link with " + to + ":", Link: link}
	}
	return adminMsg{Notice: notice}
}

type userRow struct {
	ID        int64
	Email     string
	Name      string
	IsAdmin   bool
	Disabled  bool
	CreatedAt time.Time
}

type inviteRow struct {
	Email     string
	ExpiresAt time.Time
}

func (s *server) adminUsers(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, adminMsg{})
}

func (s *server) renderAdmin(w http.ResponseWriter, r *http.Request, msg adminMsg) {
	ctx := r.Context()
	var users []userRow
	rows, err := s.db.Query(ctx, `SELECT id, email, name, is_admin, disabled_at IS NOT NULL, created_at FROM users ORDER BY created_at`)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.IsAdmin, &u.Disabled, &u.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		users = append(users, u)
	}
	var invites []inviteRow
	rows, err = s.db.Query(ctx, `SELECT email, expires_at FROM invites WHERE expires_at > now() ORDER BY created_at`)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var i inviteRow
		if err := rows.Scan(&i.Email, &i.ExpiresAt); err != nil {
			serverError(w, err)
			return
		}
		invites = append(invites, i)
	}
	s.render(w, "admin.html", map[string]any{
		"User": currentUser(r), "Users": users, "Invites": invites, "Msg": msg, "Loc": userLoc(r),
	})
}

func (s *server) createInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := normalizeEmail(r.FormValue("email"))
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		s.renderAdmin(w, r, adminMsg{Error: "Enter a valid email address."})
		return
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		serverError(w, err)
		return
	}
	if exists {
		s.renderAdmin(w, r, adminMsg{Error: "A user with that email already exists."})
		return
	}
	token := rand.Text()
	_, err := s.db.Exec(ctx, `
		INSERT INTO invites (email, token_hash, invited_by, expires_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE SET token_hash = $2, invited_by = $3, created_at = now(), expires_at = $4`,
		email, hashToken(token), currentUser(r).ID, time.Now().Add(inviteTTL))
	if err != nil {
		serverError(w, err)
		return
	}
	s.renderAdmin(w, r, s.sendLink(email, "You're invited to NoteTaker",
		"You have been invited to NoteTaker. Create your account here:\n\n%s\n\nThis link expires in 7 days.\n",
		s.baseURL+"/invite/"+token, "Invite sent to "+email+"."))
}

func (s *server) invitePage(w http.ResponseWriter, r *http.Request) {
	var email string
	err := s.db.QueryRow(r.Context(), `SELECT email FROM invites WHERE token_hash = $1 AND expires_at > now()`,
		hashToken(r.PathValue("token"))).Scan(&email)
	if err != nil {
		s.render(w, "invite.html", map[string]any{"Invalid": true})
		return
	}
	s.render(w, "invite.html", map[string]any{"Email": email, "Token": r.PathValue("token")})
}

func (s *server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	name := r.FormValue("name")
	password := r.FormValue("password")

	tx, err := s.db.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM invites WHERE token_hash = $1 AND expires_at > now() FOR UPDATE`,
		hashToken(token)).Scan(&email); err != nil {
		s.render(w, "invite.html", map[string]any{"Invalid": true})
		return
	}
	if name == "" || len(password) < 8 {
		s.render(w, "invite.html", map[string]any{
			"Email": email, "Token": token, "Name": name,
			"Error": "Enter your name and a password with at least 8 characters.",
		})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		serverError(w, err)
		return
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
		email, name, hash).Scan(&id); err != nil {
		serverError(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invites WHERE email = $1`, email); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	if err := s.startSession(ctx, w, id); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/")
}

// setDisabled disables or re-enables a user. Disabling signs them out everywhere; their notes
// and memberships stay.
func (s *server) setDisabled(disable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := pathID(r, "id")
		if id == currentUser(r).ID {
			s.renderAdmin(w, r, adminMsg{Error: "You cannot disable yourself."})
			return
		}
		if _, err := s.db.Exec(ctx, `UPDATE users SET disabled_at = CASE WHEN $1 THEN now() END WHERE id = $2`, disable, id); err != nil {
			serverError(w, err)
			return
		}
		if disable {
			if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
				serverError(w, err)
				return
			}
			if _, err := s.db.Exec(ctx, `DELETE FROM password_resets WHERE user_id = $1`, id); err != nil {
				serverError(w, err)
				return
			}
			s.hub.kick(id)
		}
		redirect(w, r, "/admin/users")
	}
}
