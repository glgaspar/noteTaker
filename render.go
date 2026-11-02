package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	_ "time/tzdata" // zone names work even in minimal containers
)

//go:embed templates
var templateFS embed.FS

// parseTemplates pairs each page with the shared layout and partials.
func parseTemplates(funcs template.FuncMap) map[string]*template.Template {
	pages, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil {
		log.Fatal(err)
	}
	m := map[string]*template.Template{}
	for _, p := range pages {
		t := template.New("layout.html").Funcs(funcs)
		m[path.Base(p)] = template.Must(t.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html", p))
	}
	return m
}

// userLoc is the viewer's time zone, reported by the browser in the "tz" cookie (see app.js),
// or the server's when unknown.
func userLoc(r *http.Request) *time.Location {
	if c, err := r.Cookie("tz"); err == nil {
		if name, err := url.QueryUnescape(c.Value); err == nil {
			if loc, err := time.LoadLocation(name); err == nil {
				return loc
			}
		}
	}
	return time.Local
}

// local converts a time to a time zone for display; templates call it as (local $.Loc .T).
func local(loc *time.Location, t time.Time) time.Time {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc)
}

// icon renders a Lucide icon from the sprite built by scripts/assets.mjs.
func (s *server) icon(name string, class ...string) template.HTML {
	return template.HTML(fmt.Sprintf(`<svg class="icon %s" aria-hidden="true"><use href="%s#%s"/></svg>`,
		template.HTMLEscapeString(strings.Join(class, " ")), s.assetURL("/static/dist/icons.svg"), template.HTMLEscapeString(name)))
}

// initials are the avatar letters for a name: "Ann Lee" -> "AL".
func initials(name string) string {
	var out []rune
	for _, word := range strings.Fields(name) {
		if r := []rune(word); unicode.IsLetter(r[0]) || unicode.IsDigit(r[0]) {
			out = append(out, unicode.ToUpper(r[0]))
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out[:min(len(out), 2)])
}

// avatarColors match the cursor colors in editor.js.
var avatarColors = []string{"#e11d48", "#2563eb", "#16a34a", "#d97706", "#9333ea", "#0891b2", "#db2777", "#65a30d"}

func avatarColor(id int64) template.CSS {
	return template.CSS(avatarColors[id%int64(len(avatarColors))])
}

// ago formats a time relative to now, in the viewer's zone: "now", "5m", "3h", "Yesterday",
// "Mon", "Jan 2", "Jan 2, 2024".
func ago(loc *time.Location, t time.Time) string {
	t = local(loc, t)
	now := time.Now().In(t.Location())
	d := now.Sub(t)
	day := func(x time.Time) time.Time { y, m, dd := x.Date(); return time.Date(y, m, dd, 0, 0, 0, 0, x.Location()) }
	days := int(day(now).Sub(day(t)).Hours() / 24)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case days == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case days == 1:
		return "Yesterday"
	case days < 7:
		return t.Format("Mon")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl[name].ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *server) renderPartial(w http.ResponseWriter, page, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl[page].ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s/%s: %v", page, name, err)
	}
}

func serverError(w http.ResponseWriter, err error) {
	if !errors.Is(err, context.Canceled) { // the client went away, e.g. a superseded live search
		log.Print(err)
	}
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
