package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/telang/telang/internal/config"
	"github.com/telang/telang/internal/metadata"
)

// JSONL record. `Kind` discriminates the payload so import is order-independent
// (buckets land before objects either way thanks to FK-free schema).
type metaRecord struct {
	V    int             `json:"v"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

type headerData struct {
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note,omitempty"`
}

type bucketData struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	EncKeyID  string    `json:"enc_key_id"`
}

type objectData struct {
	Bucket         string    `json:"bucket"`
	Key            string    `json:"key"`
	Size           int64     `json:"size"`
	CiphertextSize int64     `json:"ciphertext_size"`
	ETag           string    `json:"etag"`
	ContentType    string    `json:"content_type,omitempty"`
	MessageID      int64     `json:"message_id"`
	FileID         string    `json:"file_id"`
	NonceB64       string    `json:"nonce_b64"`
	CreatedAt      time.Time `json:"created_at"`
}

type multipartData struct {
	UploadID   string         `json:"upload_id"`
	Bucket     string         `json:"bucket"`
	Key        string         `json:"key"`
	StagingDir string         `json:"staging_dir"`
	CreatedAt  time.Time      `json:"created_at"`
	Parts      []multipartPartData `json:"parts"`
}

type multipartPartData struct {
	PartNumber int    `json:"part_number"`
	Size       int64  `json:"size"`
	ETag       string `json:"etag"`
}

const exportVersion = 1

func runExport(args []string, _ io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("export-metadata", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/telang/config.toml", "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	meta, err := metadata.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	defer meta.Close()

	enc := json.NewEncoder(stdout)
	emit := func(kind string, payload any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return enc.Encode(metaRecord{V: exportVersion, Kind: kind, Data: raw})
	}

	if err := emit("header", headerData{CreatedAt: time.Now().UTC(), Note: "telang metadata export"}); err != nil {
		return err
	}
	if err := meta.ForEachBucket(ctx, func(b *metadata.Bucket) error {
		return emit("bucket", bucketData{Name: b.Name, CreatedAt: b.CreatedAt, EncKeyID: b.EncKeyID})
	}); err != nil {
		return err
	}
	if err := meta.ForEachObject(ctx, func(o *metadata.Object) error {
		return emit("object", objectData{
			Bucket: o.Bucket, Key: o.Key,
			Size: o.Size, CiphertextSize: o.CiphertextSize,
			ETag: o.ETag, ContentType: o.ContentType,
			MessageID: o.MessageID, FileID: o.FileID,
			NonceB64:  base64.StdEncoding.EncodeToString(o.Nonce),
			CreatedAt: o.CreatedAt,
		})
	}); err != nil {
		return err
	}
	if err := meta.ForEachMultipart(ctx, func(m *metadata.MultipartUpload, parts []metadata.MultipartPart) error {
		out := multipartData{
			UploadID: m.UploadID, Bucket: m.Bucket, Key: m.Key,
			StagingDir: m.StagingDir, CreatedAt: m.CreatedAt,
		}
		for _, p := range parts {
			out.Parts = append(out.Parts, multipartPartData{PartNumber: p.PartNumber, Size: p.Size, ETag: p.ETag})
		}
		return emit("multipart", out)
	}); err != nil {
		return err
	}
	return nil
}

func runImport(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("import-metadata", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/telang/config.toml", "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	meta, err := metadata.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	defer meta.Close()

	// Refuse to overwrite a populated DB so a misfired import can't corrupt
	// an in-use deployment.
	bs, _ := meta.ListBuckets(ctx)
	if len(bs) > 0 {
		return errors.New("import-metadata: destination DB is not empty; remove telang.db first")
	}

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var (
		header  bool
		buckets int
		objects int
		uploads int
	)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec metaRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("import: parse: %w", err)
		}
		if rec.V != exportVersion {
			return fmt.Errorf("import: unsupported export version %d", rec.V)
		}
		switch rec.Kind {
		case "header":
			header = true
		case "bucket":
			if !header {
				return errors.New("import: missing header")
			}
			var b bucketData
			if err := json.Unmarshal(rec.Data, &b); err != nil {
				return err
			}
			if err := meta.CreateBucket(ctx, b.Name, b.EncKeyID); err != nil {
				return fmt.Errorf("import bucket %q: %w", b.Name, err)
			}
			buckets++
		case "object":
			var o objectData
			if err := json.Unmarshal(rec.Data, &o); err != nil {
				return err
			}
			nonce, err := base64.StdEncoding.DecodeString(o.NonceB64)
			if err != nil {
				return fmt.Errorf("import object %s/%s: nonce: %w", o.Bucket, o.Key, err)
			}
			if err := meta.PutObject(ctx, &metadata.Object{
				Bucket: o.Bucket, Key: o.Key,
				Size: o.Size, CiphertextSize: o.CiphertextSize,
				ETag: o.ETag, ContentType: o.ContentType,
				MessageID: o.MessageID, FileID: o.FileID,
				Nonce:     nonce,
				CreatedAt: o.CreatedAt,
			}); err != nil {
				return fmt.Errorf("import object %s/%s: %w", o.Bucket, o.Key, err)
			}
			objects++
		case "multipart":
			var m multipartData
			if err := json.Unmarshal(rec.Data, &m); err != nil {
				return err
			}
			if err := meta.CreateMultipart(ctx, &metadata.MultipartUpload{
				UploadID: m.UploadID, Bucket: m.Bucket, Key: m.Key,
				StagingDir: m.StagingDir, CreatedAt: m.CreatedAt,
			}); err != nil {
				return fmt.Errorf("import multipart %s: %w", m.UploadID, err)
			}
			for _, p := range m.Parts {
				if err := meta.UpsertPart(ctx, &metadata.MultipartPart{
					UploadID: m.UploadID, PartNumber: p.PartNumber, Size: p.Size, ETag: p.ETag,
				}); err != nil {
					return err
				}
			}
			uploads++
		default:
			return fmt.Errorf("import: unknown kind %q", rec.Kind)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported buckets=%d objects=%d multipart_uploads=%d\n", buckets, objects, uploads)
	_ = os.Stdout
	return nil
}
