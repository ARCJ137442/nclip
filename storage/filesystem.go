package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/utils"
)

// errUnsafeID is returned when an id contains path traversal characters.
var errUnsafeID = fmt.Errorf("invalid paste id")

// safePath constructs a filesystem path for the given id under baseDir,
// returning an error if the resulting path escapes the base directory.
// This uses filepath.Clean to normalize the path and then verifies it
// stays within baseDir, which is the pattern recognised by CodeQL.
func safePath(baseDir, id string) (string, error) {
	// Reject obvious traversal characters early for clear error messages.
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		log.Printf("[ERROR] FS: unsafe id rejected: %q", id)
		return "", errUnsafeID
	}
	p := filepath.Join(baseDir, id)
	p = filepath.Clean(p)
	// Ensure the cleaned path is still within baseDir.
	if !strings.HasPrefix(p, filepath.Clean(baseDir)+string(os.PathSeparator)) {
		log.Printf("[ERROR] FS: path escapes base dir: %q", p)
		return "", errUnsafeID
	}
	return p, nil
}

// FilesystemStore stores paste metadata and content on the local filesystem.
type FilesystemStore struct {
	dataDir     string
	bufferSize  int
	mu          sync.Mutex
	writerMu    sync.Mutex
	writerToken string
}

// NewFilesystemStore creates a FilesystemStore for the given data directory.
// If dataDir is empty it defaults to "./data". The directory is created if it does not exist.
func NewFilesystemStore(dataDir string) (*FilesystemStore, error) {
	if dataDir == "" {
		dataDir = "./data"
	}
	// Check if the dataDir not exists and create it with logging
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		log.Printf("[INFO] Creating data directory: %s", dataDir)
		if err := os.MkdirAll(dataDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create data directory %s: %w", dataDir, err)
		}
	}
	return &FilesystemStore{
		dataDir:    dataDir,
		bufferSize: 4096,
	}, nil
}

// Store saves the paste metadata (JSON) to local filesystem
func (fs *FilesystemStore) Store(paste *models.Paste) error {
	metaPath, err := safePath(fs.dataDir, paste.ID+".json")
	if err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	metaData, err := json.MarshalIndent(paste, "", "  ")
	if err != nil {
		log.Printf("[ERROR] FS Store: failed to marshal metadata for %s: %v", paste.ID, err)
		return err
	}
	if err := os.WriteFile(metaPath, metaData, 0o644); err != nil { // #nosec G306 -- path sanitised by safePath
		log.Printf("[ERROR] FS Store: failed to write metadata for %s: %v", paste.ID, err)
		return err
	}
	return nil
}

func (fs *FilesystemStore) Get(id string) (*models.Paste, error) {
	metaPath, err := safePath(fs.dataDir, id+".json")
	if err != nil {
		return nil, err
	}
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	metaData, err := os.ReadFile(metaPath) // #nosec G304 -- path sanitised by safePath
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		log.Printf("[ERROR] FS Get: failed to read metadata for %s: %v", id, err)
		return nil, err
	}
	var paste models.Paste
	if err := json.Unmarshal(metaData, &paste); err != nil {
		log.Printf("[ERROR] Get: failed to unmarshal metadata for %s: %v", id, err)
		return nil, err
	}
	if paste.IsExpired() && !paste.IsEphemeral() {
		log.Printf("[WARN] FS Get: paste %s is expired", id)
		// Delete expired paste files directly (we already hold the mutex) so subsequent accesses are clean
		_ = os.Remove(contentPath)
		if err := os.Remove(metaPath); err != nil {
			log.Printf("[WARN] FS Get: failed to remove expired metadata for %s: %v", id, err)
		}
		return nil, ErrNotFound
	}
	return &paste, nil
}

