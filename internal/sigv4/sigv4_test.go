package sigv4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testAK = "AKIDEXAMPLE"
	testSK = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	testRG = "us-east-1"
)

func staticLookup(ak string) (string, bool) {
	if ak == testAK {
		return testSK, true
	}
	return "", false
}

func fixedNow() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }

// signRequest signs an http.Request the same way the verifier expects. It is
// intentionally a separate implementation from the verifier so round-trip
// tests catch internal bugs.
func signRequest(t *testing.T, r *http.Request, payloadHash string, now time.Time) {
	t.Helper()
	dateStr := now.UTC().Format(timeFormat)
	date := dateStr[:8]
	r.Header.Set(hdrDate, dateStr)
	r.Header.Set(hdrContentSHA256, payloadHash)

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for h := range r.Header {
		lh := strings.ToLower(h)
		if strings.HasPrefix(lh, "x-amz-") && lh != "x-amz-date" && lh != "x-amz-content-sha256" {
			signed = append(signed, lh)
		}
	}
	sort.Strings(signed)
	canonReq, _ := buildCanonicalRequest(r, signed, payloadHash)
	scope := strings.Join([]string{date, testRG, "s3", "aws4_request"}, "/")
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256", dateStr, scope, hashHex([]byte(canonReq)),
	}, "\n")
	kSigning := deriveSigningKey(testSK, date, testRG, "s3")
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))

	auth := "AWS4-HMAC-SHA256 " +
		"Credential=" + testAK + "/" + scope + ", " +
		"SignedHeaders=" + strings.Join(signed, ";") + ", " +
		"Signature=" + sig
	r.Header.Set(hdrAuthorization, auth)
}

func TestVerifyHexBodyHash(t *testing.T) {
	body := []byte("payload bytes")
	hash := sha256.Sum256(body)
	hexHash := hex.EncodeToString(hash[:])

	r := httptest.NewRequest("PUT", "http://example.com/b/k", bytes.NewReader(body))
	r.Host = "example.com"
	r.Header.Set("Content-Type", "application/octet-stream")
	signRequest(t, r, hexHash, fixedNow())

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := io.ReadAll(r.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed: %q", got)
	}
}

func TestVerifyHexBodyHashMismatch(t *testing.T) {
	body := []byte("payload bytes")
	hash := sha256.Sum256(body)
	hexHash := hex.EncodeToString(hash[:])

	r := httptest.NewRequest("PUT", "http://example.com/b/k", bytes.NewReader([]byte("tampered")))
	r.Host = "example.com"
	signRequest(t, r, hexHash, fixedNow())

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); err != nil {
		t.Fatalf("verify (header sig should pass): %v", err)
	}
	if _, err := io.ReadAll(r.Body); !errors.Is(err, ErrBadPayloadHash) {
		t.Fatalf("expected ErrBadPayloadHash, got %v", err)
	}
}

func TestVerifyUnsignedPayload(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/b/k", nil)
	r.Host = "example.com"
	signRequest(t, r, unsignedPayload, fixedNow())

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyRejectsTamperedPath(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/b/k", nil)
	r.Host = "example.com"
	signRequest(t, r, unsignedPayload, fixedNow())

	r.URL.Path = "/b/other"

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch, got %v", err)
	}
}

func TestVerifyRejectsExpiredRequests(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/b/k", nil)
	r.Host = "example.com"
	signRequest(t, r, unsignedPayload, fixedNow())
	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: func() time.Time {
		return fixedNow().Add(1 * time.Hour)
	}}
	if err := v.Verify(r); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerifyRejectsUnknownKey(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/b/k", nil)
	r.Host = "example.com"
	signRequest(t, r, unsignedPayload, fixedNow())

	v := &Verifier{Region: testRG, Now: fixedNow, Lookup: func(string) (string, bool) { return "", false }}
	if err := v.Verify(r); !errors.Is(err, ErrUnknownAccessKey) {
		t.Fatalf("expected ErrUnknownAccessKey, got %v", err)
	}
}

func TestVerifyMissingAuthHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/b/k", nil)
	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); !errors.Is(err, ErrMissingAuth) {
		t.Fatalf("expected ErrMissingAuth, got %v", err)
	}
}

func TestVerifySignedChunkedPayload(t *testing.T) {
	now := fixedNow()
	dateStr := now.UTC().Format(timeFormat)
	date := dateStr[:8]
	scope := strings.Join([]string{date, testRG, "s3", "aws4_request"}, "/")
	signingKey := deriveSigningKey(testSK, date, testRG, "s3")

	r := httptest.NewRequest("PUT", "http://example.com/b/k", bytes.NewReader(nil))
	r.Host = "example.com"
	r.Header.Set("Content-Type", "application/octet-stream")
	signRequest(t, r, streamingSignedPayload, now)

	auth, err := parseAuthHeader(r.Header.Get(hdrAuthorization))
	if err != nil {
		t.Fatal(err)
	}

	chunk1 := []byte("first-chunk-data")
	chunk2 := []byte("second-chunk-data!!!")
	sig1 := signChunk(signingKey, dateStr, scope, auth.Signature, chunk1)
	sig2 := signChunk(signingKey, dateStr, scope, sig1, chunk2)
	sigFin := signChunk(signingKey, dateStr, scope, sig2, nil)

	var body bytes.Buffer
	writeChunk(&body, chunk1, sig1)
	writeChunk(&body, chunk2, sig2)
	writeChunk(&body, nil, sigFin)

	r.Body = io.NopCloser(&body)

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := append(append([]byte{}, chunk1...), chunk2...)
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded body mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestVerifyUnsignedChunkedTrailer(t *testing.T) {
	now := fixedNow()
	r := httptest.NewRequest("PUT", "http://example.com/b/k", bytes.NewReader(nil))
	r.Host = "example.com"
	signRequest(t, r, streamingUnsignedTrailer, now)

	chunk := []byte("the body bytes")
	var body bytes.Buffer
	body.WriteString(strconv.FormatInt(int64(len(chunk)), 16) + "\r\n")
	body.Write(chunk)
	body.WriteString("\r\n0\r\nx-amz-checksum-sha256:abc==\r\n\r\n")
	r.Body = io.NopCloser(&body)

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, chunk) {
		t.Fatalf("body mismatch: %q", got)
	}
}

func signChunk(key []byte, datetime, scope, prev string, data []byte) string {
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		datetime,
		scope,
		prev,
		emptySHA256,
		hashHex(data),
	}, "\n")
	return hex.EncodeToString(hmacSHA256(key, []byte(sts)))
}

func writeChunk(buf *bytes.Buffer, data []byte, sig string) {
	buf.WriteString(strconv.FormatInt(int64(len(data)), 16))
	buf.WriteString(";chunk-signature=")
	buf.WriteString(sig)
	buf.WriteString("\r\n")
	buf.Write(data)
	buf.WriteString("\r\n")
}
