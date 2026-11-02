// Package app is the NoteTaker web application: HTTP handlers, pages and permissions.
package app

import (
	"context"
	"html/template"
	"net/http"
	"notetaker/internal/collab"
	"notetaker/internal/storage"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Server serves the app; build it with New.
type Server struct {
	db         *pgxpool.Pool
	files      *storage.Storage // nil when attachment storage is not configured
	baseURL    string
	secure     bool
	smtp       SMTPConfig
	trustProxy bool
	tmpl       map[string]*template.Template
	hub        collab.Hub
	assets     map[string]*asset

	loginAccount *limiter // failed logins per email and address
	loginIP      *limiter // failed logins per address
	resetIP      *limiter // password reset requests per address
}

type Config struct {
	DB         *pgxpool.Pool
	Files      *storage.Storage // nil disables images and attachments
	BaseURL    string           // public URL, for links in emails
	SMTP       SMTPConfig       // empty Host: emails are only logged
	TrustProxy bool             // take client addresses from X-Forwarded-For
}

func New(cfg Config) (*Server, error) {
	assets, err := loadAssets()
	if err != nil {
		return nil, err
	}
	s := &Server{
		db:           cfg.DB,
		files:        cfg.Files,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		secure:       strings.HasPrefix(cfg.BaseURL, "https://"),
		smtp:         cfg.SMTP,
		trustProxy:   cfg.TrustProxy,
		assets:       assets,
		loginAccount: newLimiter(5, 15*time.Minute),
		loginIP:      newLimiter(30, 15*time.Minute),
		resetIP:      newLimiter(5, time.Hour),
	}
	s.tmpl = parseTemplates(template.FuncMap{
		"asset": s.assetURL, "local": local, "icon": s.icon, "initials": initials, "color": avatarColor, "ago": ago,
	})
	return s, nil
}

// StartJobs marks imports cut short by a restart and starts sweeping orphaned files.
func (s *Server) StartJobs() {
	recoverImports(context.Background(), s.db)
	if s.files != nil {
		go sweepFilesEvery(s.db, s.files, 6*time.Hour)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/", s.serveStatic)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("POST /logout", s.requireUser(http.HandlerFunc(s.logout)))
	mux.HandleFunc("GET /forgot", s.forgotPage)
	mux.HandleFunc("POST /forgot", s.forgot)
	mux.HandleFunc("GET /reset/{token}", s.resetPage)
	mux.HandleFunc("POST /reset/{token}", s.resetPassword)
	mux.HandleFunc("GET /invite/{token}", s.invitePage)
	mux.HandleFunc("POST /invite/{token}", s.acceptInvite)
	auth := func(h http.HandlerFunc) http.Handler { return s.requireUser(h) }
	mux.Handle("GET /{$}", auth(s.home))
	mux.Handle("POST /workspaces", auth(s.createWorkspace))
	mux.Handle("GET /w/{id}", auth(s.workspacePage))
	mux.Handle("POST /w/{id}/rename", auth(s.renameWorkspace))
	mux.Handle("POST /w/{id}/delete", auth(s.deleteWorkspace))
	mux.Handle("GET /w/{id}/search", auth(s.searchPage))
	mux.Handle("GET /w/{id}/palette", auth(s.palette))
	mux.Handle("GET /w/{id}/import", auth(s.importPage))
	mux.Handle("POST /w/{id}/import", auth(s.importNotes))
	mux.Handle("GET /w/{id}/imports/{job}", auth(s.importJobPage))
	mux.Handle("GET /files/{id}", auth(s.serveFile))
	mux.Handle("POST /notes/{id}/attachments", auth(s.uploadFile))
	mux.Handle("GET /w/{id}/members", auth(s.workspaceMembers))
	mux.Handle("POST /w/{id}/members", auth(s.addWorkspaceMember))
	mux.Handle("POST /w/{id}/members/{user}/delete", auth(s.removeWorkspaceMember))
	mux.Handle("POST /w/{id}/folders", auth(s.createFolder))
	mux.Handle("POST /folders/{id}/rename", auth(s.renameFolder))
	mux.Handle("POST /folders/{id}/delete", auth(s.deleteFolder))
	mux.Handle("GET /folders/{id}/move", auth(s.moveFolderPage))
	mux.Handle("POST /folders/{id}/move", auth(s.moveFolder))
	mux.Handle("GET /folders/{id}/share", auth(s.folderShares))
	mux.Handle("POST /folders/{id}/share", auth(s.addFolderShare))
	mux.Handle("POST /folders/{id}/share/{user}/delete", auth(s.removeFolderShare))
	mux.Handle("POST /w/{id}/notes", auth(s.createNote))
	mux.Handle("PUT /notes/{id}", auth(s.saveNote))
	mux.Handle("GET /notes/{id}/ws", auth(s.noteSocket))
	mux.Handle("POST /notes/{id}/move", auth(s.moveNote))
	mux.Handle("POST /notes/{id}/delete", auth(s.deleteNote))
	mux.Handle("POST /notes/{id}/tags", auth(s.addTag))
	mux.Handle("POST /notes/{id}/tags/{tag}/delete", auth(s.removeTag))
	mux.Handle("GET /admin/users", s.requireAdmin(http.HandlerFunc(s.adminUsers)))
	mux.Handle("POST /admin/invites", s.requireAdmin(http.HandlerFunc(s.createInvite)))
	mux.Handle("POST /admin/users/{id}/reset", s.requireAdmin(http.HandlerFunc(s.adminReset)))
	mux.Handle("POST /admin/users/{id}/disable", s.requireAdmin(s.setDisabled(true)))
	mux.Handle("POST /admin/users/{id}/enable", s.requireAdmin(s.setDisabled(false)))
	return http.NewCrossOriginProtection().Handler(mux)
}
