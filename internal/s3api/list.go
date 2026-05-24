package s3api

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxKeys = 1000
	hardMaxKeys    = 1000
)

type listObjectsV2Resp struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	IsTruncated           bool           `xml:"IsTruncated"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	Contents              []objectEntry  `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

func (h *Handler) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		writeErr(w, r, ErrNotImplemented)
		return
	}

	if _, err := h.Service.Meta.GetBucket(r.Context(), bucket); err != nil {
		writeErr(w, r, ErrNoSuchBucket)
		return
	}

	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	startAfter := q.Get("start-after")

	contToken := q.Get("continuation-token")
	if contToken != "" {
		dec, err := base64.RawURLEncoding.DecodeString(contToken)
		if err != nil {
			writeErr(w, r, ErrInvalidArgument)
			return
		}
		startAfter = string(dec)
	}

	maxKeys := defaultMaxKeys
	if mkStr := q.Get("max-keys"); mkStr != "" {
		mk, err := strconv.Atoi(mkStr)
		if err != nil || mk < 0 {
			writeErr(w, r, ErrInvalidArgument)
			return
		}
		if mk > hardMaxKeys {
			mk = hardMaxKeys
		}
		maxKeys = mk
	}

	resp := listObjectsV2Resp{
		Name:              bucket,
		Prefix:            prefix,
		Delimiter:         delimiter,
		MaxKeys:           maxKeys,
		ContinuationToken: contToken,
		StartAfter:        q.Get("start-after"),
	}
	if maxKeys == 0 {
		writeXML(w, http.StatusOK, &resp)
		return
	}

	cursor := startAfter
	seenPrefixes := map[string]struct{}{}
	emitted := 0
	var lastEmittedKey string
	truncated := false

loop:
	for {
		batch, more, err := h.Service.Meta.ListObjects(r.Context(), bucket, prefix, cursor, maxKeys-emitted)
		if err != nil {
			writeErr(w, r, ErrInternalError)
			return
		}
		if len(batch) == 0 {
			break
		}
		for _, o := range batch {
			cursor = o.Key
			if delimiter != "" {
				suffix := strings.TrimPrefix(o.Key, prefix)
				if idx := strings.Index(suffix, delimiter); idx >= 0 {
					cp := prefix + suffix[:idx+len(delimiter)]
					if _, ok := seenPrefixes[cp]; ok {
						continue
					}
					seenPrefixes[cp] = struct{}{}
					resp.CommonPrefixes = append(resp.CommonPrefixes, commonPrefix{Prefix: cp})
					emitted++
					lastEmittedKey = o.Key
					if emitted >= maxKeys {
						truncated = more || keyHasMoreAfter(h, r, bucket, prefix, cursor)
						break loop
					}
					continue
				}
			}
			resp.Contents = append(resp.Contents, objectEntry{
				Key:          o.Key,
				LastModified: o.CreatedAt.UTC().Format(time.RFC3339Nano),
				ETag:         `"` + o.ETag + `"`,
				Size:         o.Size,
				StorageClass: "STANDARD",
			})
			emitted++
			lastEmittedKey = o.Key
			if emitted >= maxKeys {
				truncated = more || keyHasMoreAfter(h, r, bucket, prefix, cursor)
				break loop
			}
		}
		if !more {
			break
		}
	}

	if truncated {
		resp.IsTruncated = true
		resp.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(lastEmittedKey))
	}
	resp.KeyCount = len(resp.Contents) + len(resp.CommonPrefixes)
	writeXML(w, http.StatusOK, &resp)
}

func keyHasMoreAfter(h *Handler, r *http.Request, bucket, prefix, after string) bool {
	rows, _, err := h.Service.Meta.ListObjects(r.Context(), bucket, prefix, after, 1)
	return err == nil && len(rows) > 0
}
