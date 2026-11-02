package main

import (
	"context"
	"html/template"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

type searchResult struct {
	ID        int64
	FolderID  int64
	Title     string
	Snippet   template.HTML
	Folder    string
	Author    string
	UpdatedAt time.Time
}

type author struct {
	ID   int64
	Name string
}

// prefixQuery turns free text into a tsquery matching every word as a prefix ("proj" finds
// "project"). Only letters and digits survive, so the result is always valid tsquery syntax.
func prefixQuery(q string) string {
	words := strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range words {
		words[i] = w + ":*"
	}
	return strings.Join(words, " & ")
}

// Snippet highlight markers; ts_headline does not escape the text, so we escape it ourselves
// and only then turn the markers into <mark> tags.
const markStart, markStop = "⟦", "⟧"

func highlight(s string) template.HTML {
	s = template.HTMLEscapeString(s)
	s = strings.NewReplacer(markStart, "<mark>", markStop, "</mark>").Replace(s)
	return template.HTML(s)
}

// parseDate reads a date input as midnight in the server's time zone, the zone timestamps
// are shown in. With nextDay it returns the following midnight, for inclusive end dates.
func parseDate(s string, nextDay bool) *time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return nil
	}
	if nextDay {
		t = t.AddDate(0, 0, 1)
	}
	return &t
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

const searchPageSize = 50

// search returns one page of results; it fetches one extra row so callers can tell whether
// there is a next page.
func (s *server) search(ctx context.Context, userID, wsID int64, page int, r *http.Request) ([]searchResult, error) {
	dateField := "updated_at"
	if r.FormValue("date_field") == "created" {
		dateField = "created_at"
	}
	rows, err := s.db.Query(ctx, `
		WITH q AS (SELECT CASE WHEN $2 = '' THEN NULL ELSE to_tsquery('notes', $2) END AS query)
		SELECT n.id, coalesce(n.folder_id, 0), n.title,
			CASE WHEN q.query IS NULL THEN left(n.content_text, 200)
			ELSE ts_headline('notes', n.content_text, q.query,
				'StartSel=`+markStart+`, StopSel=`+markStop+`, MaxFragments=2, MaxWords=25, MinWords=8') END,
			coalesce(f.name, ''), coalesce(u.name, ''), n.updated_at
		FROM notes n CROSS JOIN q
		LEFT JOIN folders f ON f.id = n.folder_id
		LEFT JOIN users u ON u.id = n.author_id
		WHERE n.workspace_id = $1
			AND effective_role($3, n.workspace_id, n.folder_id) IS NOT NULL
			AND (q.query IS NULL OR n.search @@ q.query)
			AND ($4 = '' OR n.title ILIKE '%' || $4 || '%')
			AND ($5 = 0 OR n.folder_id IN (
				WITH RECURSIVE sub AS (
					SELECT id FROM folders WHERE id = $5
					UNION ALL
					SELECT c.id FROM folders c JOIN sub ON c.parent_id = sub.id
				) SELECT id FROM sub))
			AND ($6 = 0 OR EXISTS (SELECT 1 FROM note_tags nt WHERE nt.note_id = n.id AND nt.tag_id = $6))
			AND ($7 = 0 OR n.author_id = $7)
			AND ($8::timestamptz IS NULL OR n.`+dateField+` >= $8)
			AND ($9::timestamptz IS NULL OR n.`+dateField+` < $9)
		ORDER BY CASE WHEN q.query IS NULL THEN 0 ELSE ts_rank(n.search, q.query) END DESC, n.updated_at DESC, n.id DESC
		LIMIT $10 OFFSET $11`,
		wsID, prefixQuery(r.FormValue("q")), userID, likeEscaper.Replace(strings.TrimSpace(r.FormValue("title"))),
		orZero(formID(r, "folder")), orZero(formID(r, "tag")), orZero(formID(r, "author")),
		parseDate(r.FormValue("from"), false), parseDate(r.FormValue("to"), true),
		searchPageSize+1, page*searchPageSize)
	if err != nil {
		return nil, err
	}
	var results []searchResult
	for rows.Next() {
		var res searchResult
		var snippet string
		if err := rows.Scan(&res.ID, &res.FolderID, &res.Title, &snippet, &res.Folder, &res.Author, &res.UpdatedAt); err != nil {
			return nil, err
		}
		res.Snippet = highlight(snippet)
		results = append(results, res)
	}
	return results, rows.Err()
}

// authors lists the authors of notes the user can see in a workspace.
func (s *server) authors(ctx context.Context, userID, wsID int64) ([]author, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT u.id, u.name FROM notes n JOIN users u ON u.id = n.author_id
		WHERE n.workspace_id = $1 AND effective_role($2, n.workspace_id, n.folder_id) IS NOT NULL
		ORDER BY u.name`, wsID, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[author])
}

func (s *server) searchPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	data, ok := s.sidebar(w, r)
	if !ok {
		return
	}
	ws := data["Workspace"].(workspace)
	page, _ := strconv.Atoi(r.FormValue("page"))
	page = max(page, 0)
	results, err := s.search(ctx, user.ID, ws.ID, page, r)
	if err != nil {
		serverError(w, err)
		return
	}
	if len(results) > searchPageSize {
		results = results[:searchPageSize]
		next := maps.Clone(r.URL.Query())
		next.Set("page", strconv.Itoa(page+1))
		data["NextURL"] = r.URL.Path + "?" + next.Encode()
	}
	data["Results"] = results
	if page > 0 { // "Load more" only needs the next items
		s.renderPartial(w, "search.html", "result_items", data)
		return
	}
	authors, err := s.authors(ctx, user.ID, ws.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	data["Authors"] = authors
	data["Form"] = r.Form
	s.render(w, "search.html", data)
}
