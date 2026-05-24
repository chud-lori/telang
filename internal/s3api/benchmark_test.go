package s3api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Benchmarks here measure the daemon's overhead (sigv4 + encryption + cache +
// HTTP plumbing) against the fake-Telegram harness. They are useful as
// regression detectors; absolute numbers do not match live Telegram throughput.

const benchObjectSize = 1 << 20 // 1 MiB

func BenchmarkPutObject(b *testing.B) {
	payload := make([]byte, benchObjectSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv, _ := setupServer(b)
	client := srv.Client()
	createBenchBucket(b, srv, client)

	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bench/obj", bytes.NewReader(payload))
		r := signAndDo(b, client, req, payload)
		if r.StatusCode != http.StatusOK {
			b.Fatalf("PUT: %s", r.Status)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
}

func BenchmarkGetObjectWarm(b *testing.B) {
	payload := make([]byte, benchObjectSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv, _ := setupServer(b)
	client := srv.Client()
	createBenchBucket(b, srv, client)
	prePut(b, srv, client, payload)

	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/bench/obj", nil)
		r := signAndDo(b, client, req, nil)
		if r.StatusCode != http.StatusOK {
			b.Fatalf("GET: %s", r.Status)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			b.Fatalf("drain: %v", err)
		}
		r.Body.Close()
	}
}

func BenchmarkRangeGet1KB(b *testing.B) {
	payload := make([]byte, benchObjectSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv, _ := setupServer(b)
	client := srv.Client()
	createBenchBucket(b, srv, client)
	prePut(b, srv, client, payload)

	b.SetBytes(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/bench/obj", nil)
		req.Header.Set("Range", "bytes=512000-513023")
		r := signAndDo(b, client, req, nil)
		if r.StatusCode != http.StatusPartialContent {
			b.Fatalf("Range: %s", r.Status)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
}

func createBenchBucket(b *testing.B, srv *httptest.Server, client *http.Client) {
	b.Helper()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bench", nil)
	resp := signAndDo(b, client, req, nil)
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("CreateBucket: %s", resp.Status)
	}
	resp.Body.Close()
}

func prePut(b *testing.B, srv *httptest.Server, client *http.Client, payload []byte) {
	b.Helper()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bench/obj", bytes.NewReader(payload))
	r := signAndDo(b, client, req, payload)
	if r.StatusCode != http.StatusOK {
		b.Fatalf("seed PUT: %s", r.Status)
	}
	r.Body.Close()
}
