package s3api

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/telang/telang/internal/cache"
	"github.com/telang/telang/internal/crypto"
	"github.com/telang/telang/internal/keys"
	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/storage"
)

// mapBackendErr translates a storage-layer error into the appropriate
// *S3Error. Plain (non-sentinel) errors fall through so the handler returns
// a 500 InternalError with the original cause logged.
func mapBackendErr(err error, op string) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, storage.ErrTooLarge):
		return ErrEntityTooLarge
	case errors.Is(err, storage.ErrThrottled):
		return ErrSlowDown
	case errors.Is(err, storage.ErrUnavailable):
		return ErrServiceUnavailable
	case errors.Is(err, storage.ErrNotFound):
		return ErrNoSuchKey
	}
	return fmt.Errorf("backend %s: %w", op, err)
}

// Service is the business layer between HTTP handlers and the metadata +
// storage backends. It enforces invariants (bucket exists, size ceilings,
// etc.) and owns the encryption + cache plumbing so handlers stay thin.
type Service struct {
	Meta       *metadata.Store
	Backend    storage.Backend
	Keys       *keys.Store
	Cache      *cache.Cache // may be nil to disable caching
	StagingDir string
	FrameSize  int

	mu       sync.Mutex
	cipherCache map[string]*crypto.Cipher
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

	c, err := s.cipherFor(bucket)
	if err != nil {
		return nil, err
	}
	ciphertextSize := c.CiphertextSize(size)
	if ciphertextSize > s.Backend.MaxObjectSize() {
		return nil, ErrEntityTooLarge
	}

	// Compute plaintext MD5 (ETag) while encryption consumes the same bytes.
	md5h := md5.New()
	plainTee := io.TeeReader(r, md5h)

	base := make([]byte, crypto.NonceLen)
	if _, err := rand.Read(base); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	encReader := c.NewEncrypterWithNonce(plainTee, size, base)

	put, err := s.Backend.Put(ctx, key, ciphertextSize, encReader)
	if err != nil {
		return nil, mapBackendErr(err, "put")
	}
	etag := hex.EncodeToString(md5h.Sum(nil))

	obj := &metadata.Object{
		Bucket:         bucket,
		Key:            key,
		Size:           size,
		CiphertextSize: ciphertextSize,
		ETag:           etag,
		ContentType:    contentType,
		MessageID:      put.MessageID,
		FileID:         put.FileID,
		Nonce:          base,
	}
	if err := s.Meta.PutObject(ctx, obj); err != nil {
		// Roll back the Telegram message so we don't leak orphans.
		_ = s.Backend.Delete(ctx, put.Ref)
		return nil, fmt.Errorf("metadata: %w", err)
	}
	return obj, nil
}

// GetObject returns the plaintext stream for an object. Caller must Close.
func (s *Service) GetObject(ctx context.Context, bucket, key string) (*metadata.Object, io.ReadCloser, error) {
	obj, err := s.Meta.GetObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return nil, nil, ErrNoSuchKey
		}
		return nil, nil, err
	}
	c, err := s.cipherFor(bucket)
	if err != nil {
		return nil, nil, err
	}
	ctReader, err := s.openCiphertext(ctx, obj)
	if err != nil {
		return nil, nil, mapBackendErr(err, "get")
	}
	plain, err := c.NewDecrypter(ctReader)
	if err != nil {
		ctReader.Close()
		return nil, nil, err
	}
	return obj, struct {
		io.Reader
		io.Closer
	}{plain, ctReader}, nil
}

// GetObjectRange returns the plaintext bytes [start, end] inclusive.
func (s *Service) GetObjectRange(ctx context.Context, bucket, key string, start, end int64) (*metadata.Object, io.ReadCloser, error) {
	obj, err := s.Meta.GetObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return nil, nil, ErrNoSuchKey
		}
		return nil, nil, err
	}
	if start < 0 || end >= obj.Size || start > end {
		return nil, nil, ErrInvalidRange
	}
	c, err := s.cipherFor(bucket)
	if err != nil {
		return nil, nil, err
	}
	ra, closer, err := s.openCiphertextAt(ctx, obj)
	if err != nil {
		return nil, nil, mapBackendErr(err, "get")
	}

	pr, pw := io.Pipe()
	go func() {
		defer closer.Close()
		err := c.DecryptRange(pw, ra, obj.CiphertextSize, start, end)
		pw.CloseWithError(err)
	}()
	return obj, pr, nil
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
			return nil
		}
		return err
	}
	if s.Cache != nil {
		s.Cache.Evict(obj.MessageID)
	}
	_ = s.Backend.Delete(ctx, storage.Ref{MessageID: obj.MessageID, FileID: obj.FileID})
	return nil
}

