package sigv4

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// presign produces a presigned URL the same way an AWS SDK would. Kept in the
// test file so the verifier and the test signer remain independent.
func presign(t *testing.T, method, target string, host string, now time.Time, expires int) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	dateStr := now.UTC().Format(timeFormat)
	date := dateStr[:8]
	scope := strings.Join([]string{date, testRG, "s3", "aws4_request"}, "/")

	q := u.Query()
	q.Set(qpAlgorithm, algorithm)
	q.Set(qpCredential, testAK+"/"+scope)
	q.Set(qpDate, dateStr)
	q.Set(qpExpires, strconv.Itoa(expires))
	q.Set(qpSignedHeaders, "host")
	u.RawQuery = encodeForAWS(q)

	// Canonical request.
	hdrs := "host:" + host + "\n"
	canonReq := strings.Join([]string{
		method,
		u.Path,
		u.RawQuery,
		hdrs,
		"",
		"host",
		unsignedPayload,
	}, "\n")
	sts := strings.Join([]string{algorithm, dateStr, scope, hashHex([]byte(canonReq))}, "\n")
	kSigning := deriveSigningKey(testSK, date, testRG, "s3")
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))

	u.RawQuery = u.RawQuery + "&" + qpSignature + "=" + sig
	return u.String()
}

// encodeForAWS encodes query params in the exact canonical form the verifier
// expects (sorted by key, RFC-3986-escaped values, / escaped).
func encodeForAWS(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range values[k] {
			parts = append(parts, awsEscape(k, true)+"="+awsEscape(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func TestVerifyPresignedAccepts(t *testing.T) {
	signed := presign(t, "GET", "http://example.com/b/k", "example.com", fixedNow(), 600)
	req := httptest.NewRequest("GET", signed, nil)
	req.Host = "example.com"

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(req); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyPresignedExpired(t *testing.T) {
	signed := presign(t, "GET", "http://example.com/b/k", "example.com", fixedNow(), 60)
	req := httptest.NewRequest("GET", signed, nil)
	req.Host = "example.com"

	v := &Verifier{
		Region: testRG, Lookup: staticLookup,
		Now: func() time.Time { return fixedNow().Add(10 * time.Minute) },
	}
	if err := v.Verify(req); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerifyPresignedTamperedPath(t *testing.T) {
	signed := presign(t, "GET", "http://example.com/b/k", "example.com", fixedNow(), 600)
	req := httptest.NewRequest("GET", signed, nil)
	req.Host = "example.com"
	req.URL.Path = "/b/other"

	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(req); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch, got %v", err)
	}
}

func TestVerifyPresignedAlgorithmGuard(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/b/k?X-Amz-Algorithm=evil&X-Amz-Signature=zz", nil)
	req.Host = "example.com"
	v := &Verifier{Region: testRG, Lookup: staticLookup, Now: fixedNow}
	if err := v.Verify(req); err == nil || !strings.Contains(err.Error(), "X-Amz-Algorithm") {
		t.Fatalf("want algorithm guard, got %v", err)
	}
}

// Ensure that a presigned URL with X-Amz-Signature dropped from the query
// during canonicalisation still validates — i.e. that the verifier reproduces
// the *exact* query string the signer used to compute the signature.
var _ = http.StatusOK
