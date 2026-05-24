package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// --- buckets ---

type listBucketsResp struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Owner   owner    `xml:"Owner"`
	Buckets bucketList `xml:"Buckets"`
}
type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}
type bucketList struct {
	Bucket []bucketEntry `xml:"Bucket"`
}
type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request) {
	bs, err := h.Service.ListBuckets(r.Context())
	if err != nil {
		writeErr(w, r, ErrInternalError)
		return
	}
	resp := listBucketsResp{
		Owner: owner{ID: "telang", DisplayName: "telang"},
	}
	for _, b := range bs {
		resp.Buckets.Bucket = append(resp.Buckets.Bucket, bucketEntry{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeXML(w, http.StatusOK, &resp)
}

func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.Service.CreateBucket(r.Context(), bucket); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.Service.DeleteBucket(r.Context(), bucket); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- objects ---

func (h *Handler) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	size, sized, err := putBodySize(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}

	max := h.Service.Backend.MaxObjectSize()

	var (
		reader io.Reader = r.Body
		final  int64     = size
	)
	if !sized {
		// Stream-with-no-known-size: buffer to disk so the backend can be
		// handed a sized reader. Cap at MaxObjectSize.
		f, n, serr := h.Service.stageTemp(r.Body, max)
		if serr != nil {
			if errors.Is(serr, ErrEntityTooLarge) {
				writeErr(w, r, ErrEntityTooLarge)
				return
			}
			h.logRequest(r, "stage_temp_failed", "err", serr)
			writeErr(w, r, ErrInternalError)
			return
		}
		defer cleanupTemp(f)
		reader = f
		final = n
	} else if size > max {
		writeErr(w, r, ErrEntityTooLarge)
		return
	}

	obj, perr := h.Service.PutObject(r.Context(), bucket, key, r.Header.Get("Content-Type"), final, reader)
	if perr != nil {
		writeServiceErr(w, r, perr)
		return
	}
	w.Header().Set("ETag", quote(obj.ETag))
	w.WriteHeader(http.StatusOK)
}

// putBodySize returns the size declared by the client, plus a flag indicating
// whether the size was actually present. STREAMING bodies use
// `x-amz-decoded-content-length` to carry the decoded length; UNSIGNED bodies
// use plain `Content-Length`.
func putBodySize(r *http.Request) (int64, bool, *S3Error) {
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, false, ErrInvalidArgument
		}
		return n, true, nil
	}
	if r.ContentLength >= 0 {
		return r.ContentLength, true, nil
	}
	return 0, false, nil
}

func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if hdr := r.Header.Get("Range"); hdr != "" {
		h.getObjectRange(w, r, bucket, key, hdr)
		return
	}
	obj, body, err := h.Service.GetObject(r.Context(), bucket, key)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	defer body.Close()

	w.Header().Set("ETag", quote(obj.ETag))
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	w.Header().Set("Last-Modified", obj.CreatedAt.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		h.logRequest(r, "get_object_stream_failed", "err", err)
	}
}

func (h *Handler) getObjectRange(w http.ResponseWriter, r *http.Request, bucket, key, rangeHdr string) {
	// We need the size first to resolve a suffix range (e.g. bytes=-1024).
	obj, herr := h.Service.HeadObject(r.Context(), bucket, key)
	if herr != nil {
		writeServiceErr(w, r, herr)
		return
	}
	start, end, perr := parseRange(rangeHdr, obj.Size)
	if perr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", obj.Size))
		writeErr(w, r, ErrInvalidRange)
		return
	}
	_, body, err := h.Service.GetObjectRange(r.Context(), bucket, key, start, end)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	defer body.Close()

	w.Header().Set("ETag", quote(obj.ETag))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, obj.Size))
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	w.Header().Set("Last-Modified", obj.CreatedAt.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusPartialContent)
	if _, err := io.Copy(w, body); err != nil {
		h.logRequest(r, "get_object_range_stream_failed", "err", err)
	}
}

// parseRange parses a single-range "bytes=..." header against the object size.
// Multi-range requests are deliberately not supported.
func parseRange(s string, size int64) (start, end int64, err error) {
	const prefix = "bytes="
	if !strings.HasPrefix(s, prefix) {
		return 0, 0, errBadRange
	}
	spec := strings.TrimPrefix(s, prefix)
	if strings.Contains(spec, ",") {
		return 0, 0, errBadRange
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, errBadRange
	}
	loStr := spec[:dash]
	hiStr := spec[dash+1:]

	switch {
	case loStr == "" && hiStr == "":
		return 0, 0, errBadRange
	case loStr == "":
		// suffix range: bytes=-N → last N bytes
		n, perr := strconv.ParseInt(hiStr, 10, 64)
		if perr != nil || n <= 0 || size == 0 {
			return 0, 0, errBadRange
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, nil
	case hiStr == "":
		start, perr := strconv.ParseInt(loStr, 10, 64)
		if perr != nil || start < 0 || start >= size {
			return 0, 0, errBadRange
		}
		return start, size - 1, nil
	default:
		start, e1 := strconv.ParseInt(loStr, 10, 64)
		end, e2 := strconv.ParseInt(hiStr, 10, 64)
		if e1 != nil || e2 != nil || start < 0 || end < start || start >= size {
			return 0, 0, errBadRange
		}
		if end >= size {
			end = size - 1
		}
		return start, end, nil
	}
}

var errBadRange = fmt.Errorf("invalid range")

func (h *Handler) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	obj, err := h.Service.HeadObject(r.Context(), bucket, key)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	w.Header().Set("ETag", quote(obj.ETag))
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	w.Header().Set("Last-Modified", obj.CreatedAt.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if err := h.Service.DeleteObject(r.Context(), bucket, key); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var s3e *S3Error
	if errors.As(err, &s3e) {
		writeErr(w, r, s3e)
		return
	}
	writeErr(w, r, ErrInternalError)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	_ = enc.Encode(v)
}

func quote(s string) string {
	if strings.HasPrefix(s, `"`) {
		return s
	}
	return `"` + s + `"`
}
