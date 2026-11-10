// Command notetaker runs the NoteTaker server and its maintenance commands.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"notetaker"
	"notetaker/internal/app"
	"notetaker/internal/db"
	"notetaker/internal/storage"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://notetaker:notetaker@localhost:5432/notetaker?sslmode=disable"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, notetaker.Migrations); err != nil {
		log.Fatal("migrate: ", err)
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		err = serve(ctx, pool)
	case "create-admin":
		err = createAdmin(ctx, pool, os.Args[2:])
	case "sweep-files":
		err = sweepFiles(ctx, pool)
	default:
		err = fmt.Errorf("unknown command %q (use serve, create-admin or sweep-files)", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// openStorage connects to S3_ENDPOINT, or returns nil when it is not set.
func openStorage(ctx context.Context) (*storage.Storage, error) {
	if os.Getenv("S3_ENDPOINT") == "" {
		return nil, nil
	}
	return storage.New(ctx, storage.Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
		Bucket:    env("S3_BUCKET", "notetaker"),
		SSL:       os.Getenv("S3_SSL") == "true",
	})
}

func serve(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := openStorage(ctx)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if files == nil {
		log.Print("S3_ENDPOINT not set; attachments are disabled")
	}
	s, err := app.New(app.Config{
		DB:         pool,
		Files:      files,
		BaseURL:    env("BASE_URL", "http://localhost:8080"),
		TrustProxy: os.Getenv("TRUST_PROXY") == "true",
		LLM:        app.LLMConfig{URL: os.Getenv("LLM_API_URL"), Key: os.Getenv("LLM_API_KEY"), Model: os.Getenv("LLM_MODEL")},
		SecretKey:  os.Getenv("SECRET_KEY"),
		Microsoft:  app.MicrosoftConfig{ClientID: os.Getenv("MS_CLIENT_ID"), Tenant: env("MS_TENANT", "common")},
		SMTP: app.SMTPConfig{
			Host: os.Getenv("SMTP_HOST"),
			Port: env("SMTP_PORT", "587"),
			User: os.Getenv("SMTP_USER"),
			Pass: os.Getenv("SMTP_PASS"),
			From: env("SMTP_FROM", "notetaker@localhost"),
		},
	})
	if err != nil {
		return err
	}
	s.StartJobs()
	addr := env("ADDR", ":8080")
	log.Printf("listening on %s (%s)", addr, env("BASE_URL", "http://localhost:8080"))
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func createAdmin(ctx context.Context, pool *pgxpool.Pool, args []string) error {
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
	if problem := app.PasswordProblem(string(pw)); problem != "" {
		return fmt.Errorf("password: %s", problem)
	}
	hash, err := bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO users (email, name, password_hash, is_admin) VALUES ($1, $2, $3, true)`,
		strings.ToLower(strings.TrimSpace(args[0])), args[1], hash)
	if err == nil {
		fmt.Println("admin created")
	}
	return err
}

func sweepFiles(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := openStorage(ctx)
	if err != nil || files == nil {
		return fmt.Errorf("storage not available (set S3_ENDPOINT): %v", err)
	}
	n, err := app.SweepFiles(ctx, pool, files)
	fmt.Printf("removed %d orphaned files\n", n)
	return err
}
