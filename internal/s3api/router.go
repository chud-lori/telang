package s3api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/telang/telang/internal/sigv4"
)

type ctxKey int

const requestIDKey ctxKey = 1

func requestIDFrom(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Handler is the top-level S3 HTTP handler. It runs sigv4 verification,
// dispatches by URL pattern, and translates well-known errors into the S3
// XML wire format.
type Handler struct {
	Verifier *sigv4.Verifier
	Service  *Service
	Logger   *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := newRequestID()
	ctx := context.WithValue(r.Context(), requestIDKey, id)
	r = r.WithContext(ctx)
	w.Header().Set("x-amz-request-id", id)
	w.Header().Set("Server", "telang")

	if err := h.Verifier.Verify(r); err != nil {
		h.logRequest(r, "auth_failed", "err", err)
		writeErr(w, r, sigv4ErrToS3(err))
		return
	}

	bucket, key := splitPath(r.URL.Path)

	switch {
	case bucket == "" && r.Method == http.MethodGet:
		h.listBuckets(w, r)
	case bucket != "" && key == "" && r.Method == http.MethodPut:
		h.createBucket(w, r, bucket)
	case bucket != "" && key == "" && r.Method == http.MethodDelete:
		h.deleteBucket(w, r, bucket)
	case bucket != "" && key == "" && r.Method == http.MethodGet:
		// ListObjectsV2 is v0.2; for v0.1 return NotImplemented.
		writeErr(w, r, ErrNotImplemented)
	case bucket != "" && key != "" && r.Method == http.MethodPut:
		h.putObject(w, r, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodGet:
		h.getObject(w, r, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodHead:
		h.headObject(w, r, bucket, key)
	case bucket != "" && key != "" && r.Method == http.MethodDelete:
		h.deleteObject(w, r, bucket, key)
	default:
		writeErr(w, r, ErrNotImplemented)
	}
}

func (h *Handler) logRequest(r *http.Request, msg string, kvs ...any) {
	if h.Logger == nil {
		return
	}
	args := append([]any{"method", r.Method, "path", r.URL.Path, "req_id", requestIDFrom(r)}, kvs...)
	h.Logger.Info(msg, args...)
}

// splitPath converts /a/b/c -> ("a", "b/c"), and "/a" or "/a/" -> ("a", "").
func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	slash := strings.IndexByte(p, '/')
	if slash < 0 {
		return p, ""
	}
	return p[:slash], p[slash+1:]
}

var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

func validBucketName(s string) bool {
	if !bucketRE.MatchString(s) {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// sigv4ErrToS3 maps verifier errors onto S3 error codes.
func sigv4ErrToS3(err error) *S3Error {
	switch {
	case errors.Is(err, sigv4.ErrMissingAuth), errors.Is(err, sigv4.ErrMalformedAuth):
		return ErrAccessDenied
	case errors.Is(err, sigv4.ErrUnknownAccessKey):
		return ErrInvalidAccessKeyID
	case errors.Is(err, sigv4.ErrSignatureMismatch):
		return ErrSignatureDoesNotMatch
	case errors.Is(err, sigv4.ErrExpired):
		return ErrRequestTimeTooSkewed
	case errors.Is(err, sigv4.ErrBadPayloadHash):
		return ErrXAmzContentSHA256Missing
	default:
		return ErrAccessDenied
	}
}
