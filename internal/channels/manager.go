package channels

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

type Visibility string

const (
	VisibilityAuthenticated Visibility = "authenticated"
	VisibilityPublic        Visibility = "public"
	StateOpen                          = "open"
	StateClosed                        = "closed"
)

var (
	ErrInvalidRequestID    = errors.New("invalid_idempotency_key")
	ErrIdempotencyConflict = errors.New("idempotency_conflict")
	ErrInvalidVisibility   = errors.New("invalid_visibility")
)

type Record struct {
	ID         string     `json:"id"`
	Visibility Visibility `json:"visibility"`
	CreatedAt  time.Time  `json:"created_at"`
	State      string     `json:"state"`
	senderHash [sha256.Size]byte
}

type createResult struct {
	scope      string
	visibility Visibility
	record     Record
	senderKey  string
}

type Manager struct {
	mu       sync.Mutex
	channels map[string]Record
	creates  map[string]createResult
}

func NewManager() *Manager {
	return &Manager{channels: make(map[string]Record), creates: make(map[string]createResult)}
}

func ValidRequestID(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == 16
}

func NewRequestID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (m *Manager) Create(scope, requestID string, visibility Visibility) (Record, string, bool, error) {
	if !ValidRequestID(requestID) {
		return Record{}, "", false, ErrInvalidRequestID
	}
	if visibility != VisibilityAuthenticated && visibility != VisibilityPublic {
		return Record{}, "", false, ErrInvalidVisibility
	}
	key := scope + "\x00" + requestID
	m.mu.Lock()
	defer m.mu.Unlock()
	if prior, ok := m.creates[key]; ok {
		if prior.visibility != visibility {
			return Record{}, "", false, ErrIdempotencyConflict
		}
		return prior.record, prior.senderKey, true, nil
	}

	idBytes := make([]byte, 16)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return Record{}, "", false, err
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return Record{}, "", false, err
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	senderKey := base64.RawURLEncoding.EncodeToString(tokenBytes)
	record := Record{
		ID:         id,
		Visibility: visibility,
		CreatedAt:  time.Now().UTC(),
		State:      StateOpen,
		senderHash: sha256.Sum256([]byte(senderKey)),
	}
	m.channels[id] = record
	m.creates[key] = createResult{scope: scope, visibility: visibility, record: record, senderKey: senderKey}
	return record, senderKey, false, nil
}

func (m *Manager) Get(id string) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[id]
	return record, ok
}

func (m *Manager) SenderAuthorized(id, senderKey string) bool {
	record, ok := m.Get(id)
	if !ok || senderKey == "" {
		return false
	}
	got := sha256.Sum256([]byte(senderKey))
	return subtle.ConstantTimeCompare(record.senderHash[:], got[:]) == 1
}
