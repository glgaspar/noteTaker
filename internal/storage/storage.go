// Package storage keeps attachment bytes in an S3-compatible bucket such as RustFS.
package storage

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Config struct {
	Endpoint, AccessKey, SecretKey, Bucket string
	SSL                                    bool
}

type Storage struct {
	client *minio.Client
	bucket string
}

// New connects to the bucket, creating it if needed.
func New(ctx context.Context, cfg Config) (*Storage, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.SSL,
	})
	if err != nil {
		return nil, err
	}
	s := &Storage{client: client, bucket: cfg.Bucket}
	exists, err := client.BucketExists(ctx, s.bucket)
	if err == nil && !exists {
		err = client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
	}
	return s, err
}

func (s *Storage) Put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

// Open returns the object's contents and modification time.
func (s *Storage) Open(ctx context.Context, key string) (io.ReadSeekCloser, time.Time, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, time.Time{}, err
	}
	return obj, info.LastModified, nil
}

func (s *Storage) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// List calls fn with every key under prefix.
func (s *Storage) List(ctx context.Context, prefix string, fn func(key string) error) error {
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := fn(obj.Key); err != nil {
			return err
		}
	}
	return nil
}
