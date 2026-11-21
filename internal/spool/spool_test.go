package spool

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

func TestWriteKeepsExactContentAndHead(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("abcdef"), 4096)
	file, err := Write(dir, "test-*", bytes.NewReader(content), int64(len(content)), 512)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Remove() })
	if file.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", file.Size, len(content))
	}
	if !bytes.Equal(file.Head, content[:512]) {
		t.Fatal("head does not match the beginning of the content")
	}
	actual, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, content) {
		t.Fatal("spooled content changed")
	}
}

func TestWriteRejectsOversizedContentAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	_, err := Write(dir, "test-*", bytes.NewReader(make([]byte, 33)), 32, 8)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized spool left %d temporary files", len(entries))
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	file, err := Write(t.TempDir(), "test-*", bytes.NewReader([]byte("x")), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	path := file.Path
	if err := file.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file still exists: %v", err)
	}
	if err := file.Remove(); err != nil {
		t.Fatalf("second Remove failed: %v", err)
	}
}
