package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// API sources poll a JSON URL every few minutes; each record in the answer becomes an inbox
// item once. Records are told apart by an id field, or by their content when they have none.

const (
	apiMaxBody     = 1 << 20 // bytes of an answer
	apiPerCheck    = 100     // new records per check; the rest come next time
	apiMinEvery    = 5       // minutes between checks, at least
	apiDefaultEach = 15
)

var apiClient = &http.Client{Timeout: 30 * time.Second}

type apiSource struct {
	SourceID, UserID int64
	URL              string
	Headers          map[string]string
	ItemsPath        string
	IDField          string
	Every            int
}

type apiRecord struct {
	ID  string
	Raw string // indented JSON
}

// jsonPath follows a dot path ("data.items", "results.0") into decoded JSON.
func jsonPath(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, key := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			if v = x[key]; v == nil {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// fetchRecords asks the API and returns the records at ItemsPath: each element of a list, or
// the one object found there.
func fetchRecords(ctx context.Context, a apiSource) ([]apiRecord, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range a.Headers {
		req.Header.Set(k, v)
	}
	res, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, apiMaxBody+1))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the API answered %d: %.200s", res.StatusCode, body)
	}
	if len(body) > apiMaxBody {
		return nil, errors.New("the answer is over 1 MB")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep large ids exact
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the answer isn't JSON: %v", err)
	}
	found, ok := jsonPath(doc, a.ItemsPath)
	if !ok {
		return nil, fmt.Errorf("nothing at %q in the answer", a.ItemsPath)
	}
	var items []any
	switch x := found.(type) {
	case []any:
		items = x
	case map[string]any:
		items = []any{x}
	default:
		return nil, fmt.Errorf("%q is neither a list nor an object", a.ItemsPath)
	}
	records := make([]apiRecord, 0, len(items))
	for _, item := range items {
		raw, _ := json.MarshalIndent(item, "", "  ")
		var id string
		if v, ok := jsonPath(item, a.IDField); ok && a.IDField != "" {
			switch v.(type) {
			case string, json.Number:
				id = fmt.Sprint(v)
			}
		}
		if id == "" {
			sum := sha256.Sum256(raw)
			id = "sha256:" + hex.EncodeToString(sum[:])
		}
		records = append(records, apiRecord{ID: id, Raw: string(raw)})
	}
	return records, nil
}

