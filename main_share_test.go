package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

func shareTestRouter(t *testing.T, limit int64, auth bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return setupRouter(store, &config.Config{URL: "https://clip.test", BufferSize: limit, DefaultTTL: 72 * time.Hour, MaxRenderSize: 1024, UploadAuth: auth, APIKeys: "test-key", Version: "test"})
}

func shareRequest(router http.Handler, method, url string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, body)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func createTestShare(t *testing.T, router http.Handler, items []models.ShareItem, data map[string]string, burn bool) string {
	return createTestShareWithTTL(t, router, items, data, burn, "")
}

func createTestShareWithTTL(t *testing.T, router http.Handler, items []models.ShareItem, data map[string]string, burn bool, ttl string) string {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	manifest, err := json.Marshal(models.ShareManifest{Version: 1, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("manifest", string(manifest)); err != nil {
		t.Fatal(err)
	}
	for key, value := range data {
		p, err := w.CreateFormFile(key, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Content-Type": w.FormDataContentType(), "X-Burn": map[bool]string{true: "true", false: "false"}[burn], "X-Api-Key": "test-key"}
	if ttl != "" {
		headers["X-TTL"] = ttl
	}
	r := shareRequest(router, "POST", "/api/v1/shares", &body, headers)
	if r.Code != 201 {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var result struct {
		Slug string
		URL  string
	}
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://clip.test/"+result.Slug {
		t.Fatalf("wrong share URL: %s", result.URL)
	}
	return result.Slug
}

func TestShareCustomTTLUsesServerBounds(t *testing.T) {
	router := shareTestRouter(t, 1<<20, false)
	for _, ttl := range []string{"1h", "168h"} {
		slug := createTestShareWithTTL(t, router, textShareItem(), map[string]string{"text": "ttl"}, false, ttl)
		response := shareRequest(router, "GET", "/api/v1/shares/"+slug, nil, nil)
		var metadata struct {
			CreatedAt time.Time `json:"created_at"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
			t.Fatal(err)
		}
		if got := metadata.ExpiresAt.Sub(metadata.CreatedAt); got != mustParseDuration(t, ttl) {
			t.Fatalf("TTL %s stored as %s", ttl, got)
		}
	}
}

func mustParseDuration(t *testing.T, value string) time.Duration {
	t.Helper()
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatal(err)
	}
	return duration
}

func textShareItem() []models.ShareItem {
	return []models.ShareItem{{Kind: "clipboard", Name: "text.txt", Parts: []models.SharePart{{Key: "text", MIME: "text/plain"}}}}
}

func responseForm(t *testing.T, w *httptest.ResponseRecorder) *multipart.Form {
	t.Helper()
	_, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(bytes.NewReader(w.Body.Bytes()), params["boundary"]).ReadForm(32 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := form.RemoveAll(); err != nil {
			t.Error(err)
		}
	})
	return form
}

// assertDeclaredLength proves the progress-bearing header matches the bytes
// actually written; a mismatch would hang or truncate browser downloads.
func assertDeclaredLength(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	declared := w.Header().Get("Content-Length")
	if declared == "" {
		t.Fatal("missing Content-Length")
	}
	if declared != strconv.Itoa(w.Body.Len()) {
		t.Fatalf("Content-Length = %s, body = %d", declared, w.Body.Len())
	}
}

func TestShareTextRoundTripAndOldRaw(t *testing.T) {
	router := shareTestRouter(t, 1<<20, false)
	text := "  中文\n末尾空行\n"
	slug := createTestShare(t, router, textShareItem(), map[string]string{"text": text}, false)
	for _, route := range []string{"/raw/" + slug, "/" + slug} {
		r := shareRequest(router, "GET", route, nil, map[string]string{"User-Agent": "curl"})
		if r.Code != 200 || r.Body.String() != text {
			t.Fatalf("legacy raw %s: %d %q", route, r.Code, r.Body.String())
		}
	}
	r := shareRequest(router, "GET", "/api/v1/shares/"+slug+"/content", nil, nil)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	assertDeclaredLength(t, r)
	form := responseForm(t, r)
	f, err := form.File["text"][0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	actual, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != text {
		t.Fatalf("text changed: %q", actual)
	}
	meta := shareRequest(router, "GET", "/api/v1/shares/"+slug, nil, nil)
	var info struct {
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(meta.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.ExpiresAt.Sub(info.CreatedAt) != 72*time.Hour {
		t.Fatalf("TTL = %s", info.ExpiresAt.Sub(info.CreatedAt))
	}
	a := shareRequest(router, "GET", "/api/v1/shares/"+slug+"/content", nil, nil)
	b := shareRequest(router, "GET", "/api/v1/shares/"+slug+"/content", nil, nil)
	if a.Code != 200 || b.Code != 200 || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatal("ordinary share responses must be byte-stable for range resume")
	}
}

func TestShareMetadataCarriesSafeServerPreview(t *testing.T) {
	router := shareTestRouter(t, 1<<20, false)
	safe := createTestShare(t, router, textShareItem(), map[string]string{"text": "第一行\r\n这是发件人和收件人都应看到的开头"}, false)
	response := shareRequest(router, "GET", "/api/v1/shares/"+safe, nil, nil)
	var metadata struct {
		Manifest models.ShareManifest `json:"manifest"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if got := metadata.Manifest.Items[0].Preview; !strings.HasPrefix(got, "第一行 这是") || len([]rune(got)) > 24 || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("server preview = %q", got)
	}

	secret := createTestShare(t, router, textShareItem(), map[string]string{"text": "sk-secret-value-that-must-not-be-published"}, false)
	response = shareRequest(router, "GET", "/api/v1/shares/"+secret, nil, nil)
	metadata = struct {
		Manifest models.ShareManifest `json:"manifest"`
	}{}
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if got := metadata.Manifest.Items[0].Preview; got != "" {
		t.Fatalf("credential-like preview leaked: %q", got)
	}
}

func TestShareRichBatchAndBurnArchive(t *testing.T) {
	router := shareTestRouter(t, 1<<20, false)
	items := []models.ShareItem{{Kind: "clipboard", Name: "rich.html", Parts: []models.SharePart{{Key: "plain", MIME: "text/plain"}, {Key: "html", MIME: "text/html"}}}, {Kind: "file", Name: "../报告.txt", Parts: []models.SharePart{{Key: "file", MIME: "text/plain"}}}}
	slug := createTestShare(t, router, items, map[string]string{"plain": "hello", "html": "<b>hello</b><script>alert(1)</script>", "file": "file data"}, true)
	page := shareRequest(router, "GET", "/"+slug, nil, map[string]string{"Accept": "text/html"})
	if page.Code != 200 || strings.Contains(page.Body.String(), "<b>hello") {
		t.Fatal("confirmation leaked content")
	}
	if w := shareRequest(router, "GET", "/api/v1/shares/"+slug+"/content", nil, nil); w.Code != 409 {
		t.Fatalf("unguarded read %d", w.Code)
	}
	if w := shareRequest(router, "POST", "/api/v1/shares/"+slug+"/receive", nil, nil); w.Code != 400 {
		t.Fatalf("missing confirmation %d", w.Code)
	}
	w := shareRequest(router, "POST", "/api/v1/shares/"+slug+"/receive", nil, map[string]string{"X-Nclip-Confirm": "1"})
	if w.Code != 200 {
		t.Fatalf("receive %d %s", w.Code, w.Body.String())
	}
	assertDeclaredLength(t, w)
	form := responseForm(t, w)
	if len(form.File["html"]) != 1 || len(form.File["file"]) != 1 || len(form.File["archive"]) != 1 {
		t.Fatalf("missing representations: %+v", form.File)
	}
	assertShareArchive(t, form.File["archive"][0], 4)
	if w := shareRequest(router, "GET", "/raw/"+slug, nil, nil); w.Code != 404 {
		t.Fatalf("burn survived: %d", w.Code)
	}
}

func assertShareArchive(t *testing.T, file *multipart.FileHeader, wantEntries int) {
	t.Helper()
	f, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != wantEntries {
		t.Fatalf("archive entries = %d", len(zr.File))
	}
	for _, entry := range zr.File {
		if strings.Contains(entry.Name, "..") {
			t.Fatal("unsafe archive path")
		}
	}
}

func TestShareConcurrentReceiveAndLegacyRaw(t *testing.T) {
	router := shareTestRouter(t, 1<<20, false)
	slug := createTestShare(t, router, textShareItem(), map[string]string{"text": "exactly once"}, true)
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var w *httptest.ResponseRecorder
			if i%2 == 0 {
				w = shareRequest(router, "POST", "/api/v1/shares/"+slug+"/receive", nil, map[string]string{"X-Nclip-Confirm": "1"})
			} else {
				w = shareRequest(router, "GET", "/raw/"+slug, nil, nil)
			}
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code != 404 {
			t.Fatalf("unexpected code %d", code)
		}
	}
	if success != 1 {
		t.Fatalf("successful destructive readers = %d", success)
	}
}

func TestShareAuthLimitsAndInvalidParts(t *testing.T) {
	router := shareTestRouter(t, 2048, true)
	if w := shareRequest(router, "POST", "/api/v1/shares", nil, nil); w.Code != 401 {
		t.Fatalf("missing auth %d", w.Code)
	}
	createTestShare(t, router, textShareItem(), map[string]string{"text": "authenticated"}, false)
	for _, tc := range []struct {
		name, manifest, payload string
		want                    int
	}{
		{"duplicate", "{\"version\":1,\"items\":[{\"kind\":\"clipboard\",\"parts\":[{\"key\":\"x\",\"mime\":\"text/plain\"},{\"key\":\"x\",\"mime\":\"text/html\"}]}]}", "x", 400},
		{"oversize", "{}", strings.Repeat("x", 3000), 413},
		{"bad-png", "{\"version\":1,\"items\":[{\"kind\":\"clipboard\",\"parts\":[{\"key\":\"x\",\"mime\":\"image/png\"}]}]}", "not a PNG", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("manifest", tc.manifest)
			_ = mw.WriteField("x", tc.payload)
			_ = mw.Close()
			w := shareRequest(router, "POST", "/api/v1/shares", &body, map[string]string{"Content-Type": mw.FormDataContentType(), "X-Api-Key": "test-key"})
			if w.Code != tc.want {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestShareRangeResumeUsesStableBytes(t *testing.T) {
	router := shareTestRouter(t, 1<<20, false)
	slug := createTestShare(t, router, textShareItem(), map[string]string{"text": strings.Repeat("resumeable\n", 1000)}, false)
	path := "/api/v1/shares/" + slug + "/content"

	first := shareRequest(router, "GET", path, nil, nil)
	if first.Code != 200 {
		t.Fatalf("full download %d: %s", first.Code, first.Body.String())
	}
	if first.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q", first.Header().Get("Accept-Ranges"))
	}
	full := append([]byte(nil), first.Body.Bytes()...)
	split := len(full) / 3

	head := shareRequest(router, "GET", path, nil, map[string]string{"Range": fmt.Sprintf("bytes=0-%d", split-1)})
	if head.Code != http.StatusPartialContent {
		t.Fatalf("first range %d: %s", head.Code, head.Body.String())
	}
	tail := shareRequest(router, "GET", path, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-", split)})
	if tail.Code != http.StatusPartialContent {
		t.Fatalf("resume range %d: %s", tail.Code, tail.Body.String())
	}
	joined := append(append([]byte(nil), head.Body.Bytes()...), tail.Body.Bytes()...)
	if !bytes.Equal(joined, full) {
		t.Fatalf("resumed bytes differ: head=%d tail=%d full=%d", head.Body.Len(), tail.Body.Len(), len(full))
	}
	if got := tail.Header().Get("Content-Range"); got != fmt.Sprintf("bytes %d-%d/%d", split, len(full)-1, len(full)) {
		t.Fatalf("Content-Range = %q", got)
	}
}
