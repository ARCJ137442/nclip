package storage

import (
	"errors"
	"io"

	"github.com/johnwmail/nclip/models"
)

// ErrNotFound is returned by Get when the requested paste does not exist.
// All PasteStore implementations must return this (or a wrapped version of it)
// so callers can use errors.Is(err, ErrNotFound) regardless of backend.
var ErrNotFound = errors.New("paste not found")
var ErrSlugReserved = errors.New("paste slug is reserved for channel content")
var ErrChannelWriterNotOwned = errors.New("channel writer lock is not owned")

const ChannelContentTempPrefix = ".nclip-channel-content-"
const ChannelWriterLockKey = ".nclip-channel-writer.lock"
const ChannelContentReservationSuffix = ".uploading"
const PasteSlugReservationPrefix = ".nclip-paste-slug-"

// PasteStore defines the interface for paste storage backends
type PasteStore interface {
	// Store saves a paste to the storage backend
	Store(paste *models.Paste) error

	// Get retrieves a paste by its ID
	Get(id string) (*models.Paste, error)

	// Exists checks if a paste exists by its ID
	Exists(id string) (bool, error)

	// Delete removes a paste from storage
	Delete(id string) error

	// IncrementReadCount increments the read count for a paste
	IncrementReadCount(id string) error

	// Close closes the storage connection
	Close() error

	// StoreContent saves the raw content for a paste
	StoreContent(id string, content []byte) error

	// StoreContentStream saves exactly size bytes without requiring the caller
	// to keep the payload in memory. Implementations must not publish partial
	// content as a successfully stored object.
	StoreContentStream(id string, content io.Reader, size int64) error

	// GetContent retrieves the raw content for a paste
	GetContent(id string) ([]byte, error)

	// OpenContent returns a streaming reader for the raw content.
	OpenContent(id string) (io.ReadCloser, int64, error)

	// GetContentPrefix retrieves up to n bytes of the raw content for a paste.
	// Implementations should return as many bytes as are available up to n.
	GetContentPrefix(id string, n int64) ([]byte, error)

	// StatContent reports whether content exists and its size. Implementations
	// should return (false, 0, nil) when the content does not exist.
	// This is used by handlers to perform early size checks without
	// retrieving the entire object.
	StatContent(id string) (exists bool, size int64, err error)
}

// EphemeralPasteLister is an internal cleanup capability. Its results must
// not be returned to user-facing management APIs.
type EphemeralPasteLister interface {
	ListEphemeralPastes() ([]models.Paste, error)
}

// ChannelContentStore gives unpublished channel finalization files a distinct
// temporary prefix so startup cleanup can remove them without touching normal shares.
type ChannelContentStore interface {
	StoreChannelContentStream(id string, content io.Reader, size int64) error
	CommitChannelContent(id string) error
	SweepPendingChannelContent() error
}

// ChannelWriterStore gives the channel manager an exclusive backend-wide
// writer lease. Failure to prove exclusivity must disable channel routes.
type ChannelWriterStore interface {
	AcquireChannelWriter() error
	ReleaseChannelWriter() error
	OwnsChannelWriter() bool
	WithChannelWriter(func() error) error
}

// PasteSlugReserver atomically holds a slug across a content+metadata create.
type PasteSlugReserver interface {
	ReservePasteSlug(id string) (func() error, error)
}
