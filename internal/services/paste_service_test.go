package services

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

func TestReadPasteStreamDoesNotConsumeOnCallbackFailure(t *testing.T) {
	fs, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewPasteService(fs, &config.Config{})
	expires := time.Now().Add(time.Hour)
	paste := &models.Paste{ID: "burnstream", CreatedAt: time.Now(), ExpiresAt: &expires, Size: 5, ContentType: "text/plain", BurnAfterRead: true}
	if err := fs.StoreContent(paste.ID, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Store(paste); err != nil {
		t.Fatal(err)
	}

	interrupted := errors.New("interrupted")
	err = service.ReadPasteStream(paste.ID, true, func(_ *models.Paste, _ io.Reader, _ int64) error {
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("error = %v, want callback error", err)
	}
	if _, err := service.GetPaste(paste.ID); err != nil {
		t.Fatalf("failed callback consumed the paste: %v", err)
	}

	var copyBuffer bytes.Buffer
	if err := service.ReadPasteStream(paste.ID, true, func(_ *models.Paste, source io.Reader, size int64) error {
		_, err := io.CopyN(&copyBuffer, source, size)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if copyBuffer.String() != "hello" {
		t.Fatalf("content = %q", copyBuffer.String())
	}
	if _, err := service.GetPaste(paste.ID); err == nil {
		t.Fatal("successful callback did not consume the paste")
	}
}

func TestGetPasteDeletesExpired(t *testing.T) {
	// Setup temporary filesystem store
	dir, err := os.MkdirTemp("", "nclip-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("failed to remove temp dir: %v", err)
		}
	}()

	fs, err := storage.NewFilesystemStore(dir)
	if err != nil {
		t.Fatalf("failed to create filesystem store: %v", err)
	}
	service := NewPasteService(fs, &config.Config{})

	// Create an expired paste: TTL in the past
	slug := "expiredslug"
	paste := &models.Paste{
		ID:            slug,
		CreatedAt:     time.Now().Add(-48 * time.Hour),
		ExpiresAt:     func() *time.Time { t := time.Now().Add(-24 * time.Hour); return &t }(),
		Size:          5,
		ContentType:   "text/plain",
		BurnAfterRead: false,
	}

	// Store content and metadata directly via store
	if err := fs.StoreContent(slug, []byte("hello")); err != nil {
		t.Fatalf("failed to store content: %v", err)
	}
	if err := fs.Store(paste); err != nil {
		t.Fatalf("failed to store metadata: %v", err)
	}

	// Ensure file exists
	metaPath := filepath.Join(dir, slug+".json")
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		t.Fatalf("expected metadata file to exist before GetPaste")
	}

	// Call GetPaste - it should delete expired paste and return an error
	_, err = service.GetPaste(slug)
	if err == nil {
		t.Fatalf("expected error retrieving expired paste, got nil")
	}

	// Confirm metadata and content files are removed
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("expected metadata file to be removed after expired access, still exists")
	}
	contentPath := filepath.Join(dir, slug)
	if _, err := os.Stat(contentPath); !os.IsNotExist(err) {
		t.Fatalf("expected content file to be removed after expired access, still exists")
	}
}

func TestDeleteBurnAfterRead(t *testing.T) {
	// Setup temporary filesystem store
	dir, err := os.MkdirTemp("", "nclip-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("failed to remove temp dir: %v", err)
		}
	}()

	fs, err := storage.NewFilesystemStore(dir)
	if err != nil {
		t.Fatalf("failed to create filesystem store: %v", err)
	}
	service := NewPasteService(fs, &config.Config{})

	// Create a burn-after-read paste with TTL in the future
	slug := "burnslug"
	expires := time.Now().Add(24 * time.Hour)
	paste := &models.Paste{
		ID:            slug,
		CreatedAt:     time.Now(),
		ExpiresAt:     &expires,
		Size:          5,
		ContentType:   "text/plain",
		BurnAfterRead: true,
	}

	if err := fs.StoreContent(slug, []byte("data")); err != nil {
		t.Fatalf("failed to store content: %v", err)
	}
	if err := fs.Store(paste); err != nil {
		t.Fatalf("failed to store metadata: %v", err)
	}

	// Simulate access: service.GetPaste and then service.DeletePaste (handler behavior)
	p, err := service.GetPaste(slug)
	if err != nil {
		t.Fatalf("unexpected error retrieving paste: %v", err)
	}
	if !p.BurnAfterRead {
		t.Fatalf("expected paste BurnAfterRead true")
	}

	// Simulate handler deleting paste after serving
	if err := service.DeletePaste(slug); err != nil {
		t.Fatalf("failed to delete burn-after-read paste: %v", err)
	}

	// Ensure files removed
	metaPath := filepath.Join(dir, slug+".json")
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("expected metadata removed after delete, still exists")
	}
}
