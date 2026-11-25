package channels

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnwmail/nclip/models"
)

const (
	ContentEmpty               = "empty"
	ContentUploading           = "uploading"
	ContentHeld                = "held"
	ContentAborted             = "aborted"
	MaxContentChunkBytes       = 4 << 20
	ContentInitReplayRetention = 2 * time.Minute
	MaxContentInitRecords      = 8192
)

var (
	ErrContentConflict   = errors.New("content_conflict")
	ErrContentNotReady   = errors.New("content_capacity_unavailable")
	ErrContentTooLarge   = errors.New("content_too_large")
	ErrContentChunk      = errors.New("invalid_content_chunk")
	ErrContentOffset     = errors.New("content_offset_conflict")
	ErrContentClosed     = errors.New("content_upload_closed")
	ErrContentIncomplete = errors.New("content_incomplete")
	ErrContentBusy       = errors.New("content_finalize_in_progress")
)

type contentCapacity struct {
	ready      bool
	floor      int64
	budget     int64
	blockSize  int64
	held       int64
	reserved   int64
	written    int64
	heldBySlug map[string]int64
}

type contentInitReplay struct {
	session   *ContentUpload
	expiresAt time.Time
}

type contentPart struct {
	size   int64
	offset int64
	path   string
	chunks map[int64]chunkReceipt
}

type chunkReceipt struct {
	size int64
	hash [sha256.Size]byte
}

// ContentUpload is process-local state. Its manifest and path are never
// returned by the API; only the sender's upload ID is exposed to its owner.
type ContentUpload struct {
	ID             string
	ChannelID      string
	RequestID      string
	ManifestHash   [sha256.Size]byte
	Manifest       models.ShareManifest
	TotalBytes     int64
	EffectiveLimit int64
	Reservation    int64
	Written        int64
	Directory      string
	State          string
	FinalizeKey    string
	Slug           string
	HeldBytes      int64
	mu             sync.Mutex
	aborted        atomic.Bool
	parts          map[string]*contentPart
}

type ContentInitResult struct {
	UploadID string
	State    string
	Replayed bool
}

type ContentRelease struct {
	ChannelID string
	Slug      string
	HeldBytes int64
	Upload    *ContentUpload
}

func (m *Manager) InitializeContentCapacity(freeBytes, blockSize int64) error {
	if freeBytes < 0 || blockSize <= 0 {
		return ErrContentNotReady
	}
	floor := freeBytes / 10
	if freeBytes%10 != 0 {
		floor++
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.channels) != 0 || len(m.contentUploads) != 0 {
		return ErrContentConflict
	}
	m.content = contentCapacity{
		ready:      true,
		floor:      floor,
		budget:     freeBytes - floor,
		blockSize:  blockSize,
		heldBySlug: make(map[string]int64),
	}
	return nil
}

func (m *Manager) HeldPhysicalBytes(contentBytes, metadataBytes int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	blockSize := m.content.blockSize
	if !m.content.ready || blockSize <= 0 || contentBytes < 0 || metadataBytes < 0 {
		return 0, ErrContentNotReady
	}
	round := func(size int64) (int64, error) {
		if size == 0 {
			return 0, nil
		}
		blocks := size / blockSize
		if size%blockSize != 0 {
			blocks++
		}
		if blocks > math.MaxInt64/blockSize {
			return 0, ErrCapacity
		}
		return blocks * blockSize, nil
	}
	content, err := round(contentBytes)
	if err != nil {
		return 0, err
	}
	metadata, err := round(metadataBytes)
	if err != nil || content > math.MaxInt64-metadata {
		return 0, ErrCapacity
	}
	return content + metadata, nil
}

