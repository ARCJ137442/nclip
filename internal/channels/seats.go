package channels

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"
)

const (
	SenderHeartbeatTimeout = 30 * time.Second
	PendingAdmissionWindow = 2 * time.Minute
	JoinReplayRetention    = 2 * time.Minute
	MaxJoinRecords         = 122880
)

var ErrChannelNotFound = errors.New("channel_not_found")

type seatState struct {
	id         string
	channelID  string
	scope      string
	requestID  string
	joinKey    string
	seatHash   [sha256.Size]byte
	claimHash  [sha256.Size]byte
	info       SeatInfo
	lastSeen   time.Time
	decisionID string
	decision   string
}

type seatClosure struct {
	reason    string
	expiresAt time.Time
}

type joinOutcome struct {
	channelID string
	errorCode string
	expiresAt time.Time
}

type decisionResult struct {
	decision  string
	errorCode string
	state     string
	identity  string
	joinedAt  time.Time
	expiresAt time.Time
}

type leaveResult struct {
	seatHash  [sha256.Size]byte
	release   ContentRelease
	expiresAt time.Time
}

type JoinResult struct {
	State        string
	Identity     string
	Anonymous    bool
	JoinedAt     time.Time
	PendingUntil time.Time
	SeatToken    string
	ClaimToken   string
	Replayed     bool
}

type DecisionResult struct {
	Decision string
	State    string
	Identity string
	JoinedAt time.Time
	Replayed bool
}

func joinKey(channelID, scope, requestID string) string {
	return channelID + "|" + scope + "|" + requestID
}

func decisionKey(channelID, seatID, requestID string) string {
	return channelID + "|" + seatID + "|" + requestID
}

func leaveKey(channelID, requestID string) string {
	return channelID + "|" + requestID
}

func (m *Manager) HasJoinReplay(channelID, scope, requestID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneSeatRecordsLocked(now)
	key := joinKey(channelID, scope, requestID)
	if _, closed := m.closedJoins[key]; closed {
		return true
	}
	if _, attempted := m.joinResults[key]; attempted {
		return true
	}
	record, ok := m.channels[channelID]
	return ok && record.seat != nil && record.seat.joinKey == key
}

func (m *Manager) Join(channelID, scope, requestID, username string, now time.Time) (JoinResult, error) {
	if !ValidRequestID(requestID) {
		return JoinResult{}, ErrInvalidRequestID
	}
	key := joinKey(channelID, scope, requestID)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneSeatRecordsLocked(now)
	if _, closed := m.closedJoins[key]; closed {
		return JoinResult{}, ErrJoinClosed
	}
	if prior, ok := m.joinResults[key]; ok {
		return JoinResult{}, joinError(prior.errorCode)
	}
	record, ok := m.channels[channelID]
	if !ok {
		return JoinResult{}, ErrChannelNotFound
	}
	if record.seat != nil && record.seat.info.State == "pending_admit" && !record.seat.info.PendingUntil.After(now) {
		m.releaseSeatLocked(&record, "pending_expired", now)
		m.channels[channelID] = record
		if _, closed := m.closedJoins[key]; closed {
			return JoinResult{}, ErrJoinClosed
		}
	}
	if record.seat != nil && record.seat.joinKey == key {
		return m.joinResult(record.seat, true), nil
	}
	if now.Sub(record.senderLastSeen) > SenderHeartbeatTimeout {
		record.SenderOnline = false
		m.channels[channelID] = record
		return JoinResult{}, m.cacheJoinErrorLocked(key, channelID, ErrSenderOffline, now)
	}
	if record.seat != nil {
		return JoinResult{}, m.cacheJoinErrorLocked(key, channelID, ErrSeatsFull, now)
	}
	if len(m.closedJoins) >= MaxJoinRecords {
		return JoinResult{}, ErrCapacity
	}
	seatIDBytes := make([]byte, 16)
	if _, err := rand.Read(seatIDBytes); err != nil {
		return JoinResult{}, err
	}
	seatID := base64.RawURLEncoding.EncodeToString(seatIDBytes)
	state := "paired"
	anonymous := false
	pendingUntil := time.Time{}
	identity := username
	if identity == "" {
		state = "pending_admit"
		anonymous = true
		identity = "anonymous"
		pendingUntil = now.Add(PendingAdmissionWindow)
	}
	seatToken := m.deriveSenderKey("seat:"+scope, requestID, channelID+":"+seatID)
	claimToken := m.deriveSenderKey("claim:"+scope, requestID, channelID+":"+seatID)
	seat := &seatState{
		id:        seatID,
		channelID: channelID,
		scope:     scope,
		requestID: requestID,
		joinKey:   key,
		seatHash:  sha256.Sum256([]byte(seatToken)),
		claimHash: sha256.Sum256([]byte(claimToken)),
		info: SeatInfo{
			SeatID:       seatID,
			State:        state,
			Identity:     identity,
			Anonymous:    anonymous,
			JoinedAt:     now.UTC(),
			PendingUntil: pendingUntil,
		},
		lastSeen: now,
	}
	record.seat = seat
	record.Seat = copySeatInfo(&seat.info)
	m.channels[channelID] = record
	return m.joinResult(seat, false), nil
}

