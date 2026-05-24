package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/telang/telang/internal/metadata"
)

// writeMinimalConfig writes a TOML config pointing at an isolated db path so
// the export/import subcommands can run without touching real defaults.
func writeMinimalConfig(t *testing.T, dir, dbPath string) string {
	t.Helper()
	body := "[server]\nlisten = \":9000\"\n" +
		"[s3]\naccess_key = \"AKIA0\"\nsecret_key = \"S\"\nregion = \"tg-1\"\n" +
		"[telegram]\nmode = \"bot\"\nbot_token = \"X\"\nchannel_id = -100\n" +
		"[storage]\ndb_path = \"" + dbPath + "\"\ncache_dir = \"" + filepath.Join(dir, "cache") + "\"\nstaging_dir = \"" + filepath.Join(dir, "staging") + "\"\n" +
		"[encryption]\nkeys_file = \"" + filepath.Join(dir, "keys.toml") + "\"\n"
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "telang.db")

	// Populate a source DB.
	src, err := metadata.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.CreateBucket(context.Background(), "buk", "buk"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := src.PutObject(context.Background(), &metadata.Object{
			Bucket: "buk", Key: k, Size: 10, CiphertextSize: 27,
			ETag: "etag-" + k, MessageID: int64(1 + len(k)), FileID: "fid-" + k,
			Nonce: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			ContentType: "application/octet-stream",
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	src.Close()

	cfgPath := writeMinimalConfig(t, dir, dbPath)
	var exported bytes.Buffer
	if err := runExport([]string{"--config", cfgPath}, nil, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(exported.String(), `"kind":"bucket"`) ||
		!strings.Contains(exported.String(), `"kind":"object"`) {
		t.Fatalf("export missing expected lines:\n%s", exported.String())
	}

	// Wipe the DB and re-import.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport([]string{"--config", cfgPath}, bytes.NewReader(exported.Bytes()), &out); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out.String(), "buckets=1 objects=3") {
		t.Fatalf("import counter: %s", out.String())
	}

	// Re-open and verify the rows are back.
	restored, err := metadata.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, k := range []string{"a", "b", "c"} {
		o, err := restored.GetObject(context.Background(), "buk", k)
		if err != nil {
			t.Fatalf("missing object %s: %v", k, err)
		}
		if o.ETag != "etag-"+k || len(o.Nonce) != 12 {
			t.Fatalf("restored object mismatch: %+v", o)
		}
	}
}

func TestImportRefusesPopulatedDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "telang.db")
	src, _ := metadata.Open(context.Background(), dbPath)
	_ = src.CreateBucket(context.Background(), "buk", "buk")
	src.Close()

	cfgPath := writeMinimalConfig(t, dir, dbPath)
	err := runImport([]string{"--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected populated-DB rejection, got %v", err)
	}
}
