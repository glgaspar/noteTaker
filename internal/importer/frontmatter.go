package importer

import (
	"encoding/json"
	"strings"
	"time"
)

// FrontMatter is the metadata block at the top of a Markdown file, as NoteTaker exports it
// and as Obsidian writes it.
type FrontMatter struct {
	Title            string
	Tags             []string
	Created, Updated *time.Time
}

// SplitFrontMatter separates a leading "---" YAML block from the Markdown. It understands
// the simple forms in practice: "key: value", quoted or JSON values, and tags given as
// [a, b] or as "- a" lines.
func SplitFrontMatter(src []byte) (FrontMatter, []byte) {
	var fm FrontMatter
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return fm, src
	}
	block, rest, ok := strings.Cut(text[4:], "\n---")
	if !ok {
		return fm, src
	}
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = ""
	}
	key := ""
	for _, line := range strings.Split(block, "\n") {
		if item, isItem := strings.CutPrefix(strings.TrimSpace(line), "- "); isItem && key == "tags" {
			fm.Tags = append(fm.Tags, strings.TrimPrefix(scalar(item), "#"))
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch key {
		case "title":
			fm.Title = scalar(v)
		case "tags":
			fm.Tags = append(fm.Tags, list(v)...)
		case "created", "date":
			fm.Created = date(scalar(v))
		case "updated", "modified":
			fm.Updated = date(scalar(v))
		}
	}
	return fm, []byte(strings.TrimLeft(rest, "\n"))
}

func scalar(v string) string {
	var s string
	if strings.HasPrefix(v, `"`) && json.Unmarshal([]byte(v), &s) == nil {
		return s
	}
	return strings.Trim(v, `'"`)
}

func list(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) == nil {
		return out
	}
	for _, item := range strings.Split(strings.Trim(v, "[]"), ",") {
		if item = scalar(strings.TrimSpace(item)); item != "" {
			out = append(out, strings.TrimPrefix(item, "#"))
		}
	}
	return out
}

func date(v string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t
		}
	}
	return nil
}
