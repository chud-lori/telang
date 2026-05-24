package storage

import (
	"context"
	"errors"
	"io"
)

// Ref identifies a stored blob inside Telegram.
//
//   - MessageID is the stable per-channel message identifier and is the
//     primary addressing token.
//   - FileID is a session-scoped fast-path cache; if it expires it can be
//     re-resolved from MessageID by the backend.
type Ref struct {
	MessageID int64
	FileID    string
}

type PutResult struct {
	Ref
	Size int64
}

var (
	ErrTooLarge = errors.New("storage: object exceeds backend maximum size")
	ErrNotFound = errors.New("storage: object not found")
	// ErrThrottled is reported when the backend (Telegram) returned a 429
	// FLOOD_WAIT after retries. Per §15 of telang.md this should surface as
	// 503 SlowDown to S3 clients.
	ErrThrottled = errors.New("storage: backend throttled")
	// ErrUnavailable is reported for transient transport failures (timeouts,
	// 5xx after retry exhaustion). Per §15 this should surface as 503
	// ServiceUnavailable.
	ErrUnavailable = errors.New("storage: backend unavailable")
)

// Backend is the abstraction over Telegram. The same interface is satisfied
// by the bot mode (HTTP Bot API) and mtproto mode adapters.
type Backend interface {
	// MaxObjectSize is the largest blob this backend will accept. The S3 layer
	// must reject objects above this size before calling Put.
	MaxObjectSize() int64

	// Put uploads exactly `size` bytes from r as a single Telegram message.
	// `name` is the on-Telegram filename (used for display only; addressing is
	// via MessageID/FileID).
	Put(ctx context.Context, name string, size int64, r io.Reader) (PutResult, error)

	// Get returns a reader over the full message body. Caller must Close.
	Get(ctx context.Context, ref Ref) (io.ReadCloser, error)

	// Delete removes the message from the channel. Best-effort: a missing
	// message is treated as success since the caller has already deleted the
	// metadata row.
	Delete(ctx context.Context, ref Ref) error

	// Exists reports whether the backend can still serve ref. Used by
	// `telang fsck` to detect metadata rows whose Telegram message has been
	// manually deleted out from under us.
	Exists(ctx context.Context, ref Ref) (bool, error)
}
