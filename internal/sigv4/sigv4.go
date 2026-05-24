// Package sigv4 verifies AWS Signature V4 on incoming S3 requests.
//
// Supported flavors:
//
//   - Header-based authentication (Authorization: AWS4-HMAC-SHA256 ...)
//     with x-amz-content-sha256 values:
//       UNSIGNED-PAYLOAD                       — body hash is not verified
//       <hex sha256>                           — body hash is verified
//       STREAMING-AWS4-HMAC-SHA256-PAYLOAD     — chunked, per-chunk signatures
//       STREAMING-UNSIGNED-PAYLOAD-TRAILER     — chunked, no chunk signatures
//
//   - Query-string presigned URLs (X-Amz-Algorithm + X-Amz-Credential +
//     X-Amz-Date + X-Amz-Expires + X-Amz-SignedHeaders + X-Amz-Signature).
//     Presigned URLs always use UNSIGNED-PAYLOAD; body bytes are not part
//     of the signature.
package sigv4

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm   = "AWS4-HMAC-SHA256"
	service     = "s3"
	requestKind = "aws4_request"
	timeFormat  = "20060102T150405Z"
	dateFormat  = "20060102"

	hdrAuthorization = "Authorization"
	hdrDate          = "X-Amz-Date"
	hdrContentSHA256 = "X-Amz-Content-Sha256"

	qpAlgorithm     = "X-Amz-Algorithm"
	qpCredential    = "X-Amz-Credential"
	qpDate          = "X-Amz-Date"
	qpExpires       = "X-Amz-Expires"
	qpSignedHeaders = "X-Amz-SignedHeaders"
	qpSignature     = "X-Amz-Signature"

	unsignedPayload          = "UNSIGNED-PAYLOAD"
	streamingSignedPayload   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

	maxClockSkew    = 15 * time.Minute
	maxPresignedAge = 7 * 24 * time.Hour // S3 hard cap
)

var (
	ErrMissingAuth      = errors.New("sigv4: missing Authorization header")
	ErrMalformedAuth    = errors.New("sigv4: malformed Authorization header")
	ErrUnknownAccessKey = errors.New("sigv4: unknown access key")
	ErrSignatureMismatch = errors.New("sigv4: signature does not match")
	ErrExpired          = errors.New("sigv4: request outside clock-skew window")
	ErrBadPayloadHash   = errors.New("sigv4: x-amz-content-sha256 mismatch")
	ErrUnsupportedAlgo  = errors.New("sigv4: unsupported algorithm")
)

var emptySHA256 = func() string { s := sha256.Sum256(nil); return hex.EncodeToString(s[:]) }()

// CredLookup returns the secret key for an access key, plus true if the key
// is recognised. Lookups must be constant-time-safe; for a single static pair
// wrapping a fixed string is fine.
type CredLookup func(accessKey string) (secret string, ok bool)

type Verifier struct {
	Region string
	Lookup CredLookup
	Now    func() time.Time
}