// checkAPI adds the records not seen before to the user's inbox. With skip (the first check of
// a source that shouldn't import what is already there) they are only recorded as seen.
func (s *Server) checkAPI(ctx context.Context, a apiSource, skip bool) error {
	records, err := fetchRecords(ctx, a)
	if err != nil {
		return err
	}
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	rows, err := s.db.Query(ctx, `SELECT external_id FROM inbox_items WHERE source_id = $1 AND external_id = ANY($2)`, a.SourceID, ids)
	if err != nil {
		return err
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range known {
		seen[id] = true
	}
	added := 0
	for _, r := range records {
		if seen[r.ID] || added >= apiPerCheck {
			continue
		}
		seen[r.ID] = true // a list may repeat a record
		if skip {
			_, err = s.db.Exec(ctx, `INSERT INTO inbox_items (user_id, source_id, external_id, raw, title, status)
				VALUES ($1, $2, $3, '', '', 'skipped') ON CONFLICT DO NOTHING`, a.UserID, a.SourceID, r.ID)
		} else {
			sg := suggest(r.Raw)
			sg.ExternalID = r.ID
			err = s.addToInbox(ctx, a.UserID, a.SourceID, r.Raw, sg)
			added++
		}
		if errors.Is(err, errInboxFull) {
			return nil // the rest come once the user handles some items
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- the checker ----

func (s *Server) checkDueAPIs(ctx context.Context) error {
	rows, err := s.db.Query(ctx, `SELECT source_id FROM api_sources
		WHERE checked_at < now() - make_interval(mins => every_minutes) ORDER BY checked_at`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.checkAPINow(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// checkAPINow checks one source and records when and how it went. Only database errors are
// returned; the API's own are shown on the sources page.
func (s *Server) checkAPINow(ctx context.Context, sourceID int64) error {
	a, err := s.loadAPISource(ctx, sourceID)
	if err == nil {
		err = s.checkAPI(ctx, a, false)
	}
	msg := ""
	if err != nil {
		var dbErr interface{ SQLState() string }
		if errors.As(err, &dbErr) || errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		msg = err.Error()
	}
	_, err = s.db.Exec(ctx, `UPDATE api_sources SET checked_at = now(), last_error = $2 WHERE source_id = $1`, sourceID, msg)
	return err
}

// loadAPISource reads a source and decrypts its headers.
func (s *Server) loadAPISource(ctx context.Context, sourceID int64) (apiSource, error) {
	a, sealed, err := s.loadAPIRow(ctx, sourceID)
	if err != nil || sealed == nil {
		return a, err
	}
	plain, err := s.secrets.open(sealed)
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal([]byte(plain), &a.Headers)
}

func (s *Server) loadAPIRow(ctx context.Context, sourceID int64) (a apiSource, sealedHeaders []byte, err error) {
	err = s.db.QueryRow(ctx, `SELECT a.source_id, s.user_id, a.url, a.headers, a.items_path, a.id_field, a.every_minutes
		FROM api_sources a JOIN task_sources s ON s.id = a.source_id WHERE a.source_id = $1`, sourceID).
		Scan(&a.SourceID, &a.UserID, &a.URL, &sealedHeaders, &a.ItemsPath, &a.IDField, &a.Every)
	return a, sealedHeaders, err
}

// ---- adding, editing and checking API sources ----

// apiForm reads the API form. Headers are "Name: value" lines; none given keeps the saved ones
// when editing (unless "clear_headers" is set).
func apiForm(r *http.Request) (name string, a apiSource, problem string) {
	name = strings.TrimSpace(r.FormValue("name"))
	a = apiSource{URL: strings.TrimSpace(r.FormValue("url")), ItemsPath: strings.Trim(strings.TrimSpace(r.FormValue("items_path")), "."),
		IDField: strings.Trim(strings.TrimSpace(r.FormValue("id_field")), ".")}
	a.Every, _ = strconv.Atoi(r.FormValue("every"))
	if a.Every == 0 {
		a.Every = apiDefaultEach
	}
	for _, line := range strings.Split(r.FormValue("headers"), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if k = strings.TrimSpace(k); !ok || k == "" || strings.ContainsAny(k, " \t") {
			return name, a, fmt.Sprintf("Write each header as “Name: value” (%q isn't).", line)
		}
		if a.Headers == nil {
			a.Headers = map[string]string{}
		}
		a.Headers[k] = strings.TrimSpace(v)
	}
	u, err := url.Parse(a.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		problem = "The URL must start with http:// or https://."
	case a.Every < apiMinEvery:
		problem = fmt.Sprintf("Check at most every %d minutes.", apiMinEvery)
	}
	if name == "" && err == nil {
		name = u.Host
	}
	return name, a, problem
}

func (s *Server) apiError(w http.ResponseWriter, r *http.Request, id int64, msg string) {
	s.renderSources(w, r, map[string]any{"APIError": msg, "APIFormID": id, "APIForm": map[string]string{
		"name": r.FormValue("name"), "url": r.FormValue("url"), "items_path": r.FormValue("items_path"),
		"id_field": r.FormValue("id_field"), "every": r.FormValue("every"), "import": r.FormValue("import"), "tags": r.FormValue("tags")}})
}

// sealHeaders encrypts headers for storage; nil when there are none.
func (s *Server) sealHeaders(h map[string]string) ([]byte, error) {
	if len(h) == 0 {
		return nil, nil
	}
	if s.secrets == nil {
		return nil, errors.New("Headers can't be saved: the server has no SECRET_KEY set.")
	}
	plain, _ := json.Marshal(h)
	return s.secrets.seal(string(plain)), nil
}

func (s *Server) addAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, a, problem := apiForm(r)
	if problem != "" {
		s.apiError(w, r, 0, problem)
		return
	}
	sealed, err := s.sealHeaders(a.Headers)
	if err != nil {
		s.apiError(w, r, 0, err.Error())
		return
	}
	if _, err := fetchRecords(ctx, a); err != nil {
		s.apiError(w, r, 0, "The API couldn't be read: "+err.Error())
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `INSERT INTO task_sources (user_id, kind, name, tags) VALUES ($1, 'api', $2, $3) RETURNING id`,
		currentUser(r).ID, name, formTags(r)).Scan(&a.SourceID)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO api_sources (source_id, url, headers, items_path, id_field, every_minutes, checked_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())`, a.SourceID, a.URL, sealed, a.ItemsPath, a.IDField, a.Every)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The first check: what is already there is imported, or only noted as seen.
	a.UserID = currentUser(r).ID
	msg := ""
	if err := s.checkAPI(ctx, a, r.FormValue("import") == ""); err != nil {
		var dbErr interface{ SQLState() string }
		if errors.As(err, &dbErr) {
			s.serverError(w, r, err)
			return
		}
		msg = err.Error()
	}
	if _, err := s.db.Exec(ctx, `UPDATE api_sources SET last_error = $2 WHERE source_id = $1`, a.SourceID, msg); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}

// ownAPI loads the {id} API source if it is the current user's; otherwise it answers 404.
func (s *Server) ownAPI(w http.ResponseWriter, r *http.Request) (apiSource, []byte, bool) {
	a, sealed, err := s.loadAPIRow(r.Context(), pathID(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.UserID != currentUser(r).ID) {
		s.fail(w, r, http.StatusNotFound, "")
		return a, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return a, nil, false
	}
	return a, sealed, true
}

func (s *Server) editAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	old, sealed, ok := s.ownAPI(w, r)
	if !ok {
		return
	}
	name, a, problem := apiForm(r)
	if problem != "" {
		s.apiError(w, r, old.SourceID, problem)
		return
	}
	a.SourceID, a.UserID = old.SourceID, old.UserID
	switch {
	case len(a.Headers) > 0:
		var err error
		if sealed, err = s.sealHeaders(a.Headers); err != nil {
			s.apiError(w, r, old.SourceID, err.Error())
			return
		}
	case r.FormValue("clear_headers") != "":
		sealed = nil
	case sealed != nil:
		plain, err := s.secrets.open(sealed)
		if err != nil {
			s.apiError(w, r, old.SourceID, "Enter the headers again: "+err.Error())
			return
		}
		json.Unmarshal([]byte(plain), &a.Headers)
	}
	if _, err := fetchRecords(ctx, a); err != nil {
		s.apiError(w, r, old.SourceID, "The API couldn't be read: "+err.Error())
		return
	}
	if _, err := s.db.Exec(ctx, `
		WITH s AS (UPDATE task_sources SET name = $2 WHERE id = $1)
		UPDATE api_sources SET url = $3, headers = $4, items_path = $5, id_field = $6, every_minutes = $7, last_error = ''
		WHERE source_id = $1`, a.SourceID, name, a.URL, sealed, a.ItemsPath, a.IDField, a.Every); err != nil {
		s.serverError(w, r, err)
		return
	}
	// Records the source now points at are only noted as seen, as when it was added, unless
	// the user asks to import them.
	if a.URL != old.URL || a.ItemsPath != old.ItemsPath || a.IDField != old.IDField || r.FormValue("import") != "" {
		if err := s.checkAPI(ctx, a, r.FormValue("import") == ""); err != nil {
			var dbErr interface{ SQLState() string }
			if errors.As(err, &dbErr) {
				s.serverError(w, r, err)
				return
			}
		}
	}
	redirect(w, r, "/tasks/sources")
}

func (s *Server) checkAPIHandler(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.ownAPI(w, r)
	if !ok {
		return
	}
	if err := s.checkAPINow(r.Context(), a.SourceID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}