func (m *Manager) BeginContent(channelID, senderKey, requestID string, manifest models.ShareManifest, manifestHash [sha256.Size]byte, total, limit, reservation, freeBytes int64, directory string, now time.Time) (ContentInitResult, error) {
	if !ValidRequestID(requestID) {
		return ContentInitResult{}, ErrInvalidRequestID
	}
	if directory == "" {
		return ContentInitResult{}, ErrContentNotReady
	}
	key := contentInitKey(channelID, requestID)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneContentInitsLocked(now)
	record, ok := m.channels[channelID]
	if !ok {
		return ContentInitResult{}, ErrChannelNotFound
	}
	if !matchHash(record.senderHash, senderKey) {
		return ContentInitResult{}, ErrUnauthorized
	}
	if current := record.contentUpload; current != nil && current.RequestID == requestID {
		if current.ManifestHash != manifestHash {
			return ContentInitResult{}, ErrIdempotencyConflict
		}
		return contentInitResult(current, true), nil
	}
	if prior, exists := m.contentInits[key]; exists {
		if prior.session.ManifestHash != manifestHash {
			return ContentInitResult{}, ErrIdempotencyConflict
		}
		return contentInitResult(prior.session, true), nil
	}
	if total <= 0 || limit <= 0 || total > limit {
		return ContentInitResult{}, ErrContentTooLarge
	}
	if !m.content.ready || reservation <= 0 {
		return ContentInitResult{}, ErrContentNotReady
	}
	if record.Seat == nil || record.Seat.State != "paired" {
		return ContentInitResult{}, ErrContentConflict
	}
	if record.ContentState != "" && record.ContentState != ContentEmpty {
		return ContentInitResult{}, ErrContentConflict
	}
	if _, cleanupPending := m.pendingContentReleases[channelID]; cleanupPending {
		return ContentInitResult{}, ErrContentConflict
	}
	if len(m.contentInits) >= MaxContentInitRecords {
		return ContentInitResult{}, ErrCapacity
	}
	if reservation > m.content.budget-m.content.held-m.content.reserved {
		return ContentInitResult{}, ErrContentNotReady
	}
	unmaterialized := m.content.reserved - m.content.written
	if unmaterialized < 0 || reservation > math.MaxInt64-unmaterialized || m.content.floor > math.MaxInt64-unmaterialized-reservation || freeBytes < m.content.floor+unmaterialized+reservation {
		return ContentInitResult{}, ErrContentNotReady
	}
	uploadID, err := NewRequestID()
	if err != nil {
		return ContentInitResult{}, err
	}
	parts := make(map[string]*contentPart)
	for _, item := range manifest.Items {
		for _, part := range item.Parts {
			if !validContentPartKey(part.Key) || part.Size <= 0 {
				return ContentInitResult{}, ErrContentChunk
			}
			if _, duplicate := parts[part.Key]; duplicate {
				return ContentInitResult{}, ErrContentChunk
			}
			parts[part.Key] = &contentPart{
				size:   part.Size,
				path:   filepath.Join(directory, part.Key+".part"),
				chunks: make(map[int64]chunkReceipt),
			}
		}
	}
	session := &ContentUpload{
		ID:             uploadID,
		ChannelID:      channelID,
		RequestID:      requestID,
		ManifestHash:   manifestHash,
		Manifest:       manifest,
		TotalBytes:     total,
		EffectiveLimit: limit,
		Reservation:    reservation,
		Directory:      directory,
		State:          ContentUploading,
		parts:          parts,
	}
	record.ContentState = ContentUploading
	record.contentUpload = session
	m.channels[channelID] = record
	m.contentUploads[uploadID] = session
	m.content.reserved += reservation
	m.contentInits[key] = contentInitReplay{session: session, expiresAt: now.Add(ContentInitReplayRetention)}
	return contentInitResult(session, false), nil
}

func contentInitKey(channelID, requestID string) string {
	return channelID + "|" + requestID
}

func contentInitResult(session *ContentUpload, replayed bool) ContentInitResult {
	return ContentInitResult{UploadID: session.ID, State: session.State, Replayed: replayed}
}

