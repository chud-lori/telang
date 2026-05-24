package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.CreateBucket(ctx, "alpha", "k1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateBucket(ctx, "alpha", "k1"); !errors.Is(err, ErrBucketExists) {
		t.Fatalf("duplicate create: want ErrBucketExists, got %v", err)
	}

	b, err := s.GetBucket(ctx, "alpha")
	if err != nil || b.Name != "alpha" || b.EncKeyID != "k1" {
		t.Fatalf("get: bucket=%+v err=%v", b, err)
	}

	list, err := s.ListBuckets(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	if err := s.DeleteBucket(ctx, "alpha"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetBucket(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
}

func TestObjectLifecycleAndOverwrite(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.CreateBucket(ctx, "b", "k1"); err != nil {
		t.Fatal(err)
	}

	o := &Object{
		Bucket: "b", Key: "k", Size: 10, CiphertextSize: 10,
		ETag: "e1", ContentType: "text/plain", MessageID: 1, FileID: "f1",
		Nonce: []byte{1, 2, 3},
	}
	if err := s.PutObject(ctx, o); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetObject(ctx, "b", "k")
	if err != nil || got.ETag != "e1" || got.MessageID != 1 {
		t.Fatalf("get: got=%+v err=%v", got, err)
	}

	// Bucket with rows cannot be deleted.
	if err := s.DeleteBucket(ctx, "b"); !errors.Is(err, ErrBucketNotEmpty) {
		t.Fatalf("delete non-empty: want ErrBucketNotEmpty, got %v", err)
	}

	// Overwrite same key.
	o2 := *o
	o2.Size = 20
	o2.ETag = "e2"
	o2.MessageID = 2
	o2.FileID = "f2"
	if err := s.PutObject(ctx, &o2); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetObject(ctx, "b", "k")
	if err != nil || got.ETag != "e2" || got.MessageID != 2 {
		t.Fatalf("overwrite: got=%+v err=%v", got, err)
	}

	prev, err := s.DeleteObject(ctx, "b", "k")
	if err != nil || prev.MessageID != 2 {
		t.Fatalf("delete object: prev=%+v err=%v", prev, err)
	}
	if _, err := s.GetObject(ctx, "b", "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
}