func (m *Manager) cacheJoinErrorLocked(key, channelID string, err error, now time.Time) error {
	if len(m.joinResults) >= MaxJoinRecords {
		return ErrCapacity
	}
	m.joinResults[key] = joinOutcome{channelID: channelID, errorCode: err.Error(), expiresAt: now.Add(JoinReplayRetention)}
	return err
}

func joinError(code string) error {
	switch code {
	case ErrSeatsFull.Error():
		return ErrSeatsFull
	case ErrSenderOffline.Error():
		return ErrSenderOffline
	default:
		return ErrCapacity
	}
}

func (m *Manager) joinResult(seat *seatState, replayed bool) JoinResult {
	return JoinResult{
		State:        seat.info.State,
		Identity:     seat.info.Identity,
		Anonymous:    seat.info.Anonymous,
		JoinedAt:     seat.info.JoinedAt,
		PendingUntil: seat.info.PendingUntil,
		SeatToken:    m.deriveSenderKey("seat:"+seat.scope, seat.requestID, seat.channelID+":"+seat.id),
		ClaimToken:   m.deriveSenderKey("claim:"+seat.scope, seat.requestID, seat.channelID+":"+seat.id),
		Replayed:     replayed,
	}
}

func (m *Manager) SenderStatus(id, senderKey string, now time.Time) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[id]
	if !ok {
		return Record{}, ErrChannelNotFound
	}
	if !matchHash(record.senderHash, senderKey) {
		return Record{}, ErrUnauthorized
	}
	record.senderLastSeen = now
	record.SenderOnline = true
	m.expirePendingLocked(&record, now)
	m.channels[id] = record
	return cloneRecord(record), nil
}

func (m *Manager) ReceiverStatus(id, seatToken string, now time.Time) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[id]
	if !ok {
		return Record{}, ErrChannelNotFound
	}
	if record.seat == nil {
		return Record{}, m.closedSeatErrorLocked(seatToken, now)
	}
	if !matchHash(record.seat.seatHash, seatToken) {
		return Record{}, ErrUnauthorized
	}
	if record.seat.info.State == "pending_admit" && !record.seat.info.PendingUntil.After(now) {
		m.releaseSeatLocked(&record, "pending_expired", now)
		m.channels[id] = record
		return Record{}, ErrPendingExpired
	}
	record.seat.lastSeen = now
	m.channels[id] = record
	return cloneRecord(record), nil
}

func (m *Manager) SenderHeartbeat(id, senderKey string, now time.Time) error {
	_, err := m.SenderStatus(id, senderKey, now)
	return err
}

func (m *Manager) SeatHeartbeat(id, seatToken string, now time.Time) error {
	_, err := m.ReceiverStatus(id, seatToken, now)
	return err
}

func (m *Manager) SeatAuthorized(id, seatToken string) bool {
	record, ok := m.Get(id)
	return ok && record.seat != nil && matchHash(record.seat.seatHash, seatToken)
}

