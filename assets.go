// Package notetaker holds the files embedded in the binary: templates, built static assets and
// database migrations. The Go code lives in cmd/ and internal/.
package notetaker

import "embed"

var (
	//go:embed templates
	Templates embed.FS
	//go:embed static
	Static embed.FS
	//go:embed migrations/*.sql
	Migrations embed.FS
)