func (fs *FilesystemStore) Exists(id string) (bool, error) {
	metaPath, err := safePath(fs.dataDir, id+".json")
	if err != nil {
		return false, err
	}
	pendingPath, err := safePath(fs.dataDir, ChannelContentTempPrefix+id+".pending")
	if err != nil {
		return false, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, err = os.Stat(metaPath)
	if err == nil {
		return true, nil
	}
	if !os.IsNotExist(err) {
		log.Printf("[ERROR] FS Exists: failed to stat metadata for %s: %v", id, err)
		return false, err
	}
	_, err = os.Stat(pendingPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		log.Printf("[ERROR] FS Exists: failed to stat pending channel content for %s: %v", id, err)
		return false, err
	}
	return true, nil
}

func (fs *FilesystemStore) Delete(id string) error {
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return err
	}
	metaPath, err := safePath(fs.dataDir, id+".json")
	if err != nil {
		return err
	}
	pendingPath, err := safePath(fs.dataDir, ChannelContentTempPrefix+id+".pending")
	if err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := os.Remove(contentPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete content: %w", err)
	}
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete metadata: %w", err)
	}
	if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete pending channel content: %w", err)
	}
	return nil
}

func (fs *FilesystemStore) IncrementReadCount(id string) error {
	metaPath, err := safePath(fs.dataDir, id+".json")
	if err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	metaData, err := os.ReadFile(metaPath) // #nosec G304 -- path sanitised by safePath
	if err != nil {
		log.Printf("[ERROR] FS IncrementReadCount: failed to read metadata for %s: %v", id, err)
		return err
	}
	var paste models.Paste
	if err := json.Unmarshal(metaData, &paste); err != nil {
		log.Printf("[ERROR] FS IncrementReadCount: failed to unmarshal metadata for %s: %v", id, err)
		return err
	}
	paste.ReadCount++
	newMeta, err := json.MarshalIndent(&paste, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, newMeta, 0o644); err != nil { // #nosec G306 -- path sanitised by safePath
		log.Printf("[ERROR] FS IncrementReadCount: failed to write metadata for %s: %v", id, err)
		return err
	}
	return nil
}

func (fs *FilesystemStore) StoreContent(id string, content []byte) error {
	return fs.StoreContentStream(id, bytes.NewReader(content), int64(len(content)))
}

// StoreContentStream writes to a private temporary file and renames it only
// after the full payload and size have been verified.
func (fs *FilesystemStore) StoreContentStream(id string, content io.Reader, size int64) error {
	return fs.storeContentStream(id, content, size, "."+id+".upload-*")
}

func (fs *FilesystemStore) StoreChannelContentStream(id string, content io.Reader, size int64) error {
	if !utils.IsValidSlug(id) {
		return errUnsafeID
	}
	stagePath, err := safePath(fs.dataDir, ChannelContentTempPrefix+id+".pending")
	if err != nil {
		return err
	}
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return err
	}
	metaPath, err := safePath(fs.dataDir, id+".json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(fs.dataDir, 0o755); err != nil {
		return err
	}
	fs.mu.Lock()
	for _, path := range []string{contentPath, metaPath} {
		if _, err := os.Stat(path); err == nil {
			fs.mu.Unlock()
			return ErrSlugReserved
		} else if !os.IsNotExist(err) {
			fs.mu.Unlock()
			return err
		}
	}
	file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	fs.mu.Unlock()
	if err != nil {
		if os.IsExist(err) {
			return ErrSlugReserved
		}
		return fmt.Errorf("reserve pending channel slug: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(stagePath)
	}
	written, err := io.Copy(file, io.LimitReader(content, size+1))
	if err != nil {
		cleanup()
		return err
	}
	if written != size {
		cleanup()
		return fmt.Errorf("content size mismatch: expected %d bytes, received %d", size, written)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(stagePath)
		return err
	}
	return nil
}

func (fs *FilesystemStore) CommitChannelContent(id string) error {
	if !utils.IsValidSlug(id) {
		return errUnsafeID
	}
	stagePath, err := safePath(fs.dataDir, ChannelContentTempPrefix+id+".pending")
	if err != nil {
		return err
	}
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return err
	}
	if err := os.Rename(stagePath, contentPath); err != nil {
		return fmt.Errorf("publish channel content: %w", err)
	}
	return nil
}

