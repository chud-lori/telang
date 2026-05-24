package keys

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateGetRemoveAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.toml")

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	k, err := s.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != KeyLen {
		t.Fatalf("key length: %d", len(k))
	}
	if _, err := s.Create("alpha"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}

	// File must be chmod 600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm: %o", info.Mode().Perm())
	}

	// Reload and verify the same bytes are returned.
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(k) {
		t.Fatalf("key round-trip differs")
	}

	if err := s2.Remove("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get("alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after remove: %v", err)
	}
}

func TestRefusesLooseFilePerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.toml")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for chmod != 600")
	}
}
