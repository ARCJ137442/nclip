package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultUploadLimitBytes      int64 = 5 * 1024 * 1024
	MaxUploadLimitBytes          int64 = 100 * 1024 * 1024
	MultipartOverheadBytes       int64 = 64 * 1024
	MiB                                = 1024 * 1024
)

// UploadLimit stores the administrator's content quota and its service ceiling.
// The settings file is shared only by the single supported application writer.
type UploadLimit struct {
	mu       sync.RWMutex
	path     string
	value    int64
	ceiling  int64
	revision uint64
}

type uploadLimitFile struct {
	Version  int   `json:"version"`
	Bytes    int64 `json:"bytes"`
	Revision uint64 `json:"revision"`
}

// OpenUploadLimit loads the persisted quota or starts with the configured default.
func OpenUploadLimit(dataDir string, initial, ceiling int64) (*UploadLimit, error) {
	if initial < MiB || ceiling < MiB || initial > ceiling || ceiling > MaxUploadLimitBytes {
		return nil, fmt.Errorf("invalid upload limit bounds: initial=%d ceiling=%d", initial, ceiling)
	}
	dir := filepath.Join(dataDir, ".settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create upload settings directory: %w", err)
	}
	limit := &UploadLimit{path: filepath.Join(dir, "upload-limit.json"), value: initial, ceiling: ceiling}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect upload settings directory: %w", err)
	}
	data, err := os.ReadFile(limit.path)
	if errors.Is(err, os.ErrNotExist) {
		return limit, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read upload limit settings: %w", err)
	}
	var saved uploadLimitFile
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode upload limit settings: %w", err)
	}
	if saved.Version != 1 || saved.Bytes < MiB || saved.Bytes > ceiling || saved.Bytes%MiB != 0 || saved.Revision == 0 {
		return nil, fmt.Errorf("invalid persisted upload limit settings")
	}
	limit.value = saved.Bytes
	limit.revision = saved.Revision
	return limit, nil
}

// Snapshot returns the current user quota, service ceiling, and revision.
func (s *UploadLimit) Snapshot() (value, ceiling int64, revision uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value, s.ceiling, s.revision
}

// Set validates, persists atomically, then publishes the new limit to readers.
func (s *UploadLimit) Set(value int64, expectedRevision uint64) (revision uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value < MiB || value > s.ceiling || value%MiB != 0 {
		return s.revision, fmt.Errorf("upload_limit_out_of_range")
	}
	if expectedRevision != s.revision {
		return s.revision, fmt.Errorf("upload_limit_changed")
	}
	if value == s.value && s.revision > 0 {
		return s.revision, nil
	}
	next := uploadLimitFile{Version: 1, Bytes: value, Revision: s.revision + 1}
	data, err := json.Marshal(next)
	if err != nil {
		return s.revision, fmt.Errorf("encode upload limit settings: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".upload-limit-*.tmp")
	if err != nil {
		return s.revision, fmt.Errorf("create upload limit settings temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return s.revision, fmt.Errorf("protect upload limit settings: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return s.revision, fmt.Errorf("write upload limit settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return s.revision, fmt.Errorf("sync upload limit settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return s.revision, fmt.Errorf("close upload limit settings: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return s.revision, fmt.Errorf("replace upload limit settings: %w", err)
	}
	s.value = value
	s.revision = next.Revision
	return s.revision, nil
}