// SweepPendingChannelContent removes finalization states that did not finish.
// Pending markers remain until content and metadata are deleted, making a
// crash during cleanup safe to retry on the next startup.
func (fs *FilesystemStore) SweepPendingChannelContent() error {
	entries, err := os.ReadDir(fs.dataDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ChannelContentTempPrefix) {
			continue
		}
		if !strings.HasSuffix(name, ".pending") {
			if err := os.RemoveAll(filepath.Join(fs.dataDir, name)); err != nil {
				return fmt.Errorf("remove orphan channel staging object: %w", err)
			}
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, ChannelContentTempPrefix), ".pending")
		if !utils.IsValidSlug(id) {
			if err := os.RemoveAll(filepath.Join(fs.dataDir, name)); err != nil {
				return err
			}
			continue
		}
		paste, err := fs.Get(id)
		switch {
		case err == nil && paste.IsEphemeral():
			if err := fs.Delete(id); err != nil {
				return fmt.Errorf("remove orphan ephemeral channel content %s: %w", id, err)
			}
		case err == nil:
			// A normal share won a slug collision. Remove only the pending
			// channel object; the ordinary paste owns the final slug paths.
		case errors.Is(err, ErrNotFound):
			// Without matching metadata the final slug may belong to an
			// ordinary upload between its content and metadata writes.
		default:
			return fmt.Errorf("inspect pending channel content %s: %w", id, err)
		}
		if err := os.Remove(filepath.Join(fs.dataDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove orphan channel content marker: %w", err)
		}
	}
	return nil
}

func (fs *FilesystemStore) storeContentStream(id string, content io.Reader, size int64, tempPattern string) error {
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return err
	}
	if utils.IsDebugEnabled() {
		log.Printf("[DEBUG] StoreContentStream: id=%s, content_len=%d", id, size)
	}
	if err := os.MkdirAll(fs.dataDir, 0o755); err != nil {
		log.Printf("[ERROR] FS StoreContentStream: failed to create data directory %s: %v", fs.dataDir, err)
		return err
	}
	tmp, err := os.CreateTemp(fs.dataDir, tempPattern)
	if err != nil {
		return fmt.Errorf("create content temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	written, err := io.Copy(tmp, io.LimitReader(content, size+1))
	if err != nil {
		cleanup()
		log.Printf("[ERROR] FS StoreContentStream: failed to write content for %s: %v", id, err)
		return err
	}
	if written != size {
		cleanup()
		return fmt.Errorf("content size mismatch: expected %d bytes, received %d", size, written)
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !strings.HasPrefix(id, ChannelContentTempPrefix) {
		pendingPath, pathErr := safePath(fs.dataDir, ChannelContentTempPrefix+id+".pending")
		if pathErr != nil {
			_ = os.Remove(tmpPath)
			return pathErr
		}
		if _, statErr := os.Stat(pendingPath); statErr == nil {
			_ = os.Remove(tmpPath)
			return ErrSlugReserved
		} else if !os.IsNotExist(statErr) {
			_ = os.Remove(tmpPath)
			return statErr
		}
	}
	if err := os.Rename(tmpPath, contentPath); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("[ERROR] FS StoreContentStream: failed to publish content for %s: %v", id, err)
		return err
	}
	return nil
}

func (fs *FilesystemStore) GetContent(id string) ([]byte, error) {
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	data, err := os.ReadFile(contentPath) // #nosec G304 -- path sanitised by safePath
	if err != nil {
		log.Printf("[ERROR] FS GetContent: failed to read content for %s: %v", id, err)
		return nil, err
	}
	return data, nil
}

// OpenContent opens stored content without copying it into memory.
func (fs *FilesystemStore) OpenContent(id string) (io.ReadCloser, int64, error) {
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(contentPath) // #nosec G304 -- path sanitised by safePath
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

// StatContent reports whether content exists on disk and its size.
func (fs *FilesystemStore) StatContent(id string) (bool, int64, error) {
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return false, 0, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	st, err := os.Stat(contentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		log.Printf("[ERROR] FS StatContent: failed to stat content for %s: %v", id, err)
		return false, 0, err
	}
	return true, st.Size(), nil
}

// GetContentPrefix reads up to n bytes from the content file. If the file is
// smaller than n, it returns the full content.
func (fs *FilesystemStore) GetContentPrefix(id string, n int64) ([]byte, error) {
	contentPath, err := safePath(fs.dataDir, id)
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, err := os.Open(contentPath) // #nosec G304 -- path sanitised by safePath
	if err != nil {
		log.Printf("[ERROR] FS GetContentPrefix: failed to open content for %s: %v", id, err)
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			log.Printf("[WARN] FS GetContentPrefix: failed to close file for %s: %v", id, cerr)
		}
	}()
	// If n is small enough to allocate, read into buffer
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return buf[:read], nil
		}
		if err == io.ErrUnexpectedEOF {
			return buf[:read], nil
		}
		return nil, err
	}
	return buf[:read], nil
}