func (m *Manager) Leave(id, requestID, seatToken string, now time.Time) error {
	release, err := m.LeaveWithContent(id, requestID, seatToken, now)
	m.CleanupContentRelease(release)
	return err
}

func (m *Manager) LeaveWithContent(id, requestID, seatToken string, now time.Time) (ContentRelease, error) {
	if !ValidRequestID(requestID) {
		return ContentRelease{}, ErrInvalidRequestID
	}
	m.mu.Lock()
	m.pruneSeatRecordsLocked(now)
	key := leaveKey(id, requestID)
	tokenHash := sha256.Sum256([]byte(seatToken))
	if prior, ok := m.leaves[key]; ok {
		if subtle.ConstantTimeCompare(prior.seatHash[:], tokenHash[:]) != 1 {
			m.mu.Unlock()
			return ContentRelease{}, ErrIdempotencyConflict
		}
		m.mu.Unlock()
		return prior.release, nil
	}
	record, ok := m.channels[id]
	if !ok {
		m.mu.Unlock()
		return ContentRelease{}, nil
	}
	if record.seat == nil {
		m.mu.Unlock()
		return ContentRelease{}, nil
	}
	if !matchHash(record.seat.seatHash, seatToken) {
		m.mu.Unlock()
		return ContentRelease{}, ErrUnauthorized
	}
	if len(m.leaves) >= MaxJoinRecords {
		m.mu.Unlock()
		return ContentRelease{}, ErrCapacity
	}
	release := m.detachContentLocked(&record)
	if release.Slug == "" {
		if pending, exists := m.pendingContentReleases[id]; exists {
			release = pending
		}
	}
	m.releaseSeatLocked(&record, "left", now)
	m.channels[id] = record
	if release.Slug != "" {
		m.pendingContentReleases[id] = release
	}
	m.leaves[key] = leaveResult{seatHash: tokenHash, release: release, expiresAt: now.Add(JoinReplayRetention)}
	m.mu.Unlock()
	return release, nil
}

func (m *Manager) Decide(id, senderKey, seatID, requestID, decision string, now time.Time) (DecisionResult, error) {
	if !ValidRequestID(requestID) {
		return DecisionResult{}, ErrInvalidRequestID
	}
	if !ValidSeatID(seatID) {
		return DecisionResult{}, ErrInvalidSeatID
	}
	if decision != "admit" && decision != "reject" {
		return DecisionResult{}, ErrInvalidDecision
	}
	key := decisionKey(id, seatID, requestID)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneSeatRecordsLocked(now)
	record, ok := m.channels[id]
	if !ok {
		return DecisionResult{}, ErrChannelNotFound
	}
	if !matchHash(record.senderHash, senderKey) {
		return DecisionResult{}, ErrUnauthorized
	}
	if prior, ok := m.decisions[key]; ok {
		if prior.decision != decision {
			return DecisionResult{}, ErrIdempotencyConflict
		}
		if prior.errorCode != "" {
			return DecisionResult{}, decisionError(prior.errorCode)
		}
		return DecisionResult{Decision: prior.decision, State: prior.state, Identity: prior.identity, JoinedAt: prior.joinedAt, Replayed: true}, nil
	}
	if len(m.decisions) >= MaxJoinRecords {
		return DecisionResult{}, ErrCapacity
	}
	if record.seat == nil || record.seat.id != seatID {
		closure, closed := m.closedSeatIDs[seatKey(id, seatID)]
		if closed && closure.reason == "pending_expired" {
			m.cacheDecisionErrorLocked(key, decision, ErrPendingExpired, now)
			return DecisionResult{}, ErrPendingExpired
		}
		return DecisionResult{}, ErrDecisionConflict
	}
	if record.seat.info.State != "pending_admit" {
		return DecisionResult{}, ErrDecisionConflict
	}
	if !record.seat.info.PendingUntil.After(now) {
		m.releaseSeatLocked(&record, "pending_expired", now)
		m.channels[id] = record
		m.cacheDecisionErrorLocked(key, decision, ErrPendingExpired, now)
		return DecisionResult{}, ErrPendingExpired
	}
	info := record.seat.info
	state := "paired"
	if decision == "admit" {
		record.seat.info.State = "paired"
		record.seat.info.PendingUntil = time.Time{}
		record.seat.decisionID = requestID
		record.seat.decision = decision
		record.Seat = copySeatInfo(&record.seat.info)
	} else {
		state = "open"
		m.releaseSeatLocked(&record, "sender_rejected", now)
	}
	m.decisions[key] = decisionResult{decision: decision, state: state, identity: info.Identity, joinedAt: info.JoinedAt, expiresAt: now.Add(JoinReplayRetention)}
	m.channels[id] = record
	return DecisionResult{Decision: decision, State: state, Identity: info.Identity, JoinedAt: info.JoinedAt}, nil
}

