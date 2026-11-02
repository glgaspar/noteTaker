package main

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"time"
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
