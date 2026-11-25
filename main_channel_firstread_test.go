package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/storage"
)

type nativeReadFailureStore struct {
	*storage.FilesystemStore
	failRead bool
}
type nativeReadFailureBody struct{ io.ReadCloser }

func (b *nativeReadFailureBody) Read([]byte) (int, error) {
	return 0, errors.New("injected source failure before first byte")
}
func (s *nativeReadFailureStore) OpenContent(id string) (io.ReadCloser, int64, error) {
	body, size, err := s.FilesystemStore.OpenContent(id)
	if err == nil && s.failRead {
		return &nativeReadFailureBody{ReadCloser: body}, size, nil
	}
	return body, size, err
}

func TestRegressionNativeFirstReadAndSeekFailureAllowRetry(t *testing.T) {
	for _, seek := range []bool{false, true} {
		t.Run(map[bool]string{false: "first read", true: "seek"}[seek], func(t *testing.T) { assertNativeReadFailureRetry(t, seek) })
	}
}

func assertNativeReadFailureRetry(t *testing.T, seek bool) {
	t.Helper()
	root := t.TempDir()
	base, err := storage.NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	store := &nativeReadFailureStore{FilesystemStore: base}
	router := setupRouter(store, &config.Config{URL: "https://clip.test", DataDir: root, BufferSize: 8 << 20, DefaultTTL: 72 * time.Hour, MaxRenderSize: 1024, Version: "test"})
	f := createAdmittedChannelFixture(t, router)
	f.router, f.store, f.contentPath = router, base, "/api/v1/channels/"+f.channelID+"/content"
	uploadChannelFixtureContent(t, f, []byte("data"))
	url := issueRegressionTicket(t, f)
	headers := map[string]string{}
	if seek {
		first := channelRequestWithCookies(router, http.MethodGet, url, nil, nil, map[string]string{"Range": "bytes=0-1"})
		if first.Code != http.StatusPartialContent {
			t.Fatalf("initial range: %d", first.Code)
		}
		headers["Range"] = "bytes=2-"
	}
	store.failRead = true
	w := channelRequestWithCookies(router, http.MethodGet, url, nil, nil, headers)
	assertNativeJSONReadError(t, w)
	held, err := base.ListEphemeralPastes()
	if err != nil || len(held) != 1 {
		t.Fatal("source failure did not retain content", err)
	}
	store.failRead = false
	retry := channelRequestWithCookies(router, http.MethodGet, url, nil, nil, headers)
	wantStatus, wantBody := http.StatusOK, "data"
	if seek {
		wantStatus, wantBody = http.StatusPartialContent, "ta"
	}
	if retry.Code != wantStatus || retry.Body.String() != wantBody {
		t.Fatalf("same-ticket retry: %d %q", retry.Code, retry.Body.String())
	}
}

func assertNativeJSONReadError(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("read failure status=%d", w.Code)
	}
	for _, name := range []string{"Content-Disposition", "Content-Range", "Accept-Ranges", "ETag"} {
		if w.Header().Get(name) != "" {
			t.Fatalf("error retained %s", name)
		}
	}
	if length := w.Header().Get("Content-Length"); length != "" && length != strconv.Itoa(w.Body.Len()) {
		t.Fatalf("error length=%s body=%d", length, w.Body.Len())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal("not JSON")
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error != "channel_delivery_failed" {
		t.Fatalf("error body=%q", w.Body.String())
	}
}
