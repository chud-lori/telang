package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/telang/telang/internal/storage"
)

func newFakeTelegram(t *testing.T, body []byte) (*httptest.Server, *map[string]int) {
	t.Helper()
	calls := map[string]int{}
	mux := http.NewServeMux()
	const token = "TESTTOKEN"

	mux.HandleFunc("/bot"+token+"/sendDocument", func(w http.ResponseWriter, r *http.Request) {
		calls["sendDocument"]++
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, h, err := r.FormFile("document")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer f.Close()
		got, _ := io.ReadAll(f)
		if !bytes.Equal(got, body) {
			http.Error(w, "body mismatch", 400)
			return
		}
		resp := map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 42,
				"document": map[string]any{
					"file_id":   "fid-1",
					"file_name": h.Filename,
					"file_size": int64(len(body)),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/bot"+token+"/getFile", func(w http.ResponseWriter, r *http.Request) {
		calls["getFile"]++
		_ = r.ParseForm()
		if r.FormValue("file_id") != "fid-1" {
			http.Error(w, "bad file_id", 400)
			return
		}
		resp := map[string]any{
			"ok": true,
			"result": map[string]any{
				"file_id":   "fid-1",
				"file_path": "documents/file_1.bin",
				"file_size": int64(len(body)),
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/file/bot"+token+"/documents/file_1.bin", func(w http.ResponseWriter, r *http.Request) {
		calls["download"]++
		w.Header().Set("Content-Length", "")
		_, _ = w.Write(body)
	})

	mux.HandleFunc("/bot"+token+"/deleteMessage", func(w http.ResponseWriter, r *http.Request) {
		calls["deleteMessage"]++
		_ = r.ParseForm()
		if r.FormValue("message_id") != "42" {
			http.Error(w, "bad message_id", 400)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestBotPutGetDelete(t *testing.T) {
	payload := []byte("hello telang")
	srv, calls := newFakeTelegram(t, payload)

	b, err := New("TESTTOKEN", -1001234567890,
		WithEndpoint(srv.URL),
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := b.Put(context.Background(), "test.bin", int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.MessageID != 42 || res.FileID != "fid-1" {
		t.Fatalf("put result: %+v", res)
	}

	rc, err := b.Get(context.Background(), storage.Ref{MessageID: 42, FileID: "fid-1"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("body mismatch: got %q want %q", got, payload)
	}

	if err := b.Delete(context.Background(), storage.Ref{MessageID: 42, FileID: "fid-1"}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if (*calls)["sendDocument"] != 1 || (*calls)["getFile"] != 1 || (*calls)["download"] != 1 || (*calls)["deleteMessage"] != 1 {
		t.Fatalf("call counts: %+v", *calls)
	}
}

func TestRejectsTooLarge(t *testing.T) {
	b, _ := New("x", 1)
	_, err := b.Put(context.Background(), "blob", MaxObjectSize+1, strings.NewReader(""))
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestDeleteAbsentIsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/botTESTTOKEN/deleteMessage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	b, _ := New("TESTTOKEN", 1, WithEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	if err := b.Delete(context.Background(), storage.Ref{MessageID: 99}); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}
