package channels

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const (
	MaxChannels           = 1024
	MaxChannelRecords     = 61440
	ClosedCreateRetention = time.Minute
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
	ErrCapacity            = errors.New("channel_capacity_reached")
	ErrUnauthorized        = errors.New("channel_forbidden")
	ErrCreateClosed        = errors.New("channel_create_closed")
)

type Record struct {
	ID         string     `json:"id"`
	Visibility Visibility `json:"visibility"`
	CreatedAt  time.Time  `json:"created_at"`
	State      string     `json:"state"`
	senderHash [sha256.Size]byte
}

type createResult struct {
	visibility Visibility
	record     Record
	key        string
}

type Manager struct {
	mu            sync.Mutex
	channels      map[string]Record
	creates       map[string]createResult
	closedCreates map[string]time.Time
	masterKey     [sha256.Size]byte
	keyReady      bool
}

func NewManager() *Manager {
	return &Manager{
		channels:      make(map[string]Record),
		creates:       make(map[string]createResult),
		closedCreates: make(map[string]time.Time),
	}
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

func createKey(scope, requestID string) string {
	return scope + "|" + requestID
}

func (m *Manager) HasCreate(scope, requestID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneClosedCreatesLocked(time.Now())
	_, ok := m.creates[createKey(scope, requestID)]
	return ok
}

func (m *Manager) IsCreateClosed(scope, requestID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneClosedCreatesLocked(time.Now())
	_, ok := m.closedCreates[createKey(scope, requestID)]
	return ok
}

func (m *Manager) Create(scope, requestID string, visibility Visibility) (Record, string, bool, error) {
	if !ValidRequestID(requestID) {
		return Record{}, "", false, ErrInvalidRequestID
	}
	if visibility != VisibilityAuthenticated && visibility != VisibilityPublic {
		return Record{}, "", false, ErrInvalidVisibility
	}
	key := createKey(scope, requestID)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.pruneClosedCreatesLocked(now)
	if _, closed := m.closedCreates[key]; closed {
		return Record{}, "", false, ErrCreateClosed
	}
	if prior, ok := m.creates[key]; ok {
		if prior.visibility != visibility {
			return Record{}, "", false, ErrIdempotencyConflict
		}
		return prior.record, m.deriveSenderKey(scope, requestID, prior.record.ID), true, nil
	}
	if len(m.channels) >= MaxChannels || len(m.channels)+len(m.closedCreates) >= MaxChannelRecords {
		return Record{}, "", false, ErrCapacity
	}
	if !m.keyReady {
		if _, err := rand.Read(m.masterKey[:]); err != nil {
			return Record{}, "", false, err
		}
		m.keyReady = true
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return Record{}, "", false, err
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	senderKey := m.deriveSenderKey(scope, requestID, id)
	record := Record{
		ID:         id,
		Visibility: visibility,
		CreatedAt:  now.UTC(),
		State:      StateOpen,
		senderHash: sha256.Sum256([]byte(senderKey)),
	}
	m.channels[id] = record
	m.creates[key] = createResult{visibility: visibility, record: record, key: key}
	return record, senderKey, false, nil
}

func (m *Manager) pruneClosedCreatesLocked(now time.Time) {
	for key, expiresAt := range m.closedCreates {
		if !expiresAt.After(now) {
			delete(m.closedCreates, key)
		}
	}
}

func (m *Manager) deriveSenderKey(scope, requestID, channelID string) string {
	mac := hmac.New(sha256.New, m.masterKey[:])
	_, _ = mac.Write([]byte(scope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(requestID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(channelID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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

// Close removes an empty channel and its create result. A short closed-key
// tombstone prevents an in-flight create retry from reopening the channel.
func (m *Manager) Close(id, senderKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[id]
	if !ok {
		return nil
	}
	if senderKey == "" {
		return ErrUnauthorized
	}
	got := sha256.Sum256([]byte(senderKey))
	if subtle.ConstantTimeCompare(record.senderHash[:], got[:]) != 1 {
		return ErrUnauthorized
	}
	delete(m.channels, id)
	now := time.Now()
	m.pruneClosedCreatesLocked(now)
	for key, result := range m.creates {
		if result.record.ID == id {
			delete(m.creates, key)
			m.closedCreates[key] = now.Add(ClosedCreateRetention)
		}
	}
	return nil
}