func (m *Manager) ReplayContentInit(channelID, senderKey, requestID string, manifestHash [sha256.Size]byte, now time.Time) (ContentInitResult, bool, error) {
	if !ValidRequestID(requestID) {
		return ContentInitResult{}, false, ErrInvalidRequestID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneContentInitsLocked(now)
	record, ok := m.channels[channelID]
	if !ok {
		return ContentInitResult{}, false, ErrChannelNotFound
	}
	if !matchHash(record.senderHash, senderKey) {
		return ContentInitResult{}, false, ErrUnauthorized
	}
	if session := record.contentUpload; session != nil && session.RequestID == requestID {
		if session.ManifestHash != manifestHash {
			return ContentInitResult{}, false, ErrIdempotencyConflict
		}
		return contentInitResult(session, true), true, nil
	}
	if prior, exists := m.contentInits[contentInitKey(channelID, requestID)]; exists {
		if prior.session.ManifestHash != manifestHash {
			return ContentInitResult{}, false, ErrIdempotencyConflict
		}
		return contentInitResult(prior.session, true), true, nil
	}
	return ContentInitResult{}, false, nil
}

func validContentPartKey(key string) bool {
	if len(key) == 0 || len(key) > 40 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return key != "manifest" && key != "archive"
}

func (m *Manager) ContentSession(channelID, senderKey, uploadID string) (*ContentUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[channelID]
	if !ok {
		return nil, ErrChannelNotFound
	}
	if !matchHash(record.senderHash, senderKey) {
		return nil, ErrUnauthorized
	}
	session := m.contentUploads[uploadID]
	if session == nil || session.ChannelID != channelID || session.aborted.Load() || record.contentUpload != session {
		return nil, ErrContentClosed
	}
	return session, nil
}

func (m *Manager) AppendContentChunk(channelID, senderKey, uploadID, partKey string, offset int64, data []byte, expectedHash [sha256.Size]byte) (bool, error) {
	session, err := m.ContentSession(channelID, senderKey, uploadID)
	if err != nil {
		return false, err
	}
	if len(data) == 0 || len(data) > MaxContentChunkBytes || sha256.Sum256(data) != expectedHash {
		return false, ErrContentChunk
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.aborted.Load() {
		return false, ErrContentClosed
	}
	m.mu.Lock()
	record, ok := m.channels[channelID]
	if !ok || record.contentUpload != session || session.State != ContentUploading {
		m.mu.Unlock()
		return false, ErrContentClosed
	}
	if !matchHash(record.senderHash, senderKey) {
		m.mu.Unlock()
		return false, ErrUnauthorized
	}
	part := session.parts[partKey]
	if part == nil {
		m.mu.Unlock()
		return false, ErrContentChunk
	}
	if prior, exists := part.chunks[offset]; exists {
		m.mu.Unlock()
		if prior.size == int64(len(data)) && subtle.ConstantTimeCompare(prior.hash[:], expectedHash[:]) == 1 {
			return true, nil
		}
		return false, ErrIdempotencyConflict
	}
	if offset != part.offset || offset < 0 || offset%MaxContentChunkBytes != 0 || int64(len(data)) > part.size-offset {
		m.mu.Unlock()
		return false, ErrContentOffset
	}
	expectedSize := min(int64(MaxContentChunkBytes), part.size-offset)
	if int64(len(data)) != expectedSize {
		m.mu.Unlock()
		return false, ErrContentChunk
	}
	partPath := part.path
	m.mu.Unlock()

	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	written, writeErr := file.WriteAt(data, offset)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Truncate(partPath, offset)
		if writeErr != nil {
			return false, writeErr
		}
		return false, closeErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok = m.channels[channelID]
	if session.aborted.Load() || !ok || record.contentUpload != session || session.State != ContentUploading {
		_ = os.Truncate(partPath, offset)
		return false, ErrContentClosed
	}
	part.offset += int64(len(data))
	part.chunks[offset] = chunkReceipt{size: int64(len(data)), hash: expectedHash}
	session.Written += int64(len(data))
	m.content.written += int64(len(data))
	return false, nil
}

func (m *Manager) BeginContentFinalize(channelID, senderKey, uploadID, requestID string) (*ContentUpload, string, bool, error) {
	if !ValidRequestID(requestID) {
		return nil, "", false, ErrInvalidRequestID
	}
	session, err := m.ContentSession(channelID, senderKey, uploadID)
	if err != nil {
		return nil, "", false, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.aborted.Load() {
		return nil, "", false, ErrContentClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[channelID]
	if !ok || record.contentUpload != session {
		return nil, "", false, ErrContentClosed
	}
	if !matchHash(record.senderHash, senderKey) {
		return nil, "", false, ErrUnauthorized
	}
	if session.State == ContentHeld {
		if session.FinalizeKey != requestID {
			return nil, "", false, ErrIdempotencyConflict
		}
		return session, session.Slug, true, nil
	}
	if session.State == "finalizing" {
		if session.FinalizeKey != requestID {
			return nil, "", false, ErrIdempotencyConflict
		}
		return nil, "", false, ErrContentBusy
	}
	if session.State != ContentUploading {
		return nil, "", false, ErrContentClosed
	}
	if session.TotalBytes <= 0 || session.TotalBytes > session.EffectiveLimit {
		return nil, "", false, ErrContentTooLarge
	}
	if session.FinalizeKey != "" && session.FinalizeKey != requestID {
		return nil, "", false, ErrIdempotencyConflict
	}
	for _, part := range session.parts {
		if part.offset != part.size {
			return nil, "", false, ErrContentIncomplete
		}
	}
	if record.Seat == nil || record.Seat.State != "paired" {
		return nil, "", false, ErrContentConflict
	}
	session.FinalizeKey = requestID
	session.State = "finalizing"
	return session, "", false, nil
}

func (m *Manager) CompleteContentFinalize(channelID, senderKey string, session *ContentUpload, slug string, heldBytes int64) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[channelID]
	if !ok || record.contentUpload != session || session.aborted.Load() || session.State != "finalizing" {
		return ErrContentClosed
	}
	if !matchHash(record.senderHash, senderKey) {
		return ErrUnauthorized
	}
	if record.Seat == nil || record.Seat.State != "paired" {
		return ErrContentConflict
	}
	if heldBytes < 0 || heldBytes > session.Reservation || session.Reservation > m.content.reserved || session.Written > m.content.written {
		return ErrCapacity
	}
	m.content.reserved -= session.Reservation
	m.content.written -= session.Written
	m.content.held += heldBytes
	m.content.heldBySlug[slug] = heldBytes
	session.Reservation = 0
	session.Written = 0
	session.State = ContentHeld
	session.Slug = slug
	session.HeldBytes = heldBytes
	session.Manifest = models.ShareManifest{}
	session.parts = nil
	record.ContentState = ContentHeld
	record.contentSlug = slug
	record.contentHeld = heldBytes
	record.contentBytes = session.TotalBytes
	m.channels[channelID] = record
	return nil
}

func (m *Manager) ResetContentFinalize(session *ContentUpload) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.channels[session.ChannelID]; ok && current.contentUpload == session && session.State == "finalizing" && !session.aborted.Load() {
		session.State = ContentUploading
		m.channels[session.ChannelID] = current
	}
}

func (m *Manager) ContentActive(slug string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, record := range m.channels {
		if record.contentSlug == slug && record.ContentState == ContentHeld {
			return true
		}
	}
	return false
}

func (m *Manager) ReleaseHeldContent(slug string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, exists := m.content.heldBySlug[slug]
	if !exists {
		return
	}
	m.content.held -= held
	delete(m.content.heldBySlug, slug)
	for channelID, release := range m.pendingContentReleases {
		if release.Slug == slug {
			delete(m.pendingContentReleases, channelID)
		}
	}
}

func (m *Manager) PendingContentReleases() []ContentRelease {
	m.mu.Lock()
	defer m.mu.Unlock()
	releases := make([]ContentRelease, 0, len(m.pendingContentReleases))
	for _, release := range m.pendingContentReleases {
		releases = append(releases, release)
	}
	return releases
}

func (m *Manager) pruneContentInitsLocked(now time.Time) {
	for key, result := range m.contentInits {
		if !result.expiresAt.After(now) {
			delete(m.contentInits, key)
		}
	}
}

func (u *ContentUpload) WithPartFiles(run func(map[string]string) error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.aborted.Load() {
		return ErrContentClosed
	}
	files := make(map[string]string, len(u.parts))
	for key, part := range u.parts {
		if part.offset != part.size {
			return ErrContentIncomplete
		}
		files[key] = part.path
	}
	return run(files)
}

func (u *ContentUpload) CleanupDirectory() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.aborted.Store(true)
	if u.Directory == "" {
		return nil
	}
	return os.RemoveAll(u.Directory)
}

func (u *ContentUpload) RemoveStaging() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.aborted.Load() {
		return ErrContentClosed
	}
	if u.Directory == "" {
		return nil
	}
	if err := os.RemoveAll(u.Directory); err != nil {
		return err
	}
	u.Directory = ""
	return nil
}

