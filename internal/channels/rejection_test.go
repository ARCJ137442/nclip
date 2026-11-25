package channels

import (
	"errors"
	"testing"
	"time"
)

func TestRejectionReasonIsChannelBoundAndDoesNotAffectReplacement(t *testing.T) {
	m, record, sender := createChannelForJoinTest(t)
	now := time.Now()
	key, _ := NewRequestID()
	old, err := m.Join(record.ID, "anonymous:old", key, "", now)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := m.Get(record.ID)
	oldID := state.Seat.SeatID
	decision, _ := NewRequestID()
	if _, err := m.Decide(record.ID, sender, oldID, decision, "reject", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		assertRejectedStatus(t, m, record.ID, old.SeatToken, now.Add(time.Second))
	}
	key, _ = NewRequestID()
	next, err := m.Join(record.ID, "anonymous:next", key, "", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get(record.ID)
	assertRejectedStatus(t, m, record.ID, old.SeatToken, now.Add(3*time.Second))
	if _, err := m.Decide(record.ID, sender, oldID, decision, "reject", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := m.ReceiverStatus(record.ID, next.SeatToken, now.Add(4*time.Second))
	if err != nil || after.Seat.SeatID != before.Seat.SeatID || after.Seat.State != "pending_admit" {
		t.Fatalf("old rejection changed replacement: %v", err)
	}
	assertRejectionCredentialBoundaries(t, m, record, old.SeatToken, now)
}

func assertRejectedStatus(t *testing.T, m *Manager, id, token string, now time.Time) {
	t.Helper()
	_, err := m.ReceiverStatus(id, token, now)
	var closed *JoinClosedError
	if !errors.Is(err, ErrJoinClosed) || !errors.As(err, &closed) || closed.Reason != "sender_rejected" {
		t.Fatalf("rejection reason: %v", err)
	}
}

func assertRejectionCredentialBoundaries(t *testing.T, m *Manager, record Record, token string, now time.Time) {
	t.Helper()
	key, _ := NewRequestID()
	other, _, _, err := m.Create("other-sender", key, VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id, token string
		at        time.Time
	}{
		{other.ID, token, now}, {record.ID, "forged-token", now}, {record.ID, token, now.Add(JoinReplayRetention)},
	} {
		_, err := m.ReceiverStatus(test.id, test.token, test.at)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("invalid/expired tombstone: %v", err)
		}
	}
}
