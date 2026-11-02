package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"notetaker/internal/importer"
	"notetaker/internal/storage"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func objectKey(id int64) string { return fmt.Sprintf("attachments/%d", id) }

// putAttachment stores a file for a note and returns the attachment id.
func (s *Server) putAttachment(ctx context.Context, noteID int64, name, mimeType string, data []byte) (int64, error) {
	if s.files == nil {
		return 0, errors.New("attachment storage is not configured")
	}
	var id int64
	if err := s.db.QueryRow(ctx, `INSERT INTO attachments (note_id, name, mime, size) VALUES ($1, $2, $3, $4) RETURNING id`,
		noteID, name, mimeType, len(data)).Scan(&id); err != nil {
		return 0, err
	}
	if err := s.files.Put(ctx, objectKey(id), mimeType, data); err != nil {
		s.db.Exec(ctx, `DELETE FROM attachments WHERE id = $1`, id)
		return 0, err
	}
	return id, nil
}

const maxUpload = importer.MaxFile

// uploadFile stores a file pasted, dropped or attached in the editor.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, noteRole); !ok {
		return
	}
	if s.files == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "file storage is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	f, fh, err := r.FormFile("file")
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "file missing or larger than 25 MB")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil || len(data) > maxUpload {
		s.fail(w, r, http.StatusBadRequest, "file larger than 25 MB")
		return
	}
	mimeType := fh.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	id, err := s.putAttachment(r.Context(), pathID(r, "id"), fh.Filename, mimeType, data)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"url": fmt.Sprintf("/files/%d", id), "name": fh.Filename, "image": strings.HasPrefix(mimeType, "image/"),
	})
}

// serveFile streams an attachment to users who can see its note.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var noteID int64
	var name, mimeType string
	err := s.db.QueryRow(ctx, `SELECT note_id, name, mime FROM attachments WHERE id = $1`, pathID(r, "id")).
		Scan(&noteID, &name, &mimeType)
	if errors.Is(err, pgx.ErrNoRows) || s.files == nil {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ws, err := s.roleOf(ctx, currentUser(r).ID, noteRole, noteID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ws.Role == "" {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	obj, modified, err := s.files.Open(ctx, objectKey(pathID(r, "id")))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer obj.Close()
	disposition := "attachment"
	if strings.HasPrefix(mimeType, "image/") {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	// Uploaded files are untrusted: never let them run scripts (e.g. SVG or HTML opened directly).
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, name, modified, obj)
}

// SweepFiles deletes stored files whose attachment row is gone (deleted notes, folders or
// workspaces cascade to the rows, not to the bucket). Rows are written before their file,
// so a file without a row is always garbage.
func SweepFiles(ctx context.Context, db *pgxpool.Pool, st *storage.Storage) (int, error) {
	removed := 0
	batch := map[int64]string{}
	flush := func() error {
		ids := slices.Collect(maps.Keys(batch))
		rows, err := db.Query(ctx, `SELECT id FROM attachments WHERE id = ANY($1)`, ids)
		if err != nil {
			return err
		}
		live, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, id := range live {
			delete(batch, id)
		}
		for _, key := range batch {
			if err := st.Remove(ctx, key); err != nil {
				return err
			}
			removed++
		}
		clear(batch)
		return nil
	}
	err := st.List(ctx, "attachments/", func(key string) error {
		id, err := strconv.ParseInt(strings.TrimPrefix(key, "attachments/"), 10, 64)
		if err != nil {
			return nil
		}
		batch[id] = key
		if len(batch) == 1000 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return removed, err
	}
	return removed, flush()
}

// sweepFilesEvery runs SweepFiles now and then on an interval, for the life of the process.
func sweepFilesEvery(db *pgxpool.Pool, st *storage.Storage, interval time.Duration) {
	for {
		if n, err := SweepFiles(context.Background(), db, st); err != nil {
			log.Printf("sweep files: %v", err)
		} else if n > 0 {
			log.Printf("sweep files: removed %d orphaned files", n)
		}
		time.Sleep(interval)
	}
}
