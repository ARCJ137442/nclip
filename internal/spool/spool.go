// Package spool provides bounded-memory temporary storage for request bodies.
package spool

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrTooLarge means the source contained more bytes than the caller allowed.
var ErrTooLarge = errors.New("content too large")

// File is a temporary file positioned at its beginning.
type File struct {
	*os.File
	Path string
	Size int64
	Head []byte
}

// Write copies at most limit bytes into a new temporary file. It reads one
// extra byte to distinguish an exact-limit payload from an oversized one.
func Write(dir, pattern string, source io.Reader, limit int64, headSize int) (*File, error) {
	if source == nil {
		return nil, errors.New("nil reader")
	}
	if limit < 0 {
		return nil, fmt.Errorf("invalid byte limit: %d", limit)
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}

	buf := make([]byte, 32*1024)
	size, err := io.CopyBuffer(file, io.LimitReader(source, limit+1), buf)
	if err != nil {
		cleanup()
		return nil, err
	}
	if size > limit {
		cleanup()
		return nil, ErrTooLarge
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	head, err := readHead(file, size, headSize)
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return &File{File: file, Path: path, Size: size, Head: head}, nil
}

func readHead(file *os.File, size int64, headSize int) ([]byte, error) {
	if headSize <= 0 || size == 0 {
		return nil, nil
	}
	want := int64(headSize)
	if size < want {
		want = size
	}
	head := make([]byte, want)
	if _, err := io.ReadFull(file, head); err != nil {
		return nil, err
	}
	return head, nil
}

// Remove closes and deletes the temporary file. It is safe to call repeatedly.
func (f *File) Remove() error {
	if f == nil {
		return nil
	}
	var closeErr error
	if f.File != nil {
		closeErr = f.Close()
		f.File = nil
	}
	removeErr := os.Remove(f.Path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return closeErr
	}
	return removeErr
}
