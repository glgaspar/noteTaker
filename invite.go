package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/smtp"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const inviteTTL = 7 * 24 * time.Hour

type smtpConfig struct {
	host, port, user, pass, from string
}

// sendInvite emails the invite link, or logs it when SMTP is not configured.
func (s *server) sendInvite(to, link string) error {
	if s.smtp.host == "" {
		log.Printf("SMTP not configured; invite link for %s: %s", to, link)
		return nil
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: You're invited to NoteTaker\r\n\r\n"+
		"You have been invited to NoteTaker. Create your account here:\r\n\r\n%s\r\n\r\n"+
		"This link expires in 7 days.\r\n", s.smtp.from, to, link)
	var auth smtp.Auth
	if s.smtp.user != "" {
		auth = smtp.PlainAuth("", s.smtp.user, s.smtp.pass, s.smtp.host)
	}
	return smtp.SendMail(s.smtp.host+":"+s.smtp.port, auth, s.smtp.from, []string{to}, []byte(msg))
}

type userRow struct {
	Email     string
	Name      string
	IsAdmin   bool
	CreatedAt time.Time
}

type inviteRow struct {
	Email     string
	ExpiresAt time.Time
}

func (s *server) adminUsers(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, "", "")
}

func (s *server) renderAdmin(w http.ResponseWriter, r *http.Request, errMsg, notice string) {
	ctx := r.Context()
	var users []userRow
	rows, err := s.db.Query(ctx, `SELECT email, name, is_admin, created_at FROM users ORDER BY created_at`)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.Email, &u.Name, &u.IsAdmin, &u.CreatedAt); err != nil {
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
		"User": currentUser(r), "Users": users, "Invites": invites, "Error": errMsg, "Notice": notice,
	})
}

func (s *server) createInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := normalizeEmail(r.FormValue("email"))
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		s.renderAdmin(w, r, "Enter a valid email address.", "")
		return
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		serverError(w, err)
		return
	}
	if exists {
		s.renderAdmin(w, r, "A user with that email already exists.", "")
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
	if err := s.sendInvite(email, s.baseURL+"/invite/"+token); err != nil {
		log.Printf("send invite to %s: %v", email, err)
		s.renderAdmin(w, r, "Invite saved, but the email could not be sent.", "")
		return
	}
	s.renderAdmin(w, r, "", "Invite sent to "+email+".")
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
