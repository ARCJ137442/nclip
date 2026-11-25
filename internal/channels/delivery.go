package channels

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrDeliveryNotReady     = errors.New("delivery_not_ready")
	ErrDeliveryBusy         = errors.New("delivery_busy")
	ErrDeliveryGap          = errors.New("range_gap")
	ErrDeliveryComplete     = errors.New("delivery_complete")
	ErrSeatKicked           = errors.New("receiver_kicked")
	ErrDeliveryModeConflict = errors.New("delivery_mode_conflict")
)

type DeliveryMode string

const (
	DeliveryModeUnset             DeliveryMode = ""
	DeliveryModeAppMultipart      DeliveryMode = "app-multipart"
	DeliveryModeBrowserAttachment DeliveryMode = "browser-attachment"
)

type DeliveryLease struct {
	ChannelID string
	SeatID    string
	Mode      DeliveryMode
	ctx       context.Context
	cancel    context.CancelFunc
	canceled  atomic.Bool
	mu        sync.Mutex
	source    io.Closer
	interrupt func()
	start     int64
	written   int64
	reserved  int64
	material  int64
}

type DeliveryStart struct {
	Lease            *DeliveryLease
	Slug             string
	ContentBytes     int64
	DeliveredThrough int64
}

func (d *DeliveryLease) Context() context.Context { return d.ctx }

func (d *DeliveryLease) SetSource(source io.Closer) {
	d.mu.Lock()
	if d.canceled.Load() {
		d.mu.Unlock()
		_ = source.Close()
		return
	}
	d.source = source
	d.mu.Unlock()
}

func (d *DeliveryLease) SetInterrupt(interrupt func()) {
	d.mu.Lock()
	if d.canceled.Load() {
		d.mu.Unlock()
		if interrupt != nil {
			interrupt()
		}
		return
	}
	d.interrupt = interrupt
	d.mu.Unlock()
}

func (d *DeliveryLease) Cancel() {
	if d == nil || !d.canceled.CompareAndSwap(false, true) {
		return
	}
	d.cancel()
	d.mu.Lock()
	source, interrupt := d.source, d.interrupt
	d.source, d.interrupt = nil, nil
	d.mu.Unlock()
	if source != nil {
		_ = source.Close()
	}
	if interrupt != nil {
		interrupt()
	}
}

func (m *Manager) BeginDelivery(channelID, seatToken, claimToken string, now time.Time) (DeliveryStart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[channelID]
	if !ok {
		return DeliveryStart{}, ErrChannelNotFound
	}
	seat := record.seat
	if seat == nil || !matchHash(seat.seatHash, seatToken) || !matchHash(seat.claimHash, claimToken) {
		return DeliveryStart{}, ErrUnauthorized
	}
	return m.beginDeliveryLocked(&record, DeliveryModeAppMultipart, now, true, true)
}