func (u *ContentUpload) IsAborted() bool {
	return u.aborted.Load()
}

func (m *Manager) AbortContentUpload(channelID, senderKey, uploadID string) (ContentRelease, error) {
	m.mu.Lock()
	record, ok := m.channels[channelID]
	if !ok {
		m.mu.Unlock()
		return ContentRelease{}, ErrChannelNotFound
	}
	if !matchHash(record.senderHash, senderKey) {
		m.mu.Unlock()
		return ContentRelease{}, ErrUnauthorized
	}
	if record.contentUpload == nil || record.contentUpload.ID != uploadID {
		m.mu.Unlock()
		return ContentRelease{}, nil
	}
	if record.contentUpload.State == ContentHeld {
		m.mu.Unlock()
		return ContentRelease{}, ErrContentConflict
	}
	release := m.detachContentLocked(&record)
	m.channels[channelID] = record
	if release.Slug != "" {
		m.pendingContentReleases[channelID] = release
	}
	m.mu.Unlock()
	m.cleanupUpload(release.Upload)
	return release, nil
}

func (m *Manager) detachContentLocked(record *Record) ContentRelease {
	release := ContentRelease{ChannelID: record.ID, Slug: record.contentSlug, HeldBytes: record.contentHeld, Upload: record.contentUpload}
	if release.Upload != nil {
		release.Upload.aborted.Store(true)
		release.Upload.State = ContentAborted
	}
	record.ContentState = ContentEmpty
	record.contentUpload = nil
	record.contentSlug = ""
	record.contentHeld = 0
	record.contentBytes = 0
	return release
}

