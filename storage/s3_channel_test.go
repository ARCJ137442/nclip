package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johnwmail/nclip/models"
)

type fakeS3Objects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func TestS3PendingFinalizeIsSweptAndWriterLockIsExclusive(t *testing.T) {
	objects := &fakeS3Objects{objects: make(map[string][]byte)}
	server := httptest.NewServer(http.HandlerFunc(objects.serveHTTP))
	defer server.Close()
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: aws.String(server.URL),
		UsePathStyle: true,
	})
	first := &S3Store{bucket: "nclip-test", prefix: "channels", client: client}
	second := &S3Store{bucket: "nclip-test", prefix: "channels", client: client}

	if err := first.AcquireChannelWriter(); err != nil {
		t.Fatalf("first writer lock: %v", err)
	}
	if err := second.AcquireChannelWriter(); err == nil {
		t.Fatal("second S3 store acquired an already-held writer lock")
	}

	if err := first.StoreChannelContentStream("ARPH5", bytes.NewReader([]byte("orphan")), int64(len("orphan"))); err != nil {
		t.Fatalf("store pending finalize content: %v", err)
	}
	if err := first.StoreChannelContentStream("CMT32", bytes.NewReader([]byte("committed")), int64(len("committed"))); err != nil {
		t.Fatalf("store commit test content: %v", err)
	}
	expires := time.Now().Add(time.Hour)
	if err := first.Store(&models.Paste{ID: "CMT32", CreatedAt: time.Now(), ExpiresAt: &expires, Size: 9, ContentType: "text/plain", EphemeralChannelID: "channel"}); err != nil {
		t.Fatalf("store commit test metadata: %v", err)
	}
	if err := first.CommitChannelContent("CMT32"); err != nil {
		t.Fatalf("commit S3 channel content: %v", err)
	}
	content, err := first.GetContent("CMT32")
	if err != nil || string(content) != "committed" {
		t.Fatalf("S3 committed content = %q err=%v", content, err)
	}
	if err := first.Delete("CMT32"); err != nil {
		t.Fatalf("clean S3 commit fixture: %v", err)
	}
	if err := first.SweepPendingChannelContent(); err != nil {
		t.Fatalf("sweep pending finalize content: %v", err)
	}
	objects.mu.Lock()
	remaining := len(objects.objects)
	objects.mu.Unlock()
	if remaining != 1 { // The live writer lock is intentionally retained.
		t.Fatalf("pending finalization left %d S3 objects; want only writer lock", remaining)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release S3 writer lock: %v", err)
	}
	objects.mu.Lock()
	remaining = len(objects.objects)
	objects.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("S3 writer lock remained after close: %d objects", remaining)
	}
}

func (f *fakeS3Objects) serveHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.EscapedPath(), "/nclip-test/")
	key, _ = url.PathUnescape(key)
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r.URL.Query().Get("prefix"))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
			decoded, _ := url.PathUnescape(source)
			sourceKey := strings.TrimPrefix(strings.TrimPrefix(decoded, "/"), "nclip-test/")
			body, exists := f.objects[sourceKey]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.objects[key] = append([]byte(nil), body...)
			etag := fmt.Sprintf(`"%x"`, md5.Sum(body))
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `<CopyObjectResult><ETag>%s</ETag><LastModified>2026-11-25T00:00:00.000Z</LastModified></CopyObjectResult>`, etag)
			return
		}
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := f.objects[key]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>lock exists</Message></Error>`)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.objects[key] = body
		etag := fmt.Sprintf(`"%x"`, md5.Sum(body))
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, exists := f.objects[key]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodDelete:
		if match := r.Header.Get("If-Match"); match != "" {
			body, exists := f.objects[key]
			if !exists || match != fmt.Sprintf(`"%x"`, md5.Sum(body)) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeS3Objects) list(w http.ResponseWriter, prefix string) {
	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	f.mu.Unlock()
	sort.Strings(keys)
	var body strings.Builder
	body.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>nclip-test</Name><Prefix>`)
	_ = xml.EscapeText(&body, []byte(prefix))
	body.WriteString(`</Prefix><KeyCount>`)
	body.WriteString(fmt.Sprint(len(keys)))
	body.WriteString(`</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`)
	for _, key := range keys {
		body.WriteString(`<Contents><Key>`)
		_ = xml.EscapeText(&body, []byte(key))
		body.WriteString(`</Key><Size>1</Size><ETag>"x"</ETag></Contents>`)
	}
	body.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body.String())
}
