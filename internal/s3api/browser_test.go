package s3api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telang/telang/internal/cache"
	"github.com/telang/telang/internal/keys"
	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/sigv4"
	"github.com/telang/telang/internal/storage/bot"
)

func setupBrowserServer(t *testing.T, password string) (*httptest.Server, *fakeTelegram) {
	t.Helper()
	dir := t.TempDir()
	meta, err := metadata.Open(context.Background(), filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })

	tg := newFakeTelegram(t)
	backend, _ := bot.New("TESTTOKEN", -100100,
		bot.WithEndpoint(tg.server.URL),
		bot.WithHTTPClient(tg.server.Client()),
	)
	keyStore, _ := keys.Load(filepath.Join(dir, "keys.toml"))
	blobCache, _ := cache.Open(filepath.Join(dir, "cache"), 100<<20)
	svc := &Service{
		Meta: meta, Backend: backend, Keys: keyStore, Cache: blobCache,
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
	}
	srv := httptest.NewServer(&Handler{
		Verifier: v, Service: svc,
		Browser: NewBrowserUI(BrowserOptions{Enabled: true, Password: password}, svc),
	})
	t.Cleanup(srv.Close)
	return srv, tg
}

func sigPut(t *testing.T, srv *httptest.Server, path string, body []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+path, rd)
	resp := signAndDo(t, srv.Client(), req, body)
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s: %s\n%s", path, resp.Status, out)
	}
	resp.Body.Close()
}

func TestBrowserListingAndAnonymousDownload(t *testing.T) {
	srv, _ := setupBrowserServer(t, "")
	sigPut(t, srv, "/bui", nil)
	sigPut(t, srv, "/bui/hello.txt", []byte("hello world"))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/bui/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing status: %s", resp.Status)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("content type: %s", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if !strings.Contains(page, "hello.txt") {
		t.Fatalf("listing missing key:\n%s", page)
	}
	if !strings.Contains(page, "read-only (no password configured)") {
		t.Fatalf("expected read-only banner:\n%s", page)
	}

	// Anonymous download works.
	resp, _ = http.Get(srv.URL + "/_browse/bui/hello.txt")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "hello world" {
		t.Fatalf("download body: %q", got)
	}

	// Anonymous write is refused when no password is configured.
	body2, ct := multipartBody(t, "file", "fromform.txt", []byte("ignored"))
	resp, _ = http.Post(srv.URL+"/_browse/bui/", ct, body2)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anon upload (no password): want 403, got %s", resp.Status)
	}
}

func TestBrowserLoginUploadDelete(t *testing.T) {
	srv, _ := setupBrowserServer(t, "hunter2")
	sigPut(t, srv, "/buw", nil)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Wrong password → 401.
	resp, _ := client.PostForm(srv.URL+"/_browse/_login", url.Values{"password": {"nope"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: %s", resp.Status)
	}

	// Correct password → 303 + session cookie.
	resp, _ = client.PostForm(srv.URL+"/_browse/_login", url.Values{"password": {"hunter2"}, "return": {"/buw/"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %s", resp.Status)
	}

	// Upload via the form.
	body, ct := multipartBody(t, "file", "browserupload.bin", []byte("uploaded via browser"))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/_browse/buw/", body)
	req.Header.Set("Content-Type", ct)
	resp, _ = client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload: %s", resp.Status)
	}

	// Verify it lands as a real S3 object retrievable via signed GET.
	gReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/buw/browserupload.bin", nil)
	gResp := signAndDo(t, srv.Client(), gReq, nil)
	got, _ := io.ReadAll(gResp.Body)
	gResp.Body.Close()
	if string(got) != "uploaded via browser" {
		t.Fatalf("uploaded bytes mismatch: %q", got)
	}

	// Delete via the form.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/_browse/buw/browserupload.bin?delete=1", nil)
	resp, _ = client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %s", resp.Status)
	}
	hReq, _ := http.NewRequest(http.MethodHead, srv.URL+"/buw/browserupload.bin", nil)
	hResp := signAndDo(t, srv.Client(), hReq, nil)
	hResp.Body.Close()
	if hResp.StatusCode != http.StatusNotFound {
		t.Fatalf("post-delete HEAD: want 404, got %s", hResp.Status)
	}
}

func multipartBody(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	return buf, mw.FormDataContentType()
}
