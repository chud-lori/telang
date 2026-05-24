package s3api

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/sigv4"
	"github.com/telang/telang/internal/storage/bot"
)

// fakeTelegram is a minimal mock of the Telegram Bot API exposing just
// sendDocument, getFile, the file download path, and deleteMessage.
type fakeTelegram struct {
	t      *testing.T
	server *httptest.Server
	store  map[int64][]byte
	nextID int64
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{
		t:     t,
		store: map[int64][]byte{},
	}
	const token = "TESTTOKEN"
	mux := http.NewServeMux()

	mux.HandleFunc("/bot"+token+"/sendDocument", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file, _, err := r.FormFile("document")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		f.nextID++
		id := f.nextID
		f.store[id] = body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": id,
				"document": map[string]any{
					"file_id":   "fid-" + strconv.FormatInt(id, 10),
					"file_size": int64(len(body)),
				},
			},
		})
	})

	mux.HandleFunc("/bot"+token+"/getFile", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fid := r.FormValue("file_id")
		idStr := strings.TrimPrefix(fid, "fid-")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		if _, ok := f.store[id]; !ok {
			http.Error(w, `{"ok":false,"error_code":400,"description":"file not found"}`, 200)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"file_id":   fid,
				"file_path": "documents/" + idStr + ".bin",
			},
		})
	})

	mux.HandleFunc("/file/bot"+token+"/documents/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		idStr := strings.TrimSuffix(name, ".bin")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		body, ok := f.store[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})

	mux.HandleFunc("/bot"+token+"/deleteMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, _ := strconv.ParseInt(r.FormValue("message_id"), 10, 64)
		if _, ok := f.store[id]; !ok {
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"message to delete not found"}`))
			return
		}
		delete(f.store, id)
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func setupServer(t *testing.T) (*httptest.Server, *fakeTelegram) {
	t.Helper()
	dir := t.TempDir()
	meta, err := metadata.Open(context.Background(), filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	t.Cleanup(func() { meta.Close() })

	tg := newFakeTelegram(t)
	backend, err := bot.New("TESTTOKEN", -100100,
		bot.WithEndpoint(tg.server.URL),
		bot.WithHTTPClient(tg.server.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		Meta:       meta,
		Backend:    backend,
		StagingDir: filepath.Join(dir, "staging"),
	}
	v := &sigv4.Verifier{
		Region: testRG,
		Lookup: func(ak string) (string, bool) {
			if ak == testAK {
				return testSK, true
			}
			return "", false
		},
		Now: func() time.Time { return time.Now().UTC() },
	}
	srv := httptest.NewServer(&Handler{Verifier: v, Service: svc})
	t.Cleanup(srv.Close)
	return srv, tg
}

const (
	testAK = "AKIDEXAMPLE"
	testSK = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	testRG = "tg-1"
)

// signAndDo signs a request like a real S3 client would and executes it.
func signAndDo(t *testing.T, client *http.Client, req *http.Request, body []byte) *http.Response {
	t.Helper()
	now := time.Now().UTC()
	dateStr := now.Format("20060102T150405Z")
	date := dateStr[:8]
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])
	req.Header.Set("X-Amz-Date", dateStr)
	req.Header.Set("X-Amz-Content-Sha256", hashHex)
	if req.Body != nil {
		req.ContentLength = int64(len(body))
	}

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for h := range req.Header {
		lh := strings.ToLower(h)
		if strings.HasPrefix(lh, "x-amz-") && lh != "x-amz-date" && lh != "x-amz-content-sha256" {
			signed = append(signed, lh)
		}
	}
	sort.Strings(signed)

	canon := canonicalForTest(req, signed, hashHex)
	scope := strings.Join([]string{date, testRG, "s3", "aws4_request"}, "/")
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", dateStr, scope, sha256Hex([]byte(canon))}, "\n")
	signingKey := deriveKey(testSK, date, testRG, "s3")
	sig := hmacHex(signingKey, sts)

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testAK+"/"+scope+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

func canonicalForTest(r *http.Request, signed []string, payloadHash string) string {
	uri := r.URL.Path
	if uri == "" {
		uri = "/"
	}
	var hdrs strings.Builder
	for _, h := range signed {
		var val string
		switch h {
		case "host":
			val = r.Host
			if val == "" {
				val = r.URL.Host
			}
		case "content-length":
			val = strconv.FormatInt(r.ContentLength, 10)
		default:
			val = r.Header.Get(http.CanonicalHeaderKey(h))
		}
		hdrs.WriteString(h)
		hdrs.WriteByte(':')
		hdrs.WriteString(strings.TrimSpace(val))
		hdrs.WriteByte('\n')
	}
	return strings.Join([]string{
		r.Method, uri, r.URL.RawQuery, hdrs.String(), "", strings.Join(signed, ";"), payloadHash,
	}, "\n")
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacHex(key []byte, data string) string {
	// Use a stripped-down mac to avoid pulling crypto/hmac into the test.
	h := newHMAC(key)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func deriveKey(secret, date, region, service string) []byte {
	k := []byte("AWS4" + secret)
	k = mac(k, date)
	k = mac(k, region)
	k = mac(k, service)
	k = mac(k, "aws4_request")
	return k
}

func mac(key []byte, data string) []byte {
	h := newHMAC(key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// We can't import crypto/hmac in the test from a function package; use it here.
// (Imported indirectly via the helper just below.)
type macHash interface {
	io.Writer
	Sum([]byte) []byte
}

func newHMAC(key []byte) macHash {
	return hmacWrapper{h: hmacNew(key)}
}

func TestEndToEndV01(t *testing.T) {
	srv, tg := setupServer(t)
	client := srv.Client()

	// 1. Create bucket.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/mybucket", nil)
	resp := signAndDo(t, client, req, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateBucket: %s\n%s", resp.Status, body)
	}
	resp.Body.Close()

	// 2. Put a 1 KB object.
	payload := bytes.Repeat([]byte{0xAB}, 1024)
	wantMD5 := md5.Sum(payload)
	wantETag := hex.EncodeToString(wantMD5[:])

	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/mybucket/hello.bin", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp = signAndDo(t, client, req, payload)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PutObject: %s\n%s", resp.Status, body)
	}
	if got := strings.Trim(resp.Header.Get("ETag"), `"`); got != wantETag {
		t.Fatalf("ETag: got %s want %s", got, wantETag)
	}
	resp.Body.Close()

	// 3. HEAD the object.
	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/mybucket/hello.bin", nil)
	resp = signAndDo(t, client, req, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HeadObject: %s", resp.Status)
	}
	if resp.Header.Get("Content-Length") != "1024" {
		t.Fatalf("Content-Length: %q", resp.Header.Get("Content-Length"))
	}
	if got := strings.Trim(resp.Header.Get("ETag"), `"`); got != wantETag {
		t.Fatalf("HEAD ETag: got %s want %s", got, wantETag)
	}
	resp.Body.Close()

	// 4. GET the object and compare bytes.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/mybucket/hello.bin", nil)
	resp = signAndDo(t, client, req, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GetObject: %s", resp.Status)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("body bytes differ: got %d bytes, want %d", len(got), len(payload))
	}

	// 5. DELETE the object.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/mybucket/hello.bin", nil)
	resp = signAndDo(t, client, req, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DeleteObject: %s", resp.Status)
	}
	resp.Body.Close()

	// 6. HEAD now returns 404.
	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/mybucket/hello.bin", nil)
	resp = signAndDo(t, client, req, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("post-delete HEAD: want 404, got %s", resp.Status)
	}
	resp.Body.Close()

	// 7. Verify Telegram side: message has been removed.
	if len(tg.store) != 0 {
		t.Fatalf("fake Telegram still has %d messages", len(tg.store))
	}
}

func TestUnsignedRequestReturns403XML(t *testing.T) {
	srv, _ := setupServer(t)
	resp, err := http.Get(srv.URL + "/mybucket/whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	var e S3Error
	if err := xml.Unmarshal(body, &e); err != nil {
		t.Fatalf("xml: %v\nbody=%s", err, body)
	}
	if e.Code != "AccessDenied" {
		t.Fatalf("code: %q", e.Code)
	}
}