func (fs *FilesystemStore) Close() error {
	return fs.ReleaseChannelWriter()
}

func (fs *FilesystemStore) ReleaseChannelWriter() error {
	fs.writerMu.Lock()
	defer fs.writerMu.Unlock()
	if fs.writerToken == "" {
		return nil
	}
	lockPath := filepath.Join(fs.dataDir, ChannelWriterLockKey)
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		fs.writerToken = ""
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if string(contents) != fs.writerToken {
		fs.writerToken = ""
		return nil
	}
	err = os.Remove(lockPath)
	fs.writerToken = ""
	return err
}

// AcquireChannelWriter uses an exclusive marker in the shared data directory.
// A marker left by a crash intentionally keeps later instances fail closed;
// operators must verify no writer is alive before removing a stale marker.
func (fs *FilesystemStore) AcquireChannelWriter() error {
	fs.writerMu.Lock()
	defer fs.writerMu.Unlock()
	if fs.writerToken != "" {
		return fmt.Errorf("channel writer lock already held by this store")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("create channel writer token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	path := filepath.Join(fs.dataDir, ChannelWriterLockKey)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("acquire channel writer lock: %w", err)
	}
	if _, err := file.WriteString(token); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write channel writer lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("sync channel writer lock: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close channel writer lock: %w", err)
	}
	fs.writerToken = token
	return nil
}

func (fs *FilesystemStore) OwnsChannelWriter() bool {
	fs.writerMu.Lock()
	defer fs.writerMu.Unlock()
	if fs.writerToken == "" {
		return false
	}
	contents, err := os.ReadFile(filepath.Join(fs.dataDir, ChannelWriterLockKey))
	return err == nil && string(contents) == fs.writerToken
}

// ListPastes lists active metadata, excluding private account subdirectories.
func (fs *FilesystemStore) ListPastes() ([]models.Paste, error) {
	pastes, err := fs.listPastes(false)
	return pastes, err
}

// ListEphemeralPastes is reserved for channel startup/runtime cleanup. It is
// not the management listing and must never be exposed to ordinary callers.
func (fs *FilesystemStore) ListEphemeralPastes() ([]models.Paste, error) {
	entries, err := os.ReadDir(fs.dataDir)
	if err != nil {
		return nil, err
	}
	pastes := make([]models.Paste, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !utils.IsValidSlug(id) {
			continue
		}
		paste, err := fs.readMetadata(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if paste.IsEphemeral() {
			pastes = append(pastes, *paste)
		}
	}
	return pastes, nil
}

func (fs *FilesystemStore) listPastes(ephemeralOnly bool) ([]models.Paste, error) {
	entries, err := os.ReadDir(fs.dataDir)
	if err != nil {
		return nil, err
	}
	pastes := make([]models.Paste, 0, len(entries)/2)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !utils.IsValidSlug(id) {
			continue
		}
		paste, err := fs.Get(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if paste.IsEphemeral() != ephemeralOnly {
			continue
		}
		pastes = append(pastes, *paste)
	}
	return pastes, nil
}

func (fs *FilesystemStore) readMetadata(id string) (*models.Paste, error) {
	metaPath, err := safePath(fs.dataDir, id+".json")
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var paste models.Paste
	if err := json.Unmarshal(metaData, &paste); err != nil {
		return nil, err
	}
	return &paste, nil
}
