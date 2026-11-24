package channels

import (
	"testing"
	"time"
)

func createChannelForJoinTest(t *testing.T) (*Manager, Record, string) {
	t.Helper()
	manager := NewManager()
	requestID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	record, senderKey, _, err := manager.Create("sender", requestID, VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	return manager, record, senderKey
}

func TestJoinAuthenticatedReceiverIsPairedAndIdempotent(t *testing.T) {
	manager, record, _ := createChannelForJoinTest(t)
	requestID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	joined, err := manager.Join(record.ID, "account:user-1", requestID, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if joined.State != "paired" || joined.Identity != "alice" || joined.Anonymous || joined.SeatToken == "" || joined.ClaimToken == "" {
		t.Fatalf("unexpected authenticated join: %+v", joined)
	}
	replay, err := manager.Join(record.ID, "account:user-1", requestID, "alice", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.SeatToken != joined.SeatToken || replay.ClaimToken != joined.ClaimToken {
		t.Fatal("same join key did not replay the original seat")
	}
	otherID, _ := NewRequestID()
	if _, err := manager.Join(record.ID, "account:user-2", otherID, "bob", now.Add(2*time.Second)); err != ErrSeatsFull {
		t.Fatalf("second receiver error = %v", err)
	}
}

func TestAnonymousJoinRequiresSenderDecisionAndLeaveIsIdempotent(t *testing.T) {
	manager, record, senderKey := createChannelForJoinTest(t)
	requestID, _ := NewRequestID()
	now := time.Now()
	joined, err := manager.Join(record.ID, "ip:203.0.113.1", requestID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if joined.State != "pending_admit" || joined.Identity != "anonymous" || !joined.Anonymous || joined.PendingUntil.Sub(now) != PendingAdmissionWindow {
		t.Fatalf("unexpected anonymous join: %+v", joined)
	}
	decisionID, _ := NewRequestID()
	admitted, err := manager.Decide(record.ID, senderKey, decisionID, "admit", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if admitted.State != "paired" || admitted.Identity != "anonymous" {
		t.Fatalf("unexpected admission result: %+v", admitted)
	}
	replay, err := manager.Decide(record.ID, senderKey, decisionID, "admit", now.Add(2*time.Second))
	if err != nil || !replay.Replayed {
		t.Fatalf("admit retry = %+v, %v", replay, err)
	}
	if _, err := manager.Decide(record.ID, senderKey, decisionID, "reject", now.Add(2*time.Second)); err != ErrIdempotencyConflict {
		t.Fatalf("opposite retry error = %v", err)
	}
	leaveID, _ := NewRequestID()
	if err := manager.Leave(record.ID, leaveID, joined.SeatToken, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leave(record.ID, leaveID, joined.SeatToken, now.Add(4*time.Second)); err != nil {
		t.Fatalf("idempotent leave: %v", err)
	}
	if err := manager.Leave(record.ID, leaveID, "another-seat-token", now.Add(4*time.Second)); err != ErrIdempotencyConflict {
		t.Fatalf("leave replay with a different token = %v", err)
	}
	if _, err := manager.Join(record.ID, "ip:203.0.113.1", requestID, "", now.Add(5*time.Second)); err != ErrJoinClosed {
		t.Fatalf("same join key after leave = %v", err)
	}
	nextJoinID, _ := NewRequestID()
	nextSeat, err := manager.Join(record.ID, "ip:203.0.113.1", nextJoinID, "", now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Leave(record.ID, leaveID, joined.SeatToken, now.Add(7*time.Second)); err != nil {
		t.Fatalf("lost leave response replay: %v", err)
	}
	if _, err := manager.ReceiverStatus(record.ID, nextSeat.SeatToken, now.Add(8*time.Second)); err != nil {
		t.Fatalf("old leave replay released the next seat: %v", err)
	}
}

func TestAnonymousRejectReleasesSeatAndReplaysDecision(t *testing.T) {
	manager, record, senderKey := createChannelForJoinTest(t)
	joinID, _ := NewRequestID()
	now := time.Now()
	joined, err := manager.Join(record.ID, "ip:203.0.113.2", joinID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	decisionID, _ := NewRequestID()
	rejected, err := manager.Decide(record.ID, senderKey, decisionID, "reject", now.Add(time.Second))
	if err != nil || rejected.State != "open" {
		t.Fatalf("reject = %+v, %v", rejected, err)
	}
	replay, err := manager.Decide(record.ID, senderKey, decisionID, "reject", now.Add(2*time.Second))
	if err != nil || !replay.Replayed {
		t.Fatalf("reject retry = %+v, %v", replay, err)
	}
	if _, err := manager.ReceiverStatus(record.ID, joined.SeatToken, now.Add(3*time.Second)); err != ErrJoinClosed {
		t.Fatalf("rejected receiver status = %v", err)
	}
	newJoinID, _ := NewRequestID()
	if _, err := manager.Join(record.ID, "ip:203.0.113.3", newJoinID, "", now.Add(4*time.Second)); err != nil {
		t.Fatalf("new receiver after reject: %v", err)
	}
}

func TestPendingAdmissionExpiresAndSenderHeartbeatCanRestoreJoinability(t *testing.T) {
	manager, record, senderKey := createChannelForJoinTest(t)
	joinID, _ := NewRequestID()
	now := time.Now()
	joined, err := manager.Join(record.ID, "ip:203.0.113.4", joinID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	expiredAt := now.Add(PendingAdmissionWindow + time.Second)
	if err := manager.SenderHeartbeat(record.ID, senderKey, expiredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReceiverStatus(record.ID, joined.SeatToken, expiredAt); err != ErrPendingExpired {
		t.Fatalf("expired receiver status = %v", err)
	}
	nextJoinID, _ := NewRequestID()
	next, err := manager.Join(record.ID, "ip:203.0.113.5", nextJoinID, "", expiredAt)
	if err != nil || next.State != "pending_admit" {
		t.Fatalf("new join after timeout = %+v, %v", next, err)
	}
}

func TestSenderOfflineAndSeatsFullAttemptsAreStable(t *testing.T) {
	manager, record, senderKey := createChannelForJoinTest(t)
	now := time.Now()
	joinedID, _ := NewRequestID()
	joined, err := manager.Join(record.ID, "account:first", joinedID, "first", now)
	if err != nil {
		t.Fatal(err)
	}
	blockedID, _ := NewRequestID()
	if _, err := manager.Join(record.ID, "account:second", blockedID, "second", now); err != ErrSeatsFull {
		t.Fatalf("full seat error = %v", err)
	}
	leaveID, _ := NewRequestID()
	if err := manager.Leave(record.ID, leaveID, joined.SeatToken, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Join(record.ID, "account:second", blockedID, "second", now.Add(2*time.Second)); err != ErrSeatsFull {
		t.Fatalf("same failed request changed after slot release: %v", err)
	}
	offlineID, _ := NewRequestID()
	offlineAt := now.Add(SenderHeartbeatTimeout + time.Second)
	if _, err := manager.Join(record.ID, "account:third", offlineID, "third", offlineAt); err != ErrSenderOffline {
		t.Fatalf("offline sender join = %v", err)
	}
	if _, err := manager.Join(record.ID, "account:third", offlineID, "third", offlineAt.Add(time.Second)); err != ErrSenderOffline {
		t.Fatalf("same offline request retry = %v", err)
	}
	if err := manager.SenderHeartbeat(record.ID, senderKey, offlineAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	freshID, _ := NewRequestID()
	if _, err := manager.Join(record.ID, "account:third", freshID, "third", offlineAt.Add(3*time.Second)); err != nil {
		t.Fatalf("fresh request after sender heartbeat = %v", err)
	}
}
