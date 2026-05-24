package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/telang/telang/internal/storage"
)

// Per §5.1, Bot API allows sendDocument up to 50 MB but getFile only up to
// 20 MB. Telang rejects > 20 MB at PutObject time to keep semantics symmetric.
const MaxObjectSize int64 = 20 * 1024 * 1024

const defaultEndpoint = "https://api.telegram.org"

type Backend struct {
	token     string
	chatID    int64
	endpoint  string
	http      *http.Client
	maxRetry  int
}

type Option func(*Backend)

func WithEndpoint(u string) Option   { return func(b *Backend) { b.endpoint = u } }
func WithHTTPClient(c *http.Client) Option { return func(b *Backend) { b.http = c } }
func WithMaxRetries(n int) Option    { return func(b *Backend) { b.maxRetry = n } }

func New(token string, chatID int64, opts ...Option) (*Backend, error) {
	if token == "" {
		return nil, errors.New("bot: empty token")
	}
	if chatID == 0 {
		return nil, errors.New("bot: chat_id is zero")
	}
	b := &Backend{
		token:    token,
		chatID:   chatID,
		endpoint: defaultEndpoint,
		http: &http.Client{
			Timeout: 0, // streaming uploads can exceed a single timeout; rely on ctx
		},
		maxRetry: 3,
	}
	for _, o := range opts {
		o(b)
	}
	return b, nil
}

func (b *Backend) MaxObjectSize() int64 { return MaxObjectSize }

// --- response envelopes ---

type apiResp[T any] struct {
	OK          bool          `json:"ok"`
	Result      T             `json:"result"`
	Description string        `json:"description,omitempty"`
	ErrorCode   int           `json:"error_code,omitempty"`
	Parameters  *apiRespParam `json:"parameters,omitempty"`
}

type apiRespParam struct {
	RetryAfter int `json:"retry_after,omitempty"`
}

type document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type message struct {
	MessageID int64     `json:"message_id"`
	Document  *document `json:"document,omitempty"`
}

type fileMeta struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// --- Backend impl ---

func (b *Backend) Put(ctx context.Context, name string, size int64, r io.Reader) (storage.PutResult, error) {
	if size > MaxObjectSize {
		return storage.PutResult{}, storage.ErrTooLarge
	}
	if name == "" {
		name = "blob"
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// Stream the multipart body so we don't buffer the whole object in RAM.
	errCh := make(chan error, 1)
	go func() {
		defer pw.Close()
		if err := mw.WriteField("chat_id", strconv.FormatInt(b.chatID, 10)); err != nil {
			errCh <- err
			pw.CloseWithError(err)
			return
		}
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="document"; filename=%q`, name))
		hdr.Set("Content-Type", "application/octet-stream")
		part, err := mw.CreatePart(hdr)
		if err != nil {
			errCh <- err
			pw.CloseWithError(err)
			return
		}
		if _, err := io.CopyN(part, r, size); err != nil {
			errCh <- err
			pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			errCh <- err
			pw.CloseWithError(err)
			return
		}
		errCh <- nil
	}()

	var msg message
	err := b.callWithRetry(ctx, "sendDocument", mw.FormDataContentType(), pr, &msg)
	// Make sure the producer goroutine has exited (it has, since the reader is closed
	// once the request body is fully consumed or the request errors).
	if perr := <-errCh; perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return storage.PutResult{}, err
	}
	if msg.Document == nil {
		return storage.PutResult{}, errors.New("bot: response missing document")
	}
	return storage.PutResult{
		Ref:  storage.Ref{MessageID: msg.MessageID, FileID: msg.Document.FileID},
		Size: msg.Document.FileSize,
	}, nil
}

func (b *Backend) Get(ctx context.Context, ref storage.Ref) (io.ReadCloser, error) {
	if ref.FileID == "" {
		return nil, errors.New("bot: empty file_id; re-resolution by message_id is not supported in v0.1")
	}
	// Resolve file_id -> file_path.
	form := url.Values{}
	form.Set("file_id", ref.FileID)
	var f fileMeta
	if err := b.callForm(ctx, "getFile", form, &f); err != nil {
		return nil, err
	}
	if f.FilePath == "" {
		return nil, errors.New("bot: getFile returned empty file_path")
	}

	dl := fmt.Sprintf("%s/file/bot%s/%s", b.endpoint, b.token, f.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, storage.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("bot: download status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

func (b *Backend) Delete(ctx context.Context, ref storage.Ref) error {
	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(b.chatID, 10))
	form.Set("message_id", strconv.FormatInt(ref.MessageID, 10))
	var ok bool
	err := b.callForm(ctx, "deleteMessage", form, &ok)
	if err != nil {
		// Telegram returns 400 "message to delete not found" when already gone.
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest &&
			strings.Contains(strings.ToLower(apiErr.Description), "not found") {
			return nil
		}
		return err
	}
	return nil
}

// --- HTTP plumbing ---

type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("bot %s: %d %s (retry_after=%ds)", e.Method, e.Code, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("bot %s: %d %s", e.Method, e.Code, e.Description)
}

func (b *Backend) callForm(ctx context.Context, method string, form url.Values, out any) error {
	body := strings.NewReader(form.Encode())
	return b.callWithRetry(ctx, method, "application/x-www-form-urlencoded", body, out)
}

func (b *Backend) callWithRetry(ctx context.Context, method, contentType string, body io.Reader, out any) error {
	// Only the first attempt may have a streaming body; retries are only useful
	// for idempotent methods or when the body fits in memory. We rely on the
	// fact that getFile/deleteMessage use url.Values bodies (cheap to rebuild)
	// and that sendDocument's pipe body cannot be retried — so for streaming
	// callers, maxRetry is effectively 1.
	canRetry := true
	if _, ok := body.(*io.PipeReader); ok {
		canRetry = false
	}

	var lastErr error
	for attempt := 0; attempt <= b.maxRetry; attempt++ {
		if attempt > 0 {
			if !canRetry {
				return lastErr
			}
			d := backoff(attempt)
			if apiErr, ok := lastErr.(*APIError); ok && apiErr.RetryAfter > 0 {
				d = time.Duration(apiErr.RetryAfter) * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		err := b.callOnce(ctx, method, contentType, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
	}
	return lastErr
}

func (b *Backend) callOnce(ctx context.Context, method, contentType string, body io.Reader, out any) error {
	url := fmt.Sprintf("%s/bot%s/%s", b.endpoint, b.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	// Decode into a generic envelope to read ok/description regardless of result type.
	var env struct {
		OK          bool             `json:"ok"`
		Description string           `json:"description,omitempty"`
		ErrorCode   int              `json:"error_code,omitempty"`
		Parameters  *apiRespParam    `json:"parameters,omitempty"`
		Result      json.RawMessage  `json:"result,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("bot %s: decode: %w (body=%q)", method, err, bytes.TrimSpace(raw))
	}
	if !env.OK {
		ae := &APIError{Method: method, Code: env.ErrorCode, Description: env.Description}
		if env.Parameters != nil {
			ae.RetryAfter = env.Parameters.RetryAfter
		}
		if ae.Code == 0 {
			ae.Code = resp.StatusCode
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("bot %s: decode result: %w", method, err)
	}
	return nil
}

func isRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		// 429 FLOOD_WAIT and 5xx are retryable.
		return ae.Code == 429 || (ae.Code >= 500 && ae.Code < 600)
	}
	// Network and timeout errors are retryable.
	return err != nil
}

func backoff(attempt int) time.Duration {
	// 1s, 2s, 4s, capped at 15s.
	d := time.Second << (attempt - 1)
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}