func (m *Manager) cleanupUpload(session *ContentUpload) {
	if session == nil {
		return
	}
	_ = session.CleanupDirectory()
	m.mu.Lock()
	defer m.mu.Unlock()
	if session.Reservation > 0 {
		m.content.reserved -= session.Reservation
		session.Reservation = 0
	}
	if session.Written > 0 {
		m.content.written -= session.Written
		if m.content.written < 0 {
			m.content.written = 0
		}
		session.Written = 0
	}
	session.Manifest = models.ShareManifest{}
	delete(m.contentUploads, session.ID)
}

func (m *Manager) CleanupContentRelease(release ContentRelease) {
	m.cleanupUpload(release.Upload)
}

func (m *Manager) ContentReleaseDeleted(release ContentRelease) {
	m.CleanupContentRelease(release)
	m.mu.Lock()
	defer m.mu.Unlock()
	if release.Slug == "" {
		return
	}
	if held, exists := m.content.heldBySlug[release.Slug]; exists {
		m.content.held -= held
		delete(m.content.heldBySlug, release.Slug)
	}
	if pending, exists := m.pendingContentReleases[release.ChannelID]; exists && pending.Slug == release.Slug {
		delete(m.pendingContentReleases, release.ChannelID)
	}
}

func (m *Manager) ContentSlotCapacity() (ready bool, floor, budget, held, reserved int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.content.ready, m.content.floor, m.content.budget, m.content.held, m.content.reserved
}
