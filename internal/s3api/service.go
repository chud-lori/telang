package s3api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/storage"
)

// Service is the business layer between HTTP handlers and the metadata +
// storage backends. It is the place that enforces invariants (bucket exists,
// size ceilings, etc.) so handlers stay thin.
type Service struct {
	Meta       *metadata.Store
	Backend    storage.Backend
	StagingDir string
}

// PutObject uploads a new object. The reader must yield exactly `size` bytes.
// For streaming requests where the size is unknown up-front, the handler must
// buffer first and call this with the resolved size.
func (s *Service) PutObject(ctx context.Context, bucket, key, contentType string, size int64, r io.Reader) (*metadata.Object, error) {
	if _, err := s.Meta.GetBucket(ctx, bucket); err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return nil, ErrNoSuchBucket
		}
		return nil, err
	}
	if size > s.Backend.MaxObjectSize() {
		return nil, ErrEntityTooLarge
	}

	// Compute ETag (MD5 of plaintext) while uploading. In v0.1 we have no
	// encryption layer, so plaintext == ciphertext.
	h := md5.New()
	tee := io.TeeReader(r, h)

	put, err := s.Backend.Put(ctx, key, size, tee)
	if err != nil {
		if errors.Is(err, storage.ErrTooLarge) {
			return nil, ErrEntityTooLarge
		}
		return nil, fmt.Errorf("backend put: %w", err)
	}
	etag := hex.EncodeToString(h.Sum(nil))

	obj := &metadata.Object{
		Bucket:         bucket,
		Key:            key,
		Size:           size,
		CiphertextSize: size,
		ETag:           etag,
		ContentType:    contentType,
		MessageID:      put.MessageID,
		FileID:         put.FileID,
		Nonce:          []byte{}, // encryption is v0.2
	}
	if err := s.Meta.PutObject(ctx, obj); err != nil {
		// Best-effort rollback of the Telegram message so we don't leak orphans.
		_ = s.Backend.Delete(ctx, put.Ref)
		return nil, fmt.Errorf("metadata: %w", err)
	}
	return obj, nil
}

func (s *Service) GetObject(ctx context.Context, bucket, key string) (*metadata.Object, io.ReadCloser, error) {
	obj, err := s.Meta.GetObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return nil, nil, ErrNoSuchKey
		}
		return nil, nil, err
	}
	body, err := s.Backend.Get(ctx, storage.Ref{MessageID: obj.MessageID, FileID: obj.FileID})
	if err != nil {
		return nil, nil, err
	}
	return obj, body, nil
}

func (s *Service) HeadObject(ctx context.Context, bucket, key string) (*metadata.Object, error) {
	obj, err := s.Meta.GetObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return nil, ErrNoSuchKey
		}
		return nil, err
	}
	return obj, nil
}

func (s *Service) DeleteObject(ctx context.Context, bucket, key string) error {
	obj, err := s.Meta.DeleteObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			// S3 returns 204 even if the object did not exist.
			return nil
		}
		return err
	}
	// Try to delete the Telegram message. If it fails (e.g. message already
	// gone) the metadata row is already gone, so it's an acceptable orphan
	// that `telang fsck` will surface later.
	_ = s.Backend.Delete(ctx, storage.Ref{MessageID: obj.MessageID, FileID: obj.FileID})
	return nil
}

func (s *Service) CreateBucket(ctx context.Context, bucket string) error {
	if !validBucketName(bucket) {
		return ErrInvalidBucketName
	}
	err := s.Meta.CreateBucket(ctx, bucket, "")
	if err != nil {
		if errors.Is(err, metadata.ErrBucketExists) {
			return ErrBucketAlreadyOwnedByYou
		}
		return err
	}
	return nil
}

func (s *Service) DeleteBucket(ctx context.Context, bucket string) error {
	err := s.Meta.DeleteBucket(ctx, bucket)
	if err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return ErrNoSuchBucket
		}
		if errors.Is(err, metadata.ErrBucketNotEmpty) {
			return ErrBucketNotEmpty
		}
		return err
	}
	return nil
}

func (s *Service) ListBuckets(ctx context.Context) ([]metadata.Bucket, error) {
	return s.Meta.ListBuckets(ctx)
}

// stageTemp buffers an unknown-length body to a temp file inside the staging
// directory, capping at maxSize bytes. Returns the file (positioned at the
// start) and its size.
func (s *Service) stageTemp(r io.Reader, maxSize int64) (*os.File, int64, error) {
	if err := os.MkdirAll(s.StagingDir, 0o700); err != nil {
		return nil, 0, err
	}
	f, err := os.CreateTemp(s.StagingDir, "put-*.tmp")
	if err != nil {
		return nil, 0, err
	}
	limited := io.LimitReader(r, maxSize+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		os.Remove(f.Name())
		f.Close()
		return nil, 0, err
	}
	if n > maxSize {
		os.Remove(f.Name())
		f.Close()
		return nil, 0, ErrEntityTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		os.Remove(f.Name())
		f.Close()
		return nil, 0, err
	}
	return f, n, nil
}

// cleanupTemp removes a staged temp file. Safe to call with a nil file.
func cleanupTemp(f *os.File) {
	if f == nil {
		return
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(filepath.Clean(name))
}
