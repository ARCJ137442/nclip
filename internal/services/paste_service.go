package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"sync"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
	"github.com/johnwmail/nclip/utils"
)

// PasteService handles paste business logic
type PasteService struct {
	store  storage.PasteStore
	config *config.Config
	// All destructive readers in this process share these bounded lock stripes.
	// This is a single-instance guarantee, not a distributed S3 transaction.
	consumeLocks [256]sync.Mutex
}

// ChannelOrphanTTL is a private crash-cleanup bound for temporary channel
// content. Active channel content is exempt; this is not a product TTL.
const ChannelOrphanTTL = 168 * time.Hour

// NewPasteService creates a new paste service
func NewPasteService(store storage.PasteStore, config *config.Config) *PasteService {
	return &PasteService{
		store:  store,
		config: config,
	}
}

// CreatePasteRequest represents a request to create a paste
type CreatePasteRequest struct {
	Content            []byte
	Filename           string
	ContentType        string
	CustomSlug         string
	BurnAfterRead      bool
	TTL                time.Duration
	Share              *models.ShareManifest
	Public             bool
	EphemeralChannelID string
}

// CreatePasteStreamRequest is the bounded-memory form of CreatePasteRequest.
type CreatePasteStreamRequest struct {
	Content            io.Reader
	Size               int64
	ContentPrefix      []byte
	Filename           string
	ContentType        string
	CustomSlug         string
	BurnAfterRead      bool
	TTL                time.Duration
	Share              *models.ShareManifest
	Public             bool
	EphemeralChannelID string
}

// CreatePasteResponse represents the response from creating a paste
type CreatePasteResponse struct {
	Slug         string
	URL          string
	ExpiresAt    time.Time
	Size         int64
	MetadataSize int64
}

// GenerateSlug generates a unique slug for a paste
func (s *PasteService) GenerateSlug() (string, error) {
	batchSize := 5
	lengths := []int{5, 6, 7}
	var slug string
	for _, length := range lengths {
		candidates, err := utils.GenerateSlugBatch(batchSize, length)
		if err != nil {
			return "", fmt.Errorf("failed to generate slug batch: %w", err)
		}
		for _, candidate := range candidates {
			exists, err := s.store.Exists(candidate)
			if err != nil {
				continue // skip on error
			}
			if !exists {
				slug = candidate
				return slug, nil
			}
			// exists, check if expired
			existing, err := s.store.Get(candidate)
			if err != nil {
				continue
			}
			if existing == nil || !existing.IsEphemeral() && existing.IsExpired() {
				slug = candidate
				return slug, nil
			}
		}
	}
	return "", fmt.Errorf("failed to generate unique slug after 3 batches")
}

// ValidateCustomSlug validates and checks if a custom slug is available
func (s *PasteService) ValidateCustomSlug(slug string) error {
	if !utils.IsValidSlug(slug) {
		return fmt.Errorf("invalid slug format")
	}

	exists, err := s.store.Exists(slug)
	if err != nil {
		return fmt.Errorf("failed to check slug existence: %w", err)
	}
	if exists {
		existing, err := s.store.Get(slug)
		if err != nil {
			return fmt.Errorf("failed to retrieve existing paste: %w", err)
		}
		if existing != nil && (existing.IsEphemeral() || !existing.IsExpired()) {
			return fmt.Errorf("slug already exists")
		}
	}
	return nil
}

// CreatePaste creates a new paste
func (s *PasteService) CreatePaste(req CreatePasteRequest) (*CreatePasteResponse, error) {
	return s.CreatePasteStream(CreatePasteStreamRequest{
		Content:            bytes.NewReader(req.Content),
		Size:               int64(len(req.Content)),
		ContentPrefix:      req.Content,
		Filename:           req.Filename,
		ContentType:        req.ContentType,
		CustomSlug:         req.CustomSlug,
		BurnAfterRead:      req.BurnAfterRead,
		TTL:                req.TTL,
		Share:              req.Share,
		Public:             req.Public,
		EphemeralChannelID: req.EphemeralChannelID,
	})
}