func (m *Manager) cacheDecisionErrorLocked(key, decision string, err error, now time.Time) {
	m.decisions[key] = decisionResult{
		decision:  decision,
		errorCode: err.Error(),
		expiresAt: now.Add(JoinReplayRetention),
	}
}

func decisionError(code string) error {
	if code == ErrPendingExpired.Error() {
		return ErrPendingExpired
	}
	return ErrDecisionConflict
}

func (m *Manager) expirePendingLocked(record *Record, now time.Time) {
	if record.seat != nil && record.seat.info.State == "pending_admit" && !record.seat.info.PendingUntil.After(now) {
		m.releaseSeatLocked(record, "pending_expired", now)
	}
}

func (m *Manager) releaseSeatLocked(record *Record, reason string, now time.Time) {
	if record.seat == nil {
		return
	}
	seat := record.seat
	m.closedJoins[seat.joinKey] = now.Add(JoinReplayRetention)
	closure := seatClosure{
		reason:    reason,
		expiresAt: now.Add(JoinReplayRetention),
	}
	m.closedSeats[seat.seatHash] = closure
	m.closedSeatIDs[seatKey(record.ID, seat.id)] = closure
	record.seat = nil
	record.Seat = nil
}

func seatKey(channelID, seatID string) string {
	return channelID + "|" + seatID
}

func (m *Manager) closedSeatErrorLocked(seatToken string, now time.Time) error {
	hash := sha256.Sum256([]byte(seatToken))
	closure, ok := m.closedSeats[hash]
	if !ok || !closure.expiresAt.After(now) {
		return ErrUnauthorized
	}
	if closure.reason == "pending_expired" {
		return ErrPendingExpired
	}
	return ErrJoinClosed
}

func (m *Manager) pruneSeatRecordsLocked(now time.Time) {
	for key, expiry := range m.closedJoins {
		if !expiry.After(now) {
			delete(m.closedJoins, key)
		}
	}
	for key, result := range m.joinResults {
		if !result.expiresAt.After(now) {
			delete(m.joinResults, key)
		}
	}
	for key, result := range m.decisions {
		if !result.expiresAt.After(now) {
			delete(m.decisions, key)
		}
	}
	for key, result := range m.leaves {
		if !result.expiresAt.After(now) {
			delete(m.leaves, key)
		}
	}
	for key, closure := range m.closedSeats {
		if !closure.expiresAt.After(now) {
			delete(m.closedSeats, key)
		}
	}
	for key, closure := range m.closedSeatIDs {
		if !closure.expiresAt.After(now) {
			delete(m.closedSeatIDs, key)
		}
	}
}

func (m *Manager) PublicState(id string, now time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[id]
	if !ok {
		return "", ErrChannelNotFound
	}
	m.expirePendingLocked(&record, now)
	m.channels[id] = record
	return record.State, nil
}

func cloneRecord(record Record) Record {
	record.Seat = copySeatInfo(record.Seat)
	return record
}

func copySeatInfo(info *SeatInfo) *SeatInfo {
	if info == nil {
		return nil
	}
	copy := *info
	return &copy
}

func matchHash(expected [sha256.Size]byte, token string) bool {
	if token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(expected[:], got[:]) == 1
}
