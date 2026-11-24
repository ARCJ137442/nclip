package channels

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrDeliveryNotReady = errors.New("channel content is not ready")
	ErrDeliveryBusy     = errors.New("channel delivery is already active")
	ErrDeliveryGap      = errors.New("range starts beyond delivered bytes")
	ErrDeliveryComplete = errors.New("channel delivery already completed")
	ErrSeatKicked       = errors.New("receiver seat was kicked")
)

type DeliveryLease struct {
	ChannelID string
	SeatID    string
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
	if seat.info.State == "released" {
		if !seat.releaseUntil.After(now) {
			return DeliveryStart{}, ErrJoinClosed
		}
		seat.info.State = "paired"
		seat.releaseUntil = time.Time{}
		record.Seat = copySeatInfo(&seat.info)
	}
	if seat.info.State != "paired" || record.ContentState != ContentHeld || record.contentSlug == "" {
		return DeliveryStart{}, ErrDeliveryNotReady
	}
	if seat.delivery != nil {
		return DeliveryStart{}, ErrDeliveryBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease := &DeliveryLease{ChannelID: channelID, SeatID: seat.id, ctx: ctx, cancel: cancel}
	seat.delivery = lease
	seat.lastSeen = now
	seat.info.State = "paired"
	record.Seat = copySeatInfo(&seat.info)
	m.channels[channelID] = record
	return DeliveryStart{Lease: lease, Slug: record.contentSlug, ContentBytes: record.contentBytes, DeliveredThrough: seat.deliveredThrough}, nil
}

func (m *Manager) SetDeliveryRange(lease *DeliveryLease, start int64) error {
	if lease == nil || start < 0 {
		return ErrContentOffset
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.channels[lease.ChannelID]
	if !ok || record.seat == nil || record.seat.delivery != lease || lease.canceled.Load() {
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
	if !ok || record.seat == nil || record.seat.delivery != lease || lease.canceled.Load() {
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
	if !ok || record.seat == nil || record.seat.delivery != lease {
		return ContentRelease{}, false, ErrContentClosed
	}
	seat := record.seat
	seat.delivery = nil
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
	if !ok || record.seat == nil || record.seat.delivery != lease || lease.canceled.Load() {
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
	return ok && record.seat != nil && record.seat.delivery == lease && !lease.canceled.Load()
}