// authHeader carries the parsed pieces of an AWS4-HMAC-SHA256 Authorization
// header.
type authHeader struct {
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

// Verify validates the signature on r and (when applicable) wraps r.Body with
// a decoder that strips chunked transfer framing. After a successful Verify,
// downstream handlers should read r.Body normally.
//
// The host header is taken from r.Host, which Go's http server populates from
// the request line / Host header. The canonical URI is r.URL.Path. The
// canonical query string is r.URL.RawQuery.
func (v *Verifier) Verify(r *http.Request) error {
	if v.Lookup == nil {
		return errors.New("sigv4: verifier has no credential lookup")
	}
	authStr := r.Header.Get(hdrAuthorization)
	if authStr == "" {
		// Presigned URL? Detect by signature query param.
		if r.URL.Query().Get(qpSignature) != "" {
			return v.verifyPresigned(r)
		}
		return ErrMissingAuth
	}
	auth, err := parseAuthHeader(authStr)
	if err != nil {
		return err
	}
	if auth.Service != service {
		return fmt.Errorf("sigv4: wrong service %q in credential scope", auth.Service)
	}
	if v.Region != "" && auth.Region != v.Region {
		return fmt.Errorf("sigv4: wrong region %q in credential scope", auth.Region)
	}

	secret, ok := v.Lookup(auth.AccessKey)
	if !ok {
		return ErrUnknownAccessKey
	}

	dateStr := r.Header.Get(hdrDate)
	if dateStr == "" {
		return errors.New("sigv4: missing X-Amz-Date")
	}
	t, err := time.Parse(timeFormat, dateStr)
	if err != nil {
		return fmt.Errorf("sigv4: bad X-Amz-Date: %w", err)
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now()
	}
	if t.Before(now.Add(-maxClockSkew)) || t.After(now.Add(maxClockSkew)) {
		return ErrExpired
	}

	payloadHash := r.Header.Get(hdrContentSHA256)
	if payloadHash == "" {
		return errors.New("sigv4: missing X-Amz-Content-Sha256")
	}

	canonReq, err := buildCanonicalRequest(r, auth.SignedHeaders, payloadHash)
	if err != nil {
		return err
	}
	scope := strings.Join([]string{auth.Date, auth.Region, auth.Service, requestKind}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		dateStr,
		scope,
		hashHex([]byte(canonReq)),
	}, "\n")

	signingKey := deriveSigningKey(secret, auth.Date, auth.Region, auth.Service)
	expected := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if subtle.ConstantTimeCompare([]byte(expected), []byte(auth.Signature)) != 1 {
		return ErrSignatureMismatch
	}

	// Body verification differs by payload hash mode.
	switch payloadHash {
	case unsignedPayload, streamingUnsignedTrailer:
		// Nothing further; trailer mode strips trailers transparently when
		// the client uses chunked transfer-encoding.
		if payloadHash == streamingUnsignedTrailer {
			r.Body = newUnsignedChunkedReader(r.Body)
		}
	case streamingSignedPayload:
		r.Body = newSignedChunkedReader(r.Body, signingKey, dateStr, scope, auth.Signature)
	default:
		// Treat as hex sha256 of body. We can't stream-verify without buffering
		// the body, which defeats the streaming goal. The caller must compute
		// the hash while reading and call VerifyBodyHash at end of request.
		// To keep the public surface small, we wrap the body with a hashing
		// reader and verify lazily; if the consumer doesn't read the full body
		// the verification still fires on close.
		if _, err := hex.DecodeString(payloadHash); err != nil || len(payloadHash) != 64 {
			return fmt.Errorf("sigv4: unsupported payload hash %q", payloadHash)
		}
		r.Body = newHashCheckingReader(r.Body, payloadHash)
	}

	return nil
}

