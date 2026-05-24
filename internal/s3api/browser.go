package s3api

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html.tmpl
var browserTemplates embed.FS

var browserTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"humanSize": humanSize,
	"rfc3339":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
}).ParseFS(browserTemplates, "templates/*.html.tmpl"))

// BrowserOptions configures the minimal browser UI. Disabled means the
// daemon serves only the S3 API.
type BrowserOptions struct {
	Enabled  bool
	Password string // empty = read-only UI; login route returns 404
}

// BrowserUI is the small server-rendered admin panel. It is intentionally
// stateful only in-memory: sessions are random tokens kept here, so a
// restart invalidates everyone (acceptable for a hobby daemon).
type BrowserUI struct {
	opts    BrowserOptions
	service *Service

	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewBrowserUI(opts BrowserOptions, svc *Service) *BrowserUI {
	return &BrowserUI{
		opts:     opts,
		service:  svc,
		sessions: map[string]time.Time{},
	}
}

const (
	sessionCookie  = "telang_session"
	sessionTTL     = 24 * time.Hour
	maxUploadBytes = 2 * 1024 * 1024 * 1024 // 2 GB
)

// dispatch routes a request that the main S3 handler decided is a browser
// request. The caller has already established that sigv4 should be skipped.
func (b *BrowserUI) dispatch(w http.ResponseWriter, r *http.Request) {
	if !b.opts.Enabled {
		http.NotFound(w, r)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/_browse/_login":
		b.handleLogin(w, r)
	case p == "/_browse/_logout":
		b.handleLogout(w, r)
	case strings.HasPrefix(p, "/_browse/"):
		b.handleBrowseRoute(w, r, strings.TrimPrefix(p, "/_browse/"))
	default:
		// Should be a GET /{bucket}/ listing.
		bucket, key := splitPath(p)
		if bucket == "" || key != "" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		b.renderListing(w, r, bucket)
	}
}

// shouldHandle reports whether the main router should hand a request to the
// browser UI rather than the S3 layer.
func (b *BrowserUI) shouldHandle(r *http.Request) bool {
	if b == nil || !b.opts.Enabled {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/_browse/") {
		return true
	}
	// GET /{bucket}/ with Accept: text/html and no Authorization is a
	// browser visiting the listing page. Anything else is S3.
	if r.Method != http.MethodGet {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	bucket, key := splitPath(r.URL.Path)
	if bucket == "" || key != "" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// --- listing ---

type listingView struct {
	Bucket    string
	Objects   []listingObject
	LoggedIn  bool
	HasLogin  bool // password is configured
	UploadURL string
	LoginURL  string
	LogoutURL string
}

type listingObject struct {
	Key          string
	Size         int64
	ContentType  string
	LastModified time.Time
	DownloadURL  string
	DeleteURL    string
}

func (b *BrowserUI) renderListing(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := b.service.Meta.GetBucket(r.Context(), bucket); err != nil {
		http.NotFound(w, r)
		return
	}
	objs, _, err := b.service.Meta.ListObjects(r.Context(), bucket, "", "", 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := listingView{
		Bucket:    bucket,
		LoggedIn:  b.isLoggedIn(r),
		HasLogin:  b.opts.Password != "",
		UploadURL: "/_browse/" + bucket + "/",
		LoginURL:  "/_browse/_login",
		LogoutURL: "/_browse/_logout",
	}
	for _, o := range objs {
		view.Objects = append(view.Objects, listingObject{
			Key:          o.Key,
			Size:         o.Size,
			ContentType:  o.ContentType,
			LastModified: o.CreatedAt,
			DownloadURL:  "/_browse/" + bucket + "/" + o.Key,
			DeleteURL:    "/_browse/" + bucket + "/" + o.Key + "?delete=1",
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := browserTmpl.ExecuteTemplate(w, "listing.html.tmpl", view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// --- /_browse/{bucket}/... ---

func (b *BrowserUI) handleBrowseRoute(w http.ResponseWriter, r *http.Request, rest string) {
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	bucket := rest[:slash]
	key := rest[slash+1:]
	switch {
	case key == "" && r.Method == http.MethodPost:
		b.handleUpload(w, r, bucket)
	case key != "" && r.Method == http.MethodGet:
		b.handleDownload(w, r, bucket, key)
	case key != "" && r.Method == http.MethodPost && r.URL.Query().Get("delete") != "":
		b.handleDelete(w, r, bucket, key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *BrowserUI) handleDownload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	obj, body, err := b.service.GetObject(r.Context(), bucket, key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer body.Close()
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.Size))
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(key)+`"`)
	_, _ = io.Copy(w, body)
}

func (b *BrowserUI) handleUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !b.requireSession(w, r) {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing 'file' field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	key := sanitizeFilename(hdr.Filename)
	if key == "" {
		http.Error(w, "empty filename", http.StatusBadRequest)
		return
	}
	ct := hdr.Header.Get("Content-Type")
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(key))
	}
	if hdr.Size <= 0 || hdr.Size > maxUploadBytes {
		http.Error(w, "bad upload size", http.StatusBadRequest)
		return
	}
	if _, err := b.service.PutObject(r.Context(), bucket, key, ct, hdr.Size, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/"+bucket+"/", http.StatusSeeOther)
}

func (b *BrowserUI) handleDelete(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !b.requireSession(w, r) {
		return
	}
	if err := b.service.DeleteObject(r.Context(), bucket, key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/"+bucket+"/", http.StatusSeeOther)
}

// --- /_browse/_login + /_browse/_logout ---

func (b *BrowserUI) handleLogin(w http.ResponseWriter, r *http.Request) {
	if b.opts.Password == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = browserTmpl.ExecuteTemplate(w, "login.html.tmpl", nil)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pw := r.FormValue("password")
		if subtle.ConstantTimeCompare([]byte(pw), []byte(b.opts.Password)) != 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_ = browserTmpl.ExecuteTemplate(w, "login.html.tmpl", map[string]any{"Error": "wrong password"})
			return
		}
		tok := b.newSession()
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Expires:  time.Now().Add(sessionTTL),
		})
		ret := r.FormValue("return")
		if ret == "" || !strings.HasPrefix(ret, "/") {
			ret = "/"
		}
		http.Redirect(w, r, ret, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *BrowserUI) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		b.mu.Lock()
		delete(b.sessions, c.Value)
		b.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- session helpers ---

func (b *BrowserUI) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if b.opts.Password == "" {
		http.Error(w, "browser-write disabled (no password configured)", http.StatusForbidden)
		return false
	}
	if !b.isLoggedIn(r) {
		http.Redirect(w, r, "/_browse/_login?return="+r.URL.Path, http.StatusSeeOther)
		return false
	}
	return true
}

func (b *BrowserUI) isLoggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Since(t) > sessionTTL {
		delete(b.sessions, c.Value)
		return false
	}
	return true
}

func (b *BrowserUI) newSession() string {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	tok := hex.EncodeToString(raw[:])
	b.mu.Lock()
	b.sessions[tok] = time.Now()
	b.mu.Unlock()
	return tok
}

// --- helpers ---

// sanitizeFilename strips path components and dangerous characters so the
// upload form's filename can't escape the bucket namespace.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	// Disallow leading dots so uploads can't shadow `.git/` or similar.
	name = strings.TrimLeft(name, ".")
	return name
}

func humanSize(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n) / k
	i := 0
	for v >= k && i < len(units)-1 {
		v /= k
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// Compile-time sanity: ensure the embed contains the templates we expect.
func init() {
	for _, name := range []string{"listing.html.tmpl", "login.html.tmpl"} {
		if browserTmpl.Lookup(name) == nil {
			panic(errors.New("browser: missing template " + name))
		}
	}
}