// CreatePasteStream creates a paste from a bounded-memory content stream.
func (s *PasteService) CreatePasteStream(req CreatePasteStreamRequest) (*CreatePasteResponse, error) {
	if req.Content == nil {
		return nil, fmt.Errorf("content is required")
	}
	if req.Size < 0 {
		return nil, fmt.Errorf("invalid content size: %d", req.Size)
	}
	if req.EphemeralChannelID != "" {
		if req.Public || req.BurnAfterRead {
			return nil, fmt.Errorf("invalid ephemeral paste policy")
		}
		req.TTL = ChannelOrphanTTL
	}
	var slug string
	var err error

	if req.CustomSlug != "" {
		if err := s.ValidateCustomSlug(req.CustomSlug); err != nil {
			return nil, err
		}
		slug = req.CustomSlug
	} else {
		slug, err = s.GenerateSlug()
		if err != nil {
			return nil, fmt.Errorf("failed to generate slug: %w", err)
		}
	}

	createdAt := time.Now()
	expiresAt := createdAt.Add(req.TTL)

	contentType := req.ContentType
	if contentType == "" {
		contentType = utils.DetectContentType(req.Filename, req.ContentPrefix)
	}
	paste := &models.Paste{
		ID:                 slug,
		CreatedAt:          createdAt,
		ExpiresAt:          &expiresAt,
		Size:               req.Size,
		ContentType:        contentType,
		BurnAfterRead:      req.BurnAfterRead,
		ReadCount:          0,
		Share:              req.Share,
		Public:             req.Public,
		EphemeralChannelID: req.EphemeralChannelID,
	}
	var metadataSize int64
	if paste.IsEphemeral() {
		metadata, err := json.MarshalIndent(paste, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to encode paste metadata: %w", err)
		}
		metadataSize = int64(len(metadata))
	}

	var storeErr error
	var channelContentStore storage.ChannelContentStore
	if req.EphemeralChannelID != "" {
		if channelStore, ok := s.store.(storage.ChannelContentStore); ok {
			channelContentStore = channelStore
			storeErr = channelContentStore.StoreChannelContentStream(slug, req.Content, req.Size)
		} else {
			storeErr = s.store.StoreContentStream(slug, req.Content, req.Size)
		}
	} else {
		storeErr = s.store.StoreContentStream(slug, req.Content, req.Size)
	}
	if storeErr != nil {
		return nil, fmt.Errorf("failed to store content: %w", storeErr)
	}
	if err := s.store.Store(paste); err != nil {
		if cleanupErr := s.store.Delete(slug); cleanupErr != nil {
			return nil, fmt.Errorf("failed to store metadata: %w; cleanup failed: %v", err, cleanupErr)
		}
		return nil, fmt.Errorf("failed to store metadata: %w", err)
	}
	if channelContentStore != nil {
		if err := channelContentStore.CommitChannelContent(slug); err != nil {
			if cleanupErr := s.store.Delete(slug); cleanupErr != nil {
				return nil, fmt.Errorf("failed to publish channel content: %w; cleanup failed: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("failed to publish channel content: %w", err)
		}
	}

	return &CreatePasteResponse{
		Slug:         slug,
		URL:          "", // Will be set by handler based on request context
		ExpiresAt:    expiresAt,
		Size:         req.Size,
		MetadataSize: metadataSize,
	}, nil
}

// ConsumePaste reads and validates a complete paste, then deletes a burn paste
// before handing its bytes to the HTTP layer. A lost response cannot be replayed.
func (s *PasteService) ConsumePaste(slug string) (*models.Paste, []byte, error) {
	var paste *models.Paste
	var content bytes.Buffer
	err := s.ReadPasteStream(slug, true, func(current *models.Paste, source io.Reader, size int64) error {
		paste = current
		_, copyErr := io.CopyN(&content, source, size)
		return copyErr
	})
	if err != nil {
		return nil, nil, err
	}
	return paste, content.Bytes(), nil
}

// ReadPasteStream lets handlers consume a validated stream without loading it
// into memory. For burn pastes the deletion happens under the same in-process
// lock and only after the callback succeeds.
func (s *PasteService) ReadPasteStream(slug string, consume bool, read func(*models.Paste, io.Reader, int64) error) error {
	h := fnv.New32a()
	_, _ = h.Write([]byte(slug))
	lock := &s.consumeLocks[h.Sum32()%uint32(len(s.consumeLocks))]
	if consume {
		lock.Lock()
		defer lock.Unlock()
	}
	paste, err := s.GetPaste(slug)
	if err != nil {
		return err
	}
	content, size, err := s.OpenPasteContent(slug)
	if err != nil {
		return err
	}
	if size != paste.Size {
		_ = content.Close()
		return fmt.Errorf("size_mismatch")
	}
	readErr := read(paste, content, size)
	closeErr := content.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if paste.BurnAfterRead {
		if err := s.store.Delete(slug); err != nil {
			return fmt.Errorf("consume delete: %w", err)
		}
	}
	return nil
}

// GetPaste retrieves a paste by slug
func (s *PasteService) GetPaste(slug string) (*models.Paste, error) {
	paste, err := s.store.Get(slug)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve paste: %w", err)
	}
	if paste == nil {
		return nil, storage.ErrNotFound
	}
	if paste.IsExpired() && !paste.IsEphemeral() {
		// Log a warning that an expired paste was requested and attempt deletion.
		// We keep returning not-found semantics (handlers will map to 404) but
		// emit a WARN so the event is visible in logs.
		if err := s.store.Delete(slug); err != nil {
			return nil, fmt.Errorf("paste expired (failed to delete expired paste: %w)", err)
		}
		return nil, fmt.Errorf("paste expired")
	}

	return paste, nil
}

// GetPasteContent retrieves paste content
func (s *PasteService) GetPasteContent(slug string) ([]byte, error) {
	content, err := s.store.GetContent(slug)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve content: %w", err)
	}
	return content, nil
}

// OpenPasteContent returns a streaming reader and its authoritative size.
func (s *PasteService) OpenPasteContent(slug string) (io.ReadCloser, int64, error) {
	content, size, err := s.store.OpenContent(slug)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open content: %w", err)
	}
	if content == nil {
		return nil, 0, fmt.Errorf("failed to open content: content not found")
	}
	return content, size, nil
}

// IncrementReadCount increments the read count for a paste
func (s *PasteService) IncrementReadCount(slug string) error {
	return s.store.IncrementReadCount(slug)
}

// DeletePaste deletes a paste
func (s *PasteService) DeletePaste(slug string) error {
	return s.store.Delete(slug)
}
