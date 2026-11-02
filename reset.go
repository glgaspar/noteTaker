package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// newResetLink replaces any pending reset for the user with a fresh one.
func (s *server) newResetLink(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	token := rand.Text()
	if _, err := s.db.Exec(ctx, `DELETE FROM password_resets WHERE user_id = $1 OR expires_at < now()`, userID); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO password_resets (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), userID, time.Now().Add(ttl)); err != nil {
		return "", err
	}
	return s.baseURL + "/reset/" + token, nil
}

const resetBody = "A password reset was requested for your NoteTaker account. Choose a new password here:\n\n%s\n\n" +
	"If you did not ask for this, ignore this email.\n"

// adminReset sends a user a reset link; without SMTP the admin sees the link.
func (s *server) adminReset(w http.ResponseWriter, r *http.Request) {
	var email string
	if err := s.db.QueryRow(r.Context(), `SELECT email FROM users WHERE id = $1`, pathID(r, "id")).Scan(&email); err != nil {
		http.NotFound(w, r)
		return
	}
	link, err := s.newResetLink(r.Context(), pathID(r, "id"), 24*time.Hour)
	if err != nil {
		serverError(w, err)
		return
	}
	s.renderAdmin(w, r, s.sendLink(email, "Reset your NoteTaker password", resetBody, link, "Reset link sent to "+email+"."))
}

func (s *server) forgotPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "forgot.html", nil)
}

// forgot emails a reset link if the account exists. The answer is the same either way, and
// without SMTP the link is only logged, so this cannot be used to take over accounts.
func (s *server) forgot(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if blocked, _ := s.resetIP.blocked(ip); blocked {
		s.render(w, "forgot.html", map[string]any{"Error": "Too many requests. Try again later."})
		return
	}
	s.resetIP.fail(ip)
	email := normalizeEmail(r.FormValue("email"))
	var userID int64
	err := s.db.QueryRow(r.Context(), `SELECT id FROM users WHERE email = $1`, email).Scan(&userID)
	if err == nil {
		link, err := s.newResetLink(r.Context(), userID, time.Hour)
		if err != nil {
			serverError(w, err)
			return
		}
		if err := s.sendMail(email, "Reset your NoteTaker password", fmt.Sprintf(resetBody, link)); err != nil {
			log.Printf("send reset to %s: %v", email, err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, err)
		return
	}
	s.render(w, "forgot.html", map[string]any{"Sent": true})
}

func (s *server) resetPage(w http.ResponseWriter, r *http.Request) {
	var exists bool
	s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM password_resets WHERE token_hash = $1 AND expires_at > now())`,
		hashToken(r.PathValue("token"))).Scan(&exists)
	s.render(w, "reset.html", map[string]any{"Invalid": !exists, "Token": r.PathValue("token")})
}

func (s *server) resetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	password := r.FormValue("password")
	if len(password) < 8 {
		s.render(w, "reset.html", map[string]any{"Token": token, "Error": "Use at least 8 characters."})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		serverError(w, err)
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var userID int64
	err = tx.QueryRow(ctx, `DELETE FROM password_resets WHERE token_hash = $1 AND expires_at > now() RETURNING user_id`,
		hashToken(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.render(w, "reset.html", map[string]any{"Invalid": true})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	// A new password signs out every existing session.
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $1 WHERE id = $2`, hash, userID); err != nil {
		serverError(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	s.hub.kick(userID)
	if err := s.startSession(ctx, w, userID); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/")
}
