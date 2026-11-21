package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemStoreStreamsContentAndPublishesOnlyCompleteFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("stream"), 4096)
	if err := store.StoreContentStream("STREAM", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	reader, size, err := store.OpenContent("STREAM")
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
	actual, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, content) {
		t.Fatal("streamed content changed")
	}

	err = store.StoreContentStream("SHORT", bytes.NewReader([]byte("short")), 10)
	if err == nil {
		t.Fatal("short stream was accepted")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "SHORT")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("incomplete content was published: %v", statErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "STREAM" {
			t.Fatalf("temporary file was left behind: %s", entry.Name())
		}
	}
}