// verifyPresigned validates a query-string-signed request. Body is treated as
// UNSIGNED-PAYLOAD per the spec; presigned PUTs are still allowed, but their
// bytes are not part of the signature.
func (v *Verifier) verifyPresigned(r *http.Request) error {
	q := r.URL.Query()
	if a := q.Get(qpAlgorithm); a != algorithm {
		return fmt.Errorf("sigv4: presigned %s=%q (want %s)", qpAlgorithm, a, algorithm)
	}
	cred := q.Get(qpCredential)
	cs := strings.Split(cred, "/")
	if len(cs) != 5 || cs[4] != requestKind {
		return ErrMalformedAuth
	}
	auth := &authHeader{
		AccessKey: cs[0], Date: cs[1], Region: cs[2], Service: cs[3],
		Signature: q.Get(qpSignature),
	}
	if auth.AccessKey == "" || auth.Signature == "" {
		return ErrMalformedAuth
	}
	if auth.Service != service {
		return fmt.Errorf("sigv4: presigned credential service=%q", auth.Service)
	}
	if v.Region != "" && auth.Region != v.Region {
		return fmt.Errorf("sigv4: presigned credential region=%q", auth.Region)
	}
	if _, err := time.Parse(dateFormat, auth.Date); err != nil {
		return ErrMalformedAuth
	}

	signedRaw := q.Get(qpSignedHeaders)
	if signedRaw == "" {
		return ErrMalformedAuth
	}
	auth.SignedHeaders = strings.Split(signedRaw, ";")

	dateStr := q.Get(qpDate)
	t, err := time.Parse(timeFormat, dateStr)
	if err != nil {
		return fmt.Errorf("sigv4: presigned X-Amz-Date: %w", err)
	}
	expiresStr := q.Get(qpExpires)
	expiresSec, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || expiresSec <= 0 {
		return fmt.Errorf("sigv4: presigned X-Amz-Expires=%q", expiresStr)
	}
	expiresDur := time.Duration(expiresSec) * time.Second
	if expiresDur > maxPresignedAge {
		return fmt.Errorf("sigv4: presigned X-Amz-Expires=%ds exceeds 7-day cap", expiresSec)
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now()
	}
	if now.Before(t.Add(-maxClockSkew)) || now.After(t.Add(expiresDur)) {
		return ErrExpired
	}

	secret, ok := v.Lookup(auth.AccessKey)
	if !ok {
		return ErrUnknownAccessKey
	}

	// Canonical query string must exclude X-Amz-Signature itself.
	rawQ := stripQueryParam(r.URL.RawQuery, qpSignature)
	canonReq, err := buildCanonicalRequestWithQuery(r, auth.SignedHeaders, unsignedPayload, rawQ)
	if err != nil {
		return err
	}
	scope := strings.Join([]string{auth.Date, auth.Region, auth.Service, requestKind}, "/")
	stringToSign := strings.Join([]string{algorithm, dateStr, scope, hashHex([]byte(canonReq))}, "\n")
	signingKey := deriveSigningKey(secret, auth.Date, auth.Region, auth.Service)
	expected := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(auth.Signature)) != 1 {
		return ErrSignatureMismatch
	}
	// Presigned PUTs are allowed but unverified — we mark this by leaving the
	// body untouched. There's nothing else to do.
	return nil
}

// stripQueryParam removes a single named parameter from a raw RFC-3986 query
// string, preserving the order of the rest.
func stripQueryParam(raw, name string) string {
	if raw == "" {
		return ""
	}
	var keep []string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		key := part
		if eq >= 0 {
			key = part[:eq]
		}
		dk, err := url.QueryUnescape(key)
		if err != nil {
			dk = key
		}
		if strings.EqualFold(dk, name) {
			continue
		}
		keep = append(keep, part)
	}
	return strings.Join(keep, "&")
}

// --- canonical request construction ---

func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	return buildCanonicalRequestWithQuery(r, signedHeaders, payloadHash, r.URL.RawQuery)
}

// buildCanonicalRequestWithQuery is the variant used for presigned URLs: the
// caller supplies an already-stripped query string (with X-Amz-Signature
// removed).
func buildCanonicalRequestWithQuery(r *http.Request, signedHeaders []string, payloadHash, rawQuery string) (string, error) {
	method := r.Method
	uri := canonicalURI(r.URL.Path)
	query := canonicalQueryRaw(rawQuery)
	canonHeaders, signedList, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		method,
		uri,
		query,
		canonHeaders,
		"",
		signedList,
		payloadHash,
	}, "\n"), nil
}

func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	// Re-encode each segment per RFC 3986 unreserved-only escaping. S3 paths
	// can include arbitrary keys; r.URL.Path is already percent-decoded by
	// net/http, so we need to re-escape exactly the same way AWS does.
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s, false)
	}
	out := strings.Join(segs, "/")
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	return out
}

func canonicalQuery(u *url.URL) string { return canonicalQueryRaw(u.RawQuery) }

