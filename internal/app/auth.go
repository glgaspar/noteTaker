package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "session"
	sessionTTL    = 30 * 24 * time.Hour
)

// dummyHash keeps login timing the same whether or not the email exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)

type User struct {
	ID      int64
	Email   string
	Name    string
	IsAdmin bool
}

type userKey struct{}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userKey{}).(*User)
	return u
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// PasswordProblem says what is wrong with a new password, or "" when it is acceptable. bcrypt
// only takes 72 bytes, so longer passwords are refused rather than cut silently.
func PasswordProblem(password string) string {
	switch {
	case len(password) < 8:
		return "Use at least 8 characters."
	case len(password) > 72:
		return "Use at most 72 characters (fewer if they are accented or emoji)."
	}
	return ""
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// redirect works for both plain requests and htmx requests.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	token := rand.Text()
	if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), userID, time.Now().Add(sessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			var u User
			err := s.db.QueryRow(r.Context(), `
				SELECT u.id, u.email, u.name, u.is_admin
				FROM sessions s JOIN users u ON u.id = s.user_id
				WHERE s.token_hash = $1 AND s.expires_at > now() AND u.disabled_at IS NULL`, hashToken(c.Value)).
				Scan(&u.ID, &u.Email, &u.Name, &u.IsAdmin)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, &u)))
				return
			}
		}
		redirect(w, r, "/login")
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).IsAdmin {
			s.fail(w, r, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	email := normalizeEmail(r.FormValue("email"))
	ip := s.clientIP(r)
	account := email + "|" + ip // per email and address, so others cannot lock a user out
	accountBlocked, until := s.loginAccount.blocked(account)
	ipBlocked, ipUntil := s.loginIP.blocked(ip)
	if accountBlocked || ipBlocked {
		var wait time.Duration
		if accountBlocked {
			wait = time.Until(until)
		}
		if ipBlocked {
			wait = max(wait, time.Until(ipUntil))
		}
		minutes := max(int(wait.Round(time.Minute).Minutes()), 1)
		s.render(w, "login.html", map[string]any{
			"Error": fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", minutes), "Email": email,
		})
		return
	}
	var id int64
	var hash []byte
	err := s.db.QueryRow(r.Context(), `SELECT id, password_hash FROM users WHERE email = $1 AND disabled_at IS NULL`, email).Scan(&id, &hash)
	if err != nil {
		hash = dummyHash
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(r.FormValue("password"))) != nil || err != nil {
		s.loginAccount.fail(account)
		s.loginIP.fail(ip)
		s.render(w, "login.html", map[string]any{"Error": "Invalid email or password.", "Email": email})
		return
	}
	s.loginAccount.clear(account)
	if err := s.startSession(r.Context(), w, id); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	redirect(w, r, "/login")
}
