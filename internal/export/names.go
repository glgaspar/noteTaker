package export

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// SafeName makes a title usable as a file or folder name on every common file system.
func SafeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || unicode.IsControl(r) {
			return '-'
		}
		return r
	}, name)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if r := []rune(name); len(r) > 100 {
		name = strings.TrimSpace(string(r[:100]))
	}
	if name == "" {
		return "Untitled"
	}
	return name
}

// Names hands out unique paths, treating names that differ only in case as equal (macOS and
// Windows file systems do).
type Names map[string]bool

// Unique returns dir/base+ext, or dir/base (2)+ext and so on when taken.
func (n Names) Unique(dir, base, ext string) string {
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name += " (" + strconv.Itoa(i) + ")"
		}
		p := name + ext
		if dir != "" {
			p = dir + "/" + p
		}
		if !n[strings.ToLower(p)] {
			n[strings.ToLower(p)] = true
			return p
		}
	}
}

// PathURL escapes each segment of a relative path for use in a link.
func PathURL(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
