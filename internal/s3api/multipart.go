package s3api

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/telang/telang/internal/crypto"
	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/storage"
)

// Per S3 spec; we enforce parts ≤ 10_000 to avoid pathological staging dirs.
const maxParts = 10_000

// CreateMultipartUpload: POST /{bucket}/{key}?uploads
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (h *Handler) createMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if _, err := h.Service.Meta.GetBucket(r.Context(), bucket); err != nil {
		writeErr(w, r, ErrNoSuchBucket)
		return
	}
	id, err := h.Service.CreateMultipart(r.Context(), bucket, key)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, &initiateMultipartUploadResult{
		Bucket: bucket, Key: key, UploadID: id,
	})
}

// UploadPart: PUT /{bucket}/{key}?partNumber=N&uploadId=...
func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	partStr := q.Get("partNumber")
	partNum, err := strconv.Atoi(partStr)
	if err != nil || partNum < 1 || partNum > maxParts {
		writeErr(w, r, ErrInvalidArgument)
		return
	}
	size, sized, perr := putBodySize(r)
	if perr != nil {
		writeErr(w, r, perr)
		return
	}

	var reader io.Reader = r.Body
	maxBytes := h.Service.Backend.MaxObjectSize()
	if !sized {
		f, n, serr := h.Service.stageTemp(r.Body, maxBytes)
		if serr != nil {
			if errors.Is(serr, ErrEntityTooLarge) {
				writeErr(w, r, ErrEntityTooLarge)
				return
			}
			writeErr(w, r, ErrInternalError)
			return
		}
		defer cleanupTemp(f)
		reader = f
		size = n
	}

	etag, err := h.Service.UploadPart(r.Context(), uploadID, partNum, size, reader)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	w.Header().Set("ETag", quote(etag))
	w.WriteHeader(http.StatusOK)
}

// CompleteMultipartUpload: POST /{bucket}/{key}?uploadId=...
type completeMultipartUploadReq struct {
	XMLName xml.Name             `xml:"CompleteMultipartUpload"`
	Parts   []completeRequestPart `xml:"Part"`
}

type completeRequestPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (h *Handler) completeMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, r, ErrMalformedXML)
		return
	}
	var req completeMultipartUploadReq
	if err := xml.Unmarshal(body, &req); err != nil {
		writeErr(w, r, ErrMalformedXML)
		return
	}
	for i := 1; i < len(req.Parts); i++ {
		if req.Parts[i].PartNumber <= req.Parts[i-1].PartNumber {
			writeErr(w, r, ErrInvalidPartOrder)
			return
		}
	}

	obj, err := h.Service.CompleteMultipart(r.Context(), bucket, key, uploadID, req.Parts, r.Header.Get("Content-Type"))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, &completeMultipartUploadResult{
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     quote(obj.ETag),
	})
}

// AbortMultipartUpload: DELETE /{bucket}/{key}?uploadId=...
func (h *Handler) abortMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if err := h.Service.AbortMultipart(r.Context(), uploadID); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- service-level multipart logic ---

func (s *Service) CreateMultipart(ctx context.Context, bucket, key string) (string, error) {
	if _, err := s.Meta.GetBucket(ctx, bucket); err != nil {
		if errors.Is(err, metadata.ErrNotFound) {
			return "", ErrNoSuchBucket
		}
		return "", err
	}
	id, err := newUploadID()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.StagingDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("staging mkdir: %w", err)
	}
	m := &metadata.MultipartUpload{
		UploadID: id, Bucket: bucket, Key: key, StagingDir: dir,
	}
	if err := s.Meta.CreateMultipart(ctx, m); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return id, nil
}

func (s *Service) UploadPart(ctx context.Context, uploadID string, partNum int, size int64, r io.Reader) (string, error) {
	m, err := s.Meta.GetMultipart(ctx, uploadID)
	if err != nil {
		if errors.Is(err, metadata.ErrUploadNotFound) {
			return "", ErrNoSuchUpload
		}
		return "", err
	}
	// Quick guard: the assembled object must fit the backend ceiling. We do
	// not know the final total here yet, so we just cap a single part at the
	// backend max — Complete will re-check the assembled total.
	if size > s.Backend.MaxObjectSize() {
		return "", ErrEntityTooLarge
	}

	path := filepath.Join(m.StagingDir, partFileName(partNum))
	// Atomic part write: tmp + rename so a half-uploaded part can't be observed.
	tmp, err := os.CreateTemp(m.StagingDir, "part-*.tmp")
	if err != nil {
		return "", err
	}
	rollback := func() {
		tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, size+1))
	if err != nil {
		rollback()
		return "", err
	}
	if n != size {
		rollback()
		return "", ErrInvalidArgument
	}
	if err := tmp.Sync(); err != nil {
		rollback()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}

	etag := hex.EncodeToString(h.Sum(nil))
	if err := s.Meta.UpsertPart(ctx, &metadata.MultipartPart{
		UploadID:   uploadID,
		PartNumber: partNum,
		Size:       size,
		ETag:       etag,
	}); err != nil {
		return "", err
	}
	return etag, nil
}