// IssueDeliveryTicket mints a page-scoped capability for one browser attachment
// delivery. It does not lock the delivery representation or refresh liveness.
func (m *Manager) IssueDeliveryTicket(channelID, seatToken, claimToken string, now time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[channelID]
	if !ok {
		return "", ErrChannelNotFound
	}
	seat := record.seat
	if seat == nil || !matchHash(seat.seatHash, seatToken) || !matchHash(seat.claimHash, claimToken) {
		return "", ErrUnauthorized
	}
	if seat.info.State != "paired" || record.ContentState != ContentHeld || record.contentSlug == "" {
		return "", ErrDeliveryNotReady
	}
	if seat.deliveryMode == DeliveryModeAppMultipart {
		return "", ErrDeliveryModeConflict
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(tokenBytes)
	if seat.delivery != nil {
		if seat.delivery.Mode != DeliveryModeBrowserAttachment {
			return "", ErrDeliveryModeConflict
		}
		seat.delivery.Cancel()
	}
	seat.ticketHash = sha256.Sum256([]byte(ticket))
	m.channels[channelID] = record
	return ticket, nil
}

// BeginTicketDelivery validates the current seat-bound ticket and starts the
// same exclusive delivery lease used by app multipart reads. Unlike page
// status/heartbeat, a ticket request never restores a released seat.
func (m *Manager) BeginTicketDelivery(channelID, ticket string, now time.Time) (DeliveryStart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[channelID]
	if !ok {
		return DeliveryStart{}, ErrChannelNotFound
	}
	seat := record.seat
	if seat == nil || !matchHash(seat.ticketHash, ticket) {
		return DeliveryStart{}, ErrUnauthorized
	}
	if seat.info.State == "released" {
		return DeliveryStart{}, ErrDeliveryNotReady
	}
	if seat.info.State != "paired" || record.ContentState != ContentHeld || record.contentSlug == "" {
		return DeliveryStart{}, ErrDeliveryNotReady
	}
	return m.beginDeliveryLocked(&record, DeliveryModeBrowserAttachment, now, false, false)
}

func (m *Manager) beginDeliveryLocked(record *Record, mode DeliveryMode, now time.Time, recoverReleased, refreshLiveness bool) (DeliveryStart, error) {
	seat := record.seat
	if seat == nil {
		return DeliveryStart{}, ErrDeliveryNotReady
	}
	if seat.info.State == "released" {
		if !recoverReleased {
			return DeliveryStart{}, ErrDeliveryNotReady
		}
		if !seat.releaseUntil.After(now) {
			return DeliveryStart{}, ErrJoinClosed
		}
		seat.info.State = "paired"
		seat.releaseUntil = time.Time{}
	}
	if seat.info.State != "paired" || record.ContentState != ContentHeld || record.contentSlug == "" {
		return DeliveryStart{}, ErrDeliveryNotReady
	}
	if seat.deliveryMode != DeliveryModeUnset && seat.deliveryMode != mode {
		return DeliveryStart{}, ErrDeliveryModeConflict
	}
	if seat.delivery != nil {
		return DeliveryStart{}, ErrDeliveryBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease := &DeliveryLease{ChannelID: record.ID, SeatID: seat.id, Mode: mode, ctx: ctx, cancel: cancel}
	if seat.deliveryMode == DeliveryModeUnset {
		seat.deliveryMode = mode
	}
	seat.delivery = lease
	if refreshLiveness {
		seat.lastSeen = now
	}
	record.Seat = copySeatInfo(&seat.info)
	m.channels[record.ID] = *record
	return DeliveryStart{Lease: lease, Slug: record.contentSlug, ContentBytes: record.contentBytes, DeliveredThrough: seat.deliveredThrough}, nil
}

func (m *Manager) SetDeliveryRange(lease *DeliveryLease, start int64) error {
	if lease == nil || start < 0 {
		return ErrContentOffset
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[lease.ChannelID]
	if !ok || record.seat == nil || record.seat.delivery != lease || record.seat.deliveryMode != lease.Mode || lease.canceled.Load() {
		return ErrContentClosed
	}
	if start > record.seat.deliveredThrough {
		return ErrDeliveryGap
	}
	lease.start = start
	lease.written = 0
	return nil
}

func (m *Manager) RecordDeliveryBytes(lease *DeliveryLease, bytes int64, now time.Time) error {
	if lease == nil || bytes < 0 {
		return ErrContentOffset
	}
	if bytes == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[lease.ChannelID]
	if !ok || record.seat == nil || record.seat.delivery != lease || record.seat.deliveryMode != lease.Mode || lease.canceled.Load() {
		return ErrContentClosed
	}
	if bytes > math.MaxInt64-lease.start-lease.written {
		return ErrCapacity
	}
	lease.written += bytes
	through := lease.start + lease.written
	if through > record.seat.deliveredThrough {
		record.seat.deliveredThrough = through
	}
	record.seat.lastSeen = now
	m.channels[lease.ChannelID] = record
	return nil
}

func (m *Manager) FinishDelivery(lease *DeliveryLease, total int64, complete bool, now time.Time) (ContentRelease, bool, error) {
	if lease == nil || total < 0 {
		return ContentRelease{}, false, ErrContentOffset
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[lease.ChannelID]
	if !ok || record.seat == nil || record.seat.delivery != lease || record.seat.deliveryMode != lease.Mode {
		return ContentRelease{}, false, ErrContentClosed
	}
	seat := record.seat
	seat.delivery = nil
	if lease.canceled.Load() {
		record.Seat = copySeatInfo(&seat.info)
		m.channels[lease.ChannelID] = record
		return ContentRelease{}, false, ErrContentClosed
	}
	if lease.written > 0 {
		seat.lastSeen = now
	}
	if complete && total > 0 && seat.deliveredThrough >= total {
		release := m.detachContentLocked(&record)
		m.releaseSeatLocked(&record, "delivered", now)
		m.channels[lease.ChannelID] = record
		if release.Slug != "" {
			m.pendingContentReleases[lease.ChannelID] = release
		}
		return release, true, nil
	}
	record.Seat = copySeatInfo(&seat.info)
	m.channels[lease.ChannelID] = record
	return ContentRelease{}, false, nil
}

func (m *Manager) ReserveDeliverySpace(lease *DeliveryLease, reservation, freeBytes int64) error {
	if lease == nil || reservation <= 0 || freeBytes < 0 {
		return ErrContentNotReady
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[lease.ChannelID]
	if !ok || record.seat == nil || record.seat.delivery != lease || record.seat.deliveryMode != lease.Mode || lease.canceled.Load() {
		return ErrContentClosed
	}
	if !m.content.ready || lease.reserved != 0 {
		return ErrContentNotReady
	}
	if reservation > m.content.budget-m.content.held-m.content.reserved {
		return ErrContentNotReady
	}
	unmaterialized := m.content.reserved - m.content.written
	if unmaterialized < 0 || reservation > math.MaxInt64-unmaterialized || m.content.floor > math.MaxInt64-unmaterialized-reservation || freeBytes < m.content.floor+unmaterialized+reservation {
		return ErrContentNotReady
	}
	m.content.reserved += reservation
	lease.reserved = reservation
	return nil
}

func (m *Manager) MaterializeDeliverySpace(lease *DeliveryLease, materialized int64) error {
	if lease == nil || materialized < 0 {
		return ErrCapacity
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.deliveryCurrentLocked(lease) || materialized > lease.reserved || materialized < lease.material {
		return ErrContentClosed
	}
	delta := materialized - lease.material
	if delta > math.MaxInt64-m.content.written {
		return ErrCapacity
	}
	m.content.written += delta
	lease.material = materialized
	return nil
}

func (m *Manager) ReleaseDeliverySpace(lease *DeliveryLease) {
	if lease == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease.reserved > 0 {
		m.content.reserved -= lease.reserved
		m.content.written -= lease.material
		if m.content.written < 0 {
			m.content.written = 0
		}
		lease.reserved, lease.material = 0, 0
	}
}

func (m *Manager) SweepReceiverLiveness(now time.Time) []ContentRelease {
	m.mu.Lock()
	defer m.mu.Unlock()
	var releases []ContentRelease
	for channelID, record := range m.channels {
		seat := record.seat
		if seat == nil {
			continue
		}
		if seat.info.State == "pending_admit" {
			if !seat.info.PendingUntil.After(now) {
				m.releaseSeatLocked(&record, "pending_expired", now)
				m.channels[channelID] = record
			}
			continue
		}
		if seat.info.State == "paired" && now.Sub(seat.lastSeen) >= ReceiverHeartbeatTimeout {
			if seat.delivery != nil {
				seat.delivery.Cancel()
				seat.delivery = nil
			}
			seat.info.State = "released"
			seat.releaseUntil = now.Add(ChannelReleaseWindow)
			record.Seat = copySeatInfo(&seat.info)
			m.channels[channelID] = record
			continue
		}
		if seat.info.State != "released" || seat.releaseUntil.After(now) {
			continue
		}
		release := m.detachContentLocked(&record)
		m.releaseSeatLocked(&record, "receiver_timeout", now)
		m.channels[channelID] = record
		if release.Slug != "" {
			m.pendingContentReleases[channelID] = release
		}
		if release.Slug != "" || release.Upload != nil {
			releases = append(releases, release)
		}
	}
	return releases
}

func (m *Manager) deliveryCurrentLocked(lease *DeliveryLease) bool {
	if lease == nil {
		return false
	}
	record, ok := m.channels[lease.ChannelID]
	return ok && record.seat != nil && record.seat.delivery == lease && record.seat.deliveryMode == lease.Mode && !lease.canceled.Load()
}
