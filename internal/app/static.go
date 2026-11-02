package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"notetaker"
	"path"
	"strings"
)

type asset struct {
	data, gz []byte // gz is nil when compressing does not help
	ctype    string
	version  string // content hash, for cache busting
}

// loadAssets reads the embedded static files once, precompressing them.
func loadAssets() (map[string]*asset, error) {
	assets := map[string]*asset{}
	err := fs.WalkDir(notetaker.Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := notetaker.Static.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		a := &asset{data: data, ctype: mime.TypeByExtension(path.Ext(p)), version: hex.EncodeToString(sum[:6])}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(data)
		zw.Close()
		if buf.Len() < len(data)*9/10 {
			a.gz = buf.Bytes()
		}
		assets["/"+p] = a
		return nil
	})
	return assets, err
}

// assetURL adds the content version to a static path, so browsers can cache it forever.
func (s *Server) assetURL(p string) string {
	if a := s.assets[p]; a != nil {
		return p + "?v=" + a.version
	}
	return p
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	a := s.assets[r.URL.Path]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Vary", "Accept-Encoding")
	h.Set("ETag", `"`+a.version+`"`)
	if r.URL.Query().Get("v") == a.version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == `"`+a.version+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.data
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	w.Write(body)
}