func (s *Service) CreateBucket(ctx context.Context, bucket string) error {
	if !validBucketName(bucket) {
		return ErrInvalidBucketName
	}
	if _, err := s.Keys.Create(bucket); err != nil {
		if errors.Is(err, keys.ErrExists) {
			return ErrBucketAlreadyOwnedByYou
		}
		return fmt.Errorf("keys: %w", err)
	}
	if err := s.Meta.CreateBucket(ctx, bucket, bucket); err != nil {
		_ = s.Keys.Remove(bucket)
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
	_ = s.Keys.Remove(bucket)
	s.mu.Lock()
	delete(s.cipherCache, bucket)
	s.mu.Unlock()
	return nil
}

func (s *Service) ListBuckets(ctx context.Context) ([]metadata.Bucket, error) {
	return s.Meta.ListBuckets(ctx)
}

// --- internal helpers ---

func (s *Service) cipherFor(bucket string) (*crypto.Cipher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cipherCache == nil {
		s.cipherCache = map[string]*crypto.Cipher{}
	}
	if c, ok := s.cipherCache[bucket]; ok {
		return c, nil
	}
	key, err := s.Keys.Get(bucket)
	if err != nil {
		if errors.Is(err, keys.ErrNotFound) {
			return nil, ErrNoSuchBucket
		}
		return nil, err
	}
	fs := s.FrameSize
	if fs == 0 {
		fs = crypto.DefaultFrameSize
	}
	c, err := crypto.NewCipher(key, fs)
	if err != nil {
		return nil, err
	}
	s.cipherCache[bucket] = c
	return c, nil
}

// openCiphertext returns a streaming reader over the encrypted blob for obj,
// going through the cache when configured.
func (s *Service) openCiphertext(ctx context.Context, obj *metadata.Object) (io.ReadCloser, error) {
	ref := storage.Ref{MessageID: obj.MessageID, FileID: obj.FileID}
	if s.Cache != nil {
		return s.Cache.OpenStreaming(ctx, ref, s.Backend)
	}
	return s.Backend.Get(ctx, ref)
}

// openCiphertextAt returns a ReaderAt over the encrypted blob, materialising
// it into the cache (or a tempfile if the cache is disabled).
func (s *Service) openCiphertextAt(ctx context.Context, obj *metadata.Object) (io.ReaderAt, io.Closer, error) {
	ref := storage.Ref{MessageID: obj.MessageID, FileID: obj.FileID}
	if s.Cache != nil {
		return s.Cache.OpenAt(ctx, ref, s.Backend)
	}
	// Fallback: download to a tempfile and read from it.
	if err := os.MkdirAll(s.StagingDir, 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp(s.StagingDir, "get-*.tmp")
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.Backend.Get(ctx, ref)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	if _, err := io.Copy(f, rc); err != nil {
		rc.Close()
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	rc.Close()
	return f, &tempFileCloser{f: f}, nil
}

type tempFileCloser struct{ f *os.File }

func (t *tempFileCloser) Close() error {
	name := t.f.Name()
	err := t.f.Close()
	_ = os.Remove(name)
	return err
}

// stageTemp buffers an unknown-length body to a temp file inside the staging
// directory, capping at maxSize bytes. An ENOSPC on the staging volume is
// translated to InsufficientStorage per §15.
func (s *Service) stageTemp(r io.Reader, maxSize int64) (*os.File, int64, error) {
	if err := os.MkdirAll(s.StagingDir, 0o700); err != nil {
		return nil, 0, err
	}
	f, err := os.CreateTemp(s.StagingDir, "put-*.tmp")
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return nil, 0, ErrInsufficientStorage
		}
		return nil, 0, err
	}
	limited := io.LimitReader(r, maxSize+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		os.Remove(f.Name())
		f.Close()
		if errors.Is(err, syscall.ENOSPC) {
			return nil, 0, ErrInsufficientStorage
		}
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

func cleanupTemp(f *os.File) {
	if f == nil {
		return
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(filepath.Clean(name))
}
