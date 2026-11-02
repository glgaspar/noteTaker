package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
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
