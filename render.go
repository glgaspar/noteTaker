package main

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"path"
)

//go:embed templates
var templateFS embed.FS

// parseTemplates pairs each page with the shared layout and partials.
func parseTemplates() map[string]*template.Template {
	pages, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil {
		log.Fatal(err)
	}
	m := map[string]*template.Template{}
	for _, p := range pages {
		m[path.Base(p)] = template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html", p))
	}
	return m
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
	log.Print(err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
