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
)

type Store struct {
	db *sql.DB
}

type Bucket struct {
	Name      string
	CreatedAt time.Time
	EncKeyID  string
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
