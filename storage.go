package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// storage keeps attachment bytes in an S3-compatible bucket (RustFS); rows live in attachments.
type storage struct {
	client *minio.Client
	bucket string
}

// newStorage connects to S3_ENDPOINT, creating the bucket if needed. It returns nil when
// S3_ENDPOINT is not set; attachments are then unavailable.
func newStorage(ctx context.Context) (*storage, error) {
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), ""),
		Secure: os.Getenv("S3_SSL") == "true",
	})
	if err != nil {
		return nil, err
	}
	st := &storage{client: client, bucket: env("S3_BUCKET", "notetaker")}
	exists, err := client.BucketExists(ctx, st.bucket)
	if err == nil && !exists {
		err = client.MakeBucket(ctx, st.bucket, minio.MakeBucketOptions{})
	}
	return st, err
}

func objectKey(id int64) string { return fmt.Sprintf("attachments/%d", id) }

// putAttachment stores a file for a note and returns the attachment id.
func (s *server) putAttachment(ctx context.Context, noteID int64, name, mimeType string, data []byte) (int64, error) {
	if s.files == nil {
		return 0, errors.New("attachment storage is not configured")
	}
	var id int64
	if err := s.db.QueryRow(ctx, `INSERT INTO attachments (note_id, name, mime, size) VALUES ($1, $2, $3, $4) RETURNING id`,
		noteID, name, mimeType, len(data)).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := s.files.client.PutObject(ctx, s.files.bucket, objectKey(id), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: mimeType}); err != nil {
		s.db.Exec(ctx, `DELETE FROM attachments WHERE id = $1`, id)
		return 0, err
	}
	return id, nil
}

const maxUpload = 25 << 20

// uploadFile stores a file pasted, dropped or attached in the editor.
func (s *server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.editable(w, r, noteRole); !ok {
		return
	}
	if s.files == nil {
		http.Error(w, "file storage is not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	f, fh, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file missing or larger than 25 MB", http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil || len(data) > maxUpload {
		http.Error(w, "file larger than 25 MB", http.StatusBadRequest)
		return
	}
	mimeType := fh.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	id, err := s.putAttachment(r.Context(), pathID(r, "id"), fh.Filename, mimeType, data)
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"url": fmt.Sprintf("/files/%d", id), "name": fh.Filename, "image": strings.HasPrefix(mimeType, "image/"),
	})
}

// serveFile streams an attachment to users who can see its note.
func (s *server) serveFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var noteID int64
	var name, mimeType string
	err := s.db.QueryRow(ctx, `SELECT note_id, name, mime FROM attachments WHERE id = $1`, pathID(r, "id")).
		Scan(&noteID, &name, &mimeType)
	if errors.Is(err, pgx.ErrNoRows) || s.files == nil {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	ws, err := s.roleOf(ctx, currentUser(r).ID, noteRole, noteID)
	if err != nil {
		serverError(w, err)
		return
	}
	if ws.Role == "" {
		http.NotFound(w, r)
		return
	}
	obj, err := s.files.client.GetObject(ctx, s.files.bucket, objectKey(pathID(r, "id")), minio.GetObjectOptions{})
	if err != nil {
		serverError(w, err)
		return
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		serverError(w, err)
		return
	}
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
	http.ServeContent(w, r, name, info.LastModified, obj)
}

// sweepFiles deletes stored files whose attachment row is gone (deleted notes, folders or
// workspaces cascade to the rows, not to the bucket). Rows are written before their file,
// so a file without a row is always garbage.
func sweepFiles(ctx context.Context, db *pgxpool.Pool, st *storage) (int, error) {
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
			if err := st.client.RemoveObject(ctx, st.bucket, key, minio.RemoveObjectOptions{}); err != nil {
				return err
			}
			removed++
		}
		clear(batch)
		return nil
	}
	for obj := range st.client.ListObjects(ctx, st.bucket, minio.ListObjectsOptions{Prefix: "attachments/", Recursive: true}) {
		if obj.Err != nil {
			return removed, obj.Err
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(obj.Key, "attachments/"), 10, 64)
		if err != nil {
			continue
		}
		batch[id] = obj.Key
		if len(batch) == 1000 {
			if err := flush(); err != nil {
				return removed, err
			}
		}
	}
	return removed, flush()
}

// sweepFilesEvery runs sweepFiles now and then on an interval, for the life of the process.
func sweepFilesEvery(db *pgxpool.Pool, st *storage, interval time.Duration) {
	for {
		if n, err := sweepFiles(context.Background(), db, st); err != nil {
			log.Printf("sweep files: %v", err)
		} else if n > 0 {
			log.Printf("sweep files: removed %d orphaned files", n)
		}
		time.Sleep(interval)
	}
}
