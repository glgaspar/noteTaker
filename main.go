package main

import (
	"bufio"
	"context"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

//go:embed static
var staticFS embed.FS

type server struct {
	db      *pgxpool.Pool
	tmpl    map[string]*template.Template
	baseURL string
	secure  bool
	smtp    smtpConfig
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://notetaker:notetaker@localhost:5432/notetaker?sslmode=disable"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		log.Fatal("migrate: ", err)
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		err = serve(db)
	case "create-admin":
		err = createAdmin(ctx, db, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q (use serve or create-admin)", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(db *pgxpool.Pool) error {
	baseURL := strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/")
	s := &server{
		db:      db,
		tmpl:    parseTemplates(),
		baseURL: baseURL,
		secure:  strings.HasPrefix(baseURL, "https://"),
		smtp: smtpConfig{
			host: os.Getenv("SMTP_HOST"),
			port: env("SMTP_PORT", "587"),
			user: os.Getenv("SMTP_USER"),
			pass: os.Getenv("SMTP_PASS"),
			from: env("SMTP_FROM", "notetaker@localhost"),
		},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("POST /logout", s.requireUser(http.HandlerFunc(s.logout)))
	mux.HandleFunc("GET /invite/{token}", s.invitePage)
	mux.HandleFunc("POST /invite/{token}", s.acceptInvite)
	auth := func(h http.HandlerFunc) http.Handler { return s.requireUser(h) }
	mux.Handle("GET /{$}", auth(s.home))
	mux.Handle("POST /workspaces", auth(s.createWorkspace))
	mux.Handle("GET /w/{id}", auth(s.workspacePage))
	mux.Handle("POST /w/{id}/rename", auth(s.renameWorkspace))
	mux.Handle("POST /w/{id}/delete", auth(s.deleteWorkspace))
	mux.Handle("POST /w/{id}/folders", auth(s.createFolder))
	mux.Handle("POST /folders/{id}/rename", auth(s.renameFolder))
	mux.Handle("POST /folders/{id}/delete", auth(s.deleteFolder))
	mux.Handle("GET /admin/users", s.requireAdmin(http.HandlerFunc(s.adminUsers)))
	mux.Handle("POST /admin/invites", s.requireAdmin(http.HandlerFunc(s.createInvite)))

	addr := env("ADDR", ":8080")
	log.Printf("listening on %s (%s)", addr, baseURL)
	srv := &http.Server{
		Addr:              addr,
		Handler:           http.NewCrossOriginProtection().Handler(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func createAdmin(ctx context.Context, db *pgxpool.Pool, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: notetaker create-admin <email> <name>")
	}
	fmt.Print("Password: ")
	var pw []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
	} else {
		var line string
		line, err = bufio.NewReader(os.Stdin).ReadString('\n')
		pw = []byte(strings.TrimRight(line, "\r\n"))
	}
	if err != nil && len(pw) == 0 {
		return err
	}
	if len(pw) < 8 {
		return fmt.Errorf("password must have at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO users (email, name, password_hash, is_admin) VALUES ($1, $2, $3, true)`,
		normalizeEmail(args[0]), args[1], hash)
	if err == nil {
		fmt.Println("admin created")
	}
	return err
}