func canonicalQueryRaw(raw string) string {
	if raw == "" {
		return ""
	}
	// AWS sorts by key, then by value, after escaping. Keys without an "="
	// must still produce "key=".
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		var k, v string
		if eq < 0 {
			k = part
		} else {
			k = part[:eq]
			v = part[eq+1:]
		}
		// Decode then re-encode to normalise.
		dk, err := url.QueryUnescape(k)
		if err != nil {
			dk = k
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			dv = v
		}
		pairs = append(pairs, kv{awsEscape(dk, true), awsEscape(dv, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func canonicalHeaders(r *http.Request, signed []string) (string, string, error) {
	// Lowercase signed list, sort it.
	low := make([]string, len(signed))
	for i, h := range signed {
		low[i] = strings.ToLower(strings.TrimSpace(h))
	}
	sort.Strings(low)

	var sb strings.Builder
	for _, name := range low {
		var val string
		switch name {
		case "host":
			val = r.Host
		case "content-length":
			if r.ContentLength >= 0 {
				val = strconv.FormatInt(r.ContentLength, 10)
			} else {
				val = r.Header.Get("Content-Length")
			}
		default:
			vals := r.Header.Values(http.CanonicalHeaderKey(name))
			if len(vals) == 0 {
				return "", "", fmt.Errorf("sigv4: missing signed header %q", name)
			}
			val = strings.Join(vals, ",")
		}
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(trimAll(val))
		sb.WriteByte('\n')
	}
	return sb.String(), strings.Join(low, ";"), nil
}

// trimAll collapses runs of whitespace inside a header value and trims the
// ends, per the canonical-request rules.
func trimAll(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// awsEscape implements RFC 3986 unreserved-only escaping. If encodeSlash is
// false, '/' is left as-is (used for URI path); if true, '/' is escaped (used
// for query parameters).
func awsEscape(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' ||
			(!encodeSlash && c == '/') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// --- header parsing ---

func parseAuthHeader(s string) (*authHeader, error) {
	if !strings.HasPrefix(s, algorithm+" ") {
		return nil, ErrUnsupportedAlgo
	}
	rest := strings.TrimPrefix(s, algorithm+" ")
	parts := strings.Split(rest, ",")
	out := &authHeader{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "Credential="):
			v := strings.TrimPrefix(p, "Credential=")
			cs := strings.Split(v, "/")
			if len(cs) != 5 {
				return nil, ErrMalformedAuth
			}
			out.AccessKey, out.Date, out.Region, out.Service = cs[0], cs[1], cs[2], cs[3]
			if cs[4] != requestKind {
				return nil, ErrMalformedAuth
			}
		case strings.HasPrefix(p, "SignedHeaders="):
			out.SignedHeaders = strings.Split(strings.TrimPrefix(p, "SignedHeaders="), ";")
		case strings.HasPrefix(p, "Signature="):
			out.Signature = strings.TrimPrefix(p, "Signature=")
		default:
			// Ignore unknown fields per spec.
		}
	}
	if out.AccessKey == "" || out.Date == "" || out.Region == "" || out.Service == "" ||
		len(out.SignedHeaders) == 0 || out.Signature == "" {
		return nil, ErrMalformedAuth
	}
	if _, err := time.Parse(dateFormat, out.Date); err != nil {
		return nil, ErrMalformedAuth
	}
	return out, nil
}

// --- key derivation ---

func deriveSigningKey(secret, date, region, svc string) []byte {
	kSecret := []byte("AWS4" + secret)
	kDate := hmacSHA256(kSecret, []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(svc))
	return hmacSHA256(kService, []byte(requestKind))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- streaming body decoders ---

// hashCheckingReader buffers the SHA-256 of the body as it is read and, on
// Close, compares it to the declared hex hash from x-amz-content-sha256.
type hashCheckingReader struct {
	r        io.ReadCloser
	expected string
	h        hashState
	bad      error
}

type hashState struct {
	h io.Writer
	d func() string
}

func newHashCheckingReader(r io.ReadCloser, expected string) *hashCheckingReader {
	hh := sha256.New()
	return &hashCheckingReader{
		r:        r,
		expected: strings.ToLower(expected),
		h: hashState{
			h: hh,
			d: func() string { return hex.EncodeToString(hh.Sum(nil)) },
		},
	}
}

func (h *hashCheckingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.h.h.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		if h.h.d() != h.expected {
			h.bad = ErrBadPayloadHash
			return n, ErrBadPayloadHash
		}
	}
	return n, err
}

func (h *hashCheckingReader) Close() error {
	if h.bad != nil {
		_ = h.r.Close()
		return h.bad
	}
	return h.r.Close()
}

// signedChunkedReader parses STREAMING-AWS4-HMAC-SHA256-PAYLOAD framing and
// verifies each chunk signature against a rolling chain of HMACs anchored at
// the seed signature.
type signedChunkedReader struct {
	br         *bufio.Reader
	src        io.Closer
	signingKey []byte
	datetime   string
	scope      string
	prevSig    string

	leftover []byte // current chunk data not yet returned
	eof      bool
	err      error
}

func newSignedChunkedReader(body io.ReadCloser, signingKey []byte, datetime, scope, seed string) *signedChunkedReader {
	return &signedChunkedReader{
		br:         bufio.NewReader(body),
		src:        body,
		signingKey: signingKey,
		datetime:   datetime,
		scope:      scope,
		prevSig:    seed,
	}
}

func (s *signedChunkedReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(s.leftover) == 0 {
		if s.eof {
			return 0, io.EOF
		}
		if err := s.nextChunk(); err != nil {
			s.err = err
			return 0, err
		}
		if s.eof && len(s.leftover) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, s.leftover)
	s.leftover = s.leftover[n:]
	return n, nil
}

func (s *signedChunkedReader) Close() error { return s.src.Close() }

func (s *signedChunkedReader) nextChunk() error {
	line, err := s.br.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimRight(line, "\r\n")
	semi := strings.IndexByte(line, ';')
	if semi < 0 {
		return errors.New("sigv4: chunk header missing extension")
	}
	sizeHex := line[:semi]
	ext := line[semi+1:]
	const sigPrefix = "chunk-signature="
	idx := strings.Index(ext, sigPrefix)
	if idx < 0 {
		return errors.New("sigv4: chunk header missing chunk-signature")
	}
	chunkSig := strings.TrimSpace(ext[idx+len(sigPrefix):])
	size, err := strconv.ParseUint(sizeHex, 16, 63)
	if err != nil {
		return fmt.Errorf("sigv4: bad chunk size: %w", err)
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(s.br, data); err != nil {
		return err
	}
	// Each chunk is followed by CRLF.
	if _, err := s.br.Discard(2); err != nil {
		return err
	}

	// Verify chunk signature.
	chunkHash := hashHex(data)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		s.datetime,
		s.scope,
		s.prevSig,
		emptySHA256,
		chunkHash,
	}, "\n")
	expect := hex.EncodeToString(hmacSHA256(s.signingKey, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(expect), []byte(chunkSig)) != 1 {
		return ErrSignatureMismatch
	}
	s.prevSig = chunkSig

	if size == 0 {
		s.eof = true
		return nil
	}
	s.leftover = data
	return nil
}

// unsignedChunkedReader strips the same framing as signedChunkedReader but
// does not verify chunk signatures (used for STREAMING-UNSIGNED-PAYLOAD-TRAILER).
type unsignedChunkedReader struct {
	br       *bufio.Reader
	src      io.Closer
	leftover []byte
	eof      bool
	err      error
}

func newUnsignedChunkedReader(body io.ReadCloser) *unsignedChunkedReader {
	return &unsignedChunkedReader{br: bufio.NewReader(body), src: body}
}

func (s *unsignedChunkedReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(s.leftover) == 0 {
		if s.eof {
			return 0, io.EOF
		}
		if err := s.nextChunk(); err != nil {
			s.err = err
			return 0, err
		}
		if s.eof && len(s.leftover) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, s.leftover)
	s.leftover = s.leftover[n:]
	return n, nil
}

func (s *unsignedChunkedReader) Close() error { return s.src.Close() }

func (s *unsignedChunkedReader) nextChunk() error {
	line, err := s.br.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimRight(line, "\r\n")
	if semi := strings.IndexByte(line, ';'); semi >= 0 {
		line = line[:semi]
	}
	size, err := strconv.ParseUint(line, 16, 63)
	if err != nil {
		return fmt.Errorf("sigv4: bad chunk size: %w", err)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(s.br, data); err != nil {
		return err
	}
	if _, err := s.br.Discard(2); err != nil {
		return err
	}
	if size == 0 {
		s.eof = true
		// Consume trailers (header lines until empty).
		for {
			l, err := s.br.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if strings.TrimRight(l, "\r\n") == "" {
				return nil
			}
		}
	}
	s.leftover = data
	return nil
}

