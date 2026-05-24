package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound        = errors.New("metadata: not found")
	ErrBucketNotEmpty  = errors.New("metadata: bucket not empty")
	ErrBucketExists    = errors.New("metadata: bucket already exists")
	ErrUploadNotFound  = errors.New("metadata: multipart upload not found")
)

type Store struct {
	db *sql.DB
}

type Bucket struct {
	Name      string
	CreatedAt time.Time
	EncKeyID  string
}

type MultipartUpload struct {
	UploadID   string
	Bucket     string
	Key        string
	StagingDir string
	CreatedAt  time.Time
}

type MultipartPart struct {
	UploadID   string
	PartNumber int
	Size       int64
	ETag       string
}

type Object struct {
	Bucket          string
	Key             string
	Size            int64
	CiphertextSize  int64
	ETag            string
	ContentType     string
	MessageID       int64
	FileID          string
	Nonce           []byte
	CreatedAt       time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS buckets (
  name        TEXT PRIMARY KEY,
  created_at  INTEGER NOT NULL,
  enc_key_id  TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS objects (
  bucket           TEXT NOT NULL,
  key              TEXT NOT NULL,
  size             INTEGER NOT NULL,
  ciphertext_size  INTEGER NOT NULL,
  etag             TEXT NOT NULL,
  content_type     TEXT,
  message_id       INTEGER NOT NULL,
  file_id          TEXT NOT NULL,
  nonce            BLOB NOT NULL,
  created_at       INTEGER NOT NULL,
  PRIMARY KEY (bucket, key)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_objects_prefix ON objects (bucket, key);

CREATE TABLE IF NOT EXISTS multipart_uploads (
  upload_id   TEXT PRIMARY KEY,
  bucket      TEXT NOT NULL,
  key         TEXT NOT NULL,
  staging_dir TEXT NOT NULL,
  created_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS multipart_parts (
  upload_id   TEXT NOT NULL,
  part_number INTEGER NOT NULL,
  size        INTEGER NOT NULL,
  etag        TEXT NOT NULL,
  PRIMARY KEY (upload_id, part_number)
) STRICT;
`

func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", url.PathEscape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("metadata: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metadata: ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("metadata: schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- buckets ---

func (s *Store) CreateBucket(ctx context.Context, name, encKeyID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO buckets (name, created_at, enc_key_id) VALUES (?, ?, ?)`,
		name, time.Now().Unix(), encKeyID)
	if err != nil {
		if isUniqueErr(err) {
			return ErrBucketExists
		}
		return fmt.Errorf("metadata: create bucket: %w", err)
	}
	return nil
}

func (s *Store) GetBucket(ctx context.Context, name string) (*Bucket, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, created_at, enc_key_id FROM buckets WHERE name = ?`, name)
	var b Bucket
	var ts int64
	if err := row.Scan(&b.Name, &ts, &b.EncKeyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.CreatedAt = time.Unix(ts, 0).UTC()
	return &b, nil
}

func (s *Store) ListBuckets(ctx context.Context) ([]Bucket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, enc_key_id FROM buckets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		var ts int64
		if err := rows.Scan(&b.Name, &ts, &b.EncKeyID); err != nil {
			return nil, err
		}
		b.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM objects WHERE bucket = ?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrBucketNotEmpty
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM buckets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// --- objects ---

func (s *Store) PutObject(ctx context.Context, o *Object) error {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO objects
          (bucket, key, size, ciphertext_size, etag, content_type, message_id, file_id, nonce, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(bucket, key) DO UPDATE SET
          size            = excluded.size,
          ciphertext_size = excluded.ciphertext_size,
          etag            = excluded.etag,
          content_type    = excluded.content_type,
          message_id      = excluded.message_id,
          file_id         = excluded.file_id,
          nonce           = excluded.nonce,
          created_at      = excluded.created_at
    `,
		o.Bucket, o.Key, o.Size, o.CiphertextSize, o.ETag, nullableString(o.ContentType),
		o.MessageID, o.FileID, o.Nonce, o.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("metadata: put object: %w", err)
	}
	return nil
}

func (s *Store) GetObject(ctx context.Context, bucket, key string) (*Object, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT bucket, key, size, ciphertext_size, etag, content_type, message_id, file_id, nonce, created_at
        FROM objects WHERE bucket = ? AND key = ?`, bucket, key)
	var o Object
	var ts int64
	var ct sql.NullString
	if err := row.Scan(&o.Bucket, &o.Key, &o.Size, &o.CiphertextSize, &o.ETag, &ct,
		&o.MessageID, &o.FileID, &o.Nonce, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if ct.Valid {
		o.ContentType = ct.String
	}
	o.CreatedAt = time.Unix(ts, 0).UTC()
	return &o, nil
}

// ListObjects returns up to `limit` objects within bucket whose key starts
// with prefix, in lexical order, starting strictly after `startAfter`.
// `more` reports whether additional rows past the returned slice exist.
func (s *Store) ListObjects(ctx context.Context, bucket, prefix, startAfter string, limit int) (objs []Object, more bool, err error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT bucket, key, size, ciphertext_size, etag, content_type, message_id, file_id, nonce, created_at
        FROM objects
        WHERE bucket = ?
          AND key > ?
          AND substr(key, 1, ?) = ?
        ORDER BY key
        LIMIT ?`,
		bucket, startAfter, len(prefix), prefix, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var o Object
		var ts int64
		var ct sql.NullString
		if err := rows.Scan(&o.Bucket, &o.Key, &o.Size, &o.CiphertextSize, &o.ETag, &ct,
			&o.MessageID, &o.FileID, &o.Nonce, &ts); err != nil {
			return nil, false, err
		}
		if ct.Valid {
			o.ContentType = ct.String
		}
		o.CreatedAt = time.Unix(ts, 0).UTC()
		objs = append(objs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(objs) > limit {
		return objs[:limit], true, nil
	}
	return objs, false, nil
}

// DeleteObject removes the row and returns the previous object so the caller
// can also delete the underlying Telegram message. Returns ErrNotFound if the
// object did not exist.
func (s *Store) DeleteObject(ctx context.Context, bucket, key string) (*Object, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `
        SELECT bucket, key, size, ciphertext_size, etag, content_type, message_id, file_id, nonce, created_at
        FROM objects WHERE bucket = ? AND key = ?`, bucket, key)
	var o Object
	var ts int64
	var ct sql.NullString
	if err := row.Scan(&o.Bucket, &o.Key, &o.Size, &o.CiphertextSize, &o.ETag, &ct,
		&o.MessageID, &o.FileID, &o.Nonce, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if ct.Valid {
		o.ContentType = ct.String
	}
	o.CreatedAt = time.Unix(ts, 0).UTC()
	if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE bucket = ? AND key = ?`, bucket, key); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &o, nil
}

// --- multipart ---

func (s *Store) CreateMultipart(ctx context.Context, m *MultipartUpload) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO multipart_uploads (upload_id, bucket, key, staging_dir, created_at) VALUES (?, ?, ?, ?, ?)`,
		m.UploadID, m.Bucket, m.Key, m.StagingDir, m.CreatedAt.Unix())
	return err
}

func (s *Store) GetMultipart(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT upload_id, bucket, key, staging_dir, created_at FROM multipart_uploads WHERE upload_id = ?`, uploadID)
	var m MultipartUpload
	var ts int64
	if err := row.Scan(&m.UploadID, &m.Bucket, &m.Key, &m.StagingDir, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUploadNotFound
		}
		return nil, err
	}
	m.CreatedAt = time.Unix(ts, 0).UTC()
	return &m, nil
}

func (s *Store) DeleteMultipart(ctx context.Context, uploadID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM multipart_parts WHERE upload_id = ?`, uploadID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM multipart_uploads WHERE upload_id = ?`, uploadID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUploadNotFound
	}
	return tx.Commit()
}

// UpsertPart inserts or overwrites the record for (upload_id, part_number).
func (s *Store) UpsertPart(ctx context.Context, p *MultipartPart) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO multipart_parts (upload_id, part_number, size, etag) VALUES (?, ?, ?, ?)
        ON CONFLICT(upload_id, part_number) DO UPDATE SET size=excluded.size, etag=excluded.etag`,
		p.UploadID, p.PartNumber, p.Size, p.ETag)
	return err
}

func (s *Store) ListParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT upload_id, part_number, size, etag FROM multipart_parts WHERE upload_id = ? ORDER BY part_number`,
		uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MultipartPart
	for rows.Next() {
		var p MultipartPart
		if err := rows.Scan(&p.UploadID, &p.PartNumber, &p.Size, &p.ETag); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueErr(err error) bool {
	// modernc.org/sqlite surfaces unique constraint failures with this prefix.
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
