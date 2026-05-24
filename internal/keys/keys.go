// Package keys manages the per-bucket AES key store backed by keys.toml.
//
// keys.toml is a flat map of bucket-name -> base64(32-byte key). Telang
// enforces chmod 600 on read so a misconfigured deployment fails loud.
package keys

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/BurntSushi/toml"
)

const KeyLen = 32 // AES-256

var (
	ErrNotFound = errors.New("keys: no key for bucket")
	ErrExists   = errors.New("keys: key already exists for bucket")
)

type Store struct {
	mu   sync.RWMutex
	path string
	keys map[string][]byte
}

// Load opens (or creates) the keys file at path. The file must be chmod 600
// when it already exists; a freshly-created file is written with 0600.
func Load(path string) (*Store, error) {
	s := &Store{path: path, keys: map[string][]byte{}}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Brand-new install; create an empty file with strict perms.
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("keys: mkdir: %w", err)
		}
		if err := writeFileAtomic(path, []byte{}, 0o600); err != nil {
			return nil, fmt.Errorf("keys: create: %w", err)
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("keys: stat: %w", err)
	default:
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("keys: %s must be chmod 600 (got %o)", path, info.Mode().Perm())
		}
	}

	raw := map[string]string{}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("keys: decode: %w", err)
	}
	for bucket, b64 := range raw {
		k, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("keys: bucket %q: %w", bucket, err)
		}
		if len(k) != KeyLen {
			return nil, fmt.Errorf("keys: bucket %q: want %d-byte key, got %d", bucket, KeyLen, len(k))
		}
		s.keys[bucket] = k
	}
	return s, nil
}

func (s *Store) Get(bucket string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[bucket]
	if !ok {
		return nil, ErrNotFound
	}
	// Return a defensive copy so callers can't mutate the in-memory key.
	out := make([]byte, len(k))
	copy(out, k)
	return out, nil
}

// Create generates a fresh 32-byte key for bucket and persists keys.toml
// atomically with mode 0600. Errors if the bucket already has a key.
func (s *Store) Create(bucket string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[bucket]; ok {
		return nil, ErrExists
	}
	k := make([]byte, KeyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	s.keys[bucket] = k
	if err := s.flushLocked(); err != nil {
		delete(s.keys, bucket)
		return nil, err
	}
	out := make([]byte, KeyLen)
	copy(out, k)
	return out, nil
}

// Remove deletes the key for bucket and persists keys.toml atomically.
func (s *Store) Remove(bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[bucket]; !ok {
		return ErrNotFound
	}
	saved := s.keys[bucket]
	delete(s.keys, bucket)
	if err := s.flushLocked(); err != nil {
		s.keys[bucket] = saved
		return err
	}
	return nil
}

func (s *Store) flushLocked() error {
	// Deterministic ordering for diff-friendliness.
	names := make([]string, 0, len(s.keys))
	for n := range s.keys {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf []byte
	buf = append(buf, "# Telang per-bucket encryption keys. Back this up out of band.\n"...)
	buf = append(buf, "# Losing this file = losing the data.\n"...)
	for _, n := range names {
		line := fmt.Sprintf("%q = %q\n", n, base64.StdEncoding.EncodeToString(s.keys[n]))
		buf = append(buf, line...)
	}
	return writeFileAtomic(s.path, buf, 0o600)
}

// writeFileAtomic writes data to path via a tempfile + rename to avoid leaving
// a half-written keys file on disk.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "keys-*.tmp")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return err
	}
	return nil
}