func (s *Service) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, reqParts []completeRequestPart, contentType string) (*metadata.Object, error) {
	m, err := s.Meta.GetMultipart(ctx, uploadID)
	if err != nil {
		if errors.Is(err, metadata.ErrUploadNotFound) {
			return nil, ErrNoSuchUpload
		}
		return nil, err
	}
	if m.Bucket != bucket || m.Key != key {
		return nil, ErrNoSuchUpload
	}
	rows, err := s.Meta.ListParts(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	have := make(map[int]metadata.MultipartPart, len(rows))
	for _, p := range rows {
		have[p.PartNumber] = p
	}

	var (
		totalSize  int64
		etagBytes  []byte
	)
	files := make([]*os.File, 0, len(reqParts))
	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}
	defer closeAll()

	for _, rp := range reqParts {
		got, ok := have[rp.PartNumber]
		if !ok {
			return nil, ErrInvalidPart
		}
		clientTag := strings.Trim(rp.ETag, `"`)
		if !strings.EqualFold(clientTag, got.ETag) {
			return nil, ErrInvalidPart
		}
		f, err := os.Open(filepath.Join(m.StagingDir, partFileName(rp.PartNumber)))
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		totalSize += got.Size
		raw, _ := hex.DecodeString(got.ETag)
		etagBytes = append(etagBytes, raw...)
	}

	c, err := s.cipherFor(bucket)
	if err != nil {
		return nil, err
	}
	ciphertextSize := c.CiphertextSize(totalSize)
	if ciphertextSize > s.Backend.MaxObjectSize() {
		return nil, ErrEntityTooLarge
	}

	base := make([]byte, crypto.NonceLen)
	if _, err := rand.Read(base); err != nil {
		return nil, err
	}
	plainReader := newMultiReader(files)
	encReader := c.NewEncrypterWithNonce(plainReader, totalSize, base)

	put, err := s.Backend.Put(ctx, key, ciphertextSize, encReader)
	if err != nil {
		if errors.Is(err, storage.ErrTooLarge) {
			return nil, ErrEntityTooLarge
		}
		return nil, fmt.Errorf("backend put: %w", err)
	}

	combined := md5.Sum(etagBytes)
	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(combined[:]), len(reqParts))

	obj := &metadata.Object{
		Bucket:         bucket,
		Key:            key,
		Size:           totalSize,
		CiphertextSize: ciphertextSize,
		ETag:           etag,
		ContentType:    contentType,
		MessageID:      put.MessageID,
		FileID:         put.FileID,
		Nonce:          base,
	}
	if err := s.Meta.PutObject(ctx, obj); err != nil {
		_ = s.Backend.Delete(ctx, put.Ref)
		return nil, err
	}
	closeAll()
	_ = os.RemoveAll(m.StagingDir)
	_ = s.Meta.DeleteMultipart(ctx, uploadID)
	return obj, nil
}

func (s *Service) AbortMultipart(ctx context.Context, uploadID string) error {
	m, err := s.Meta.GetMultipart(ctx, uploadID)
	if err != nil {
		if errors.Is(err, metadata.ErrUploadNotFound) {
			return ErrNoSuchUpload
		}
		return err
	}
	_ = os.RemoveAll(m.StagingDir)
	return s.Meta.DeleteMultipart(ctx, uploadID)
}

// --- internal helpers ---

func partFileName(n int) string { return fmt.Sprintf("%010d.part", n) }

func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// newMultiReader sequentially reads a slice of *os.File. It is preferable to
// io.MultiReader of file readers because it lets us keep ownership of the
// file handles for explicit close, which matters when Complete is interrupted
// mid-stream and we must still clean up.
func newMultiReader(files []*os.File) io.Reader {
	readers := make([]io.Reader, len(files))
	for i, f := range files {
		readers[i] = f
	}
	return io.MultiReader(readers...)
}

// SortPartsByNumber is a stable sort helper for the metadata-side parts list.
func SortPartsByNumber(p []metadata.MultipartPart) {
	sort.Slice(p, func(i, j int) bool { return p[i].PartNumber < p[j].PartNumber })
}
