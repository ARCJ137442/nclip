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
	ErrSeatsFull           = errors.New("seats_full")
	ErrSenderOffline       = errors.New("sender_offline")
	ErrPendingExpired      = errors.New("pending_expired")
	ErrJoinClosed          = errors.New("join_closed")
	ErrDecisionConflict    = errors.New("decision_conflict")
	ErrInvalidDecision     = errors.New("invalid_decision")
	ErrInvalidSeatID       = errors.New("invalid_seat_id")
)

type SeatInfo struct {
	SeatID       string    `json:"seat_id"`
	State        string    `json:"state"`
	Identity     string    `json:"identity"`
	Anonymous    bool      `json:"anonymous"`
	JoinedAt     time.Time `json:"joined_at"`
	PendingUntil time.Time `json:"pending_until,omitempty"`
}

type Record struct {
	ID             string     `json:"id"`
	Visibility     Visibility `json:"visibility"`
	CreatedAt      time.Time  `json:"created_at"`
	State          string     `json:"state"`
	SenderOnline   bool       `json:"sender_online,omitempty"`
	Seat           *SeatInfo  `json:"seat,omitempty"`
	senderHash     [sha256.Size]byte
	senderLastSeen time.Time
	seat           *seatState
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
	closedJoins   map[string]time.Time
	joinResults   map[string]joinOutcome
	closedSeats   map[[sha256.Size]byte]seatClosure
	// closedSeatIDs lets a delayed sender decision resolve the old seat's terminal state.
	closedSeatIDs map[string]seatClosure
	decisions     map[string]decisionResult
	leaves        map[string]leaveResult
	masterKey     [sha256.Size]byte
	keyReady      bool
}

func NewManager() *Manager {
	return &Manager{
		channels:      make(map[string]Record),
		creates:       make(map[string]createResult),
		closedCreates: make(map[string]time.Time),
		closedJoins:   make(map[string]time.Time),
		joinResults:   make(map[string]joinOutcome),
		closedSeats:   make(map[[sha256.Size]byte]seatClosure),
		closedSeatIDs: make(map[string]seatClosure),
		decisions:     make(map[string]decisionResult),
		leaves:        make(map[string]leaveResult),
	}
}

func ValidRequestID(value string) bool {
	return validOpaqueID(value)
}

func ValidSeatID(value string) bool {
	return validOpaqueID(value)
}

func validOpaqueID(value string) bool {
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
		ID:             id,
		Visibility:     visibility,
		CreatedAt:      now.UTC(),
		State:          StateOpen,
		SenderOnline:   true,
		senderHash:     sha256.Sum256([]byte(senderKey)),
		senderLastSeen: now,
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
	return cloneRecord(record), ok
}

func (m *Manager) SenderAuthorized(id, senderKey string) bool {
	record, ok := m.Get(id)
	if !ok || senderKey == "" {
		return false
	}
	got := sha256.Sum256([]byte(senderKey))
	return subtle.ConstantTimeCompare(record.senderHash[:], got[:]) == 1
}

// Close releases the current empty G2 channel and prevents a concurrent create
// retry from reopening it for the closed-key retention window.
func (m *Manager) Close(id, senderKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[id]
	if !ok {
		return nil
	}
	if !matchHash(record.senderHash, senderKey) {
		return ErrUnauthorized
	}
	now := time.Now()
	m.releaseSeatLocked(&record, "sender_closed", now)
	delete(m.channels, id)
	m.pruneClosedCreatesLocked(now)
	for key, result := range m.creates {
		if result.record.ID == id {
			delete(m.creates, key)
			m.closedCreates[key] = now.Add(ClosedCreateRetention)
		}
	}
	return nil
}
