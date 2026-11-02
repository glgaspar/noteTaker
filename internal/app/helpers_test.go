package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"notetaker"
	"notetaker/internal/db"
	"notetaker/internal/storage"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Integration tests run against a real Postgres. TEST_DATABASE_URL names a database that is
// dropped and recreated on every run (default: notetaker_test on the docker-compose server).
// Storage tests also need RustFS (TEST_S3_ENDPOINT, default localhost:9000) and skip without it.

var (
	testDB    *pgxpool.Pool
	testFiles *storage.Storage
	seq       atomic.Int64 // makes test data unique
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	log.SetOutput(io.Discard) // server logs (e.g. emails without SMTP) are noise here; failures show in statuses
	dsn := env("TEST_DATABASE_URL", "postgres://notetaker:notetaker@localhost:5432/notetaker_test?sslmode=disable")
	if err := recreateDB(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "test database: %v (is Postgres running? try `make db`)\n", err)
		os.Exit(1)
	}
	var err error
	if testDB, err = pgxpool.New(ctx, dsn); err != nil {
		panic(err)
	}
	if err := db.Migrate(ctx, testDB, notetaker.Migrations); err != nil {
		panic(err)
	}
	if testFiles, err = storage.New(ctx, storage.Config{
		Endpoint:  env("TEST_S3_ENDPOINT", "localhost:9000"),
		AccessKey: env("TEST_S3_ACCESS_KEY", "notetaker"),
		SecretKey: env("TEST_S3_SECRET_KEY", "notetaker-secret"),
		Bucket:    "notetaker-test",
	}); err != nil {
		testFiles = nil // storage tests skip
	}
	os.Exit(m.Run())
}

func recreateDB(ctx context.Context, dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(u.Path, "/")
	u.Path = "/postgres"
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	return err
}

type app struct {
	t   *testing.T
	s   *Server
	url string
}

// newApp starts the HTTP server; withFiles also enables attachment storage (or skips the test).
func newApp(t *testing.T, withFiles bool) *app {
	t.Helper()
	var files *storage.Storage
	if withFiles {
		if testFiles == nil {
			t.Skip("RustFS not reachable; start it with `make db`")
		}
		files = testFiles
	}
	s, err := New(Config{DB: testDB, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.baseURL = ts.URL
	return &app{t: t, s: s, url: ts.URL}
}

type user struct {
	ID    int64
	Email string
	*httpClient
}

// user creates a user (password "password") and returns a logged-in client for them.
func (a *app) user(name string, admin bool) *user {
	a.t.Helper()
	email := fmt.Sprintf("%s-%d@example.com", strings.ToLower(name), seq.Add(1))
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	var id int64
	if err := testDB.QueryRow(context.Background(), `INSERT INTO users (email, name, password_hash, is_admin) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, name, hash, admin).Scan(&id); err != nil {
		a.t.Fatal(err)
	}
	c := a.client()
	if res := c.post("/login", url.Values{"email": {email}, "password": {"password"}}); res.code != http.StatusSeeOther {
		a.t.Fatalf("login %s: %d", email, res.code)
	}
	return &user{ID: id, Email: email, httpClient: c}
}

type httpClient struct {
	t    *testing.T
	base string
	http *http.Client
}

func (a *app) client() *httpClient {
	jar, _ := cookiejar.New(nil)
	return &httpClient{t: a.t, base: a.url, http: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type response struct {
	code     int
	body     string
	location string
	header   http.Header
}

func (c *httpClient) do(req *http.Request) response {
	c.t.Helper()
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return response{code: res.StatusCode, body: string(body), location: res.Header.Get("Location"), header: res.Header}
}

func (c *httpClient) get(path string) response {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	return c.do(req)
}

func (c *httpClient) post(path string, form url.Values) response {
	c.t.Helper()
	req, _ := http.NewRequest("POST", c.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

func (c *httpClient) putJSON(path string, v any) response {
	c.t.Helper()
	body, _ := json.Marshal(v)
	req, _ := http.NewRequest("PUT", c.base+path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

var idInURL = regexp.MustCompile(`(?:/w/|folder=|note=)(\d+)$`)

// createdID reads the id of a created object from the redirect that follows its creation.
func createdID(t *testing.T, res response) int64 {
	t.Helper()
	m := idInURL.FindStringSubmatch(res.location)
	if m == nil {
		t.Fatalf("no id in redirect %q (status %d): %.200s", res.location, res.code, res.body)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

func (u *user) workspace(name string) int64 {
	return createdID(u.t, u.post("/workspaces", url.Values{"name": {name}}))
}

func (u *user) folder(ws int64, name string, parent int64) int64 {
	form := url.Values{"name": {name}}
	if parent != 0 {
		form.Set("parent_id", strconv.FormatInt(parent, 10))
	}
	return createdID(u.t, u.post(fmt.Sprintf("/w/%d/folders", ws), form))
}

// note creates a note with the given title and text, in a folder (0 = workspace root).
func (u *user) note(ws, folder int64, title, text string) int64 {
	u.t.Helper()
	form := url.Values{}
	if folder != 0 {
		form.Set("folder_id", strconv.FormatInt(folder, 10))
	}
	id := createdID(u.t, u.post(fmt.Sprintf("/w/%d/notes", ws), form))
	if res := u.putJSON(fmt.Sprintf("/notes/%d", id), map[string]any{"title": title, "content": nil, "text": text}); res.code != http.StatusNoContent {
		u.t.Fatalf("save note: %d %s", res.code, res.body)
	}
	return id
}

func (u *user) share(path, email, role string) {
	u.t.Helper()
	if res := u.post(path, url.Values{"email": {email}, "role": {role}}); res.code != http.StatusSeeOther {
		u.t.Fatalf("share %s with %s: %d %.200s", path, email, res.code, res.body)
	}
}

func dbInt(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := testDB.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
