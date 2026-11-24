package channels

import (
	"sync"
	"testing"
)

func TestCreateIdempotencyAndSenderCapability(t *testing.T) {
	manager := NewManager()
	requestID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	record, senderKey, replay, err := manager.Create("user:sender", requestID, VisibilityAuthenticated)
	if err != nil {
		t.Fatal(err)
	}
	if replay || len(record.ID) != 22 || len(senderKey) != 43 {
		t.Fatalf("unexpected generated values: replay=%v id=%q key length=%d", replay, record.ID, len(senderKey))
	}
	if !manager.SenderAuthorized(record.ID, senderKey) || manager.SenderAuthorized(record.ID, "wrong") {
		t.Fatal("sender capability validation failed")
	}
	replayed, replayKey, replay, err := manager.Create("user:sender", requestID, VisibilityAuthenticated)
	if err != nil {
		t.Fatal(err)
	}
	if !replay || replayed.ID != record.ID || replayKey != senderKey {
		t.Fatal("idempotent retry did not return the original channel and capability")
	}
}

func TestCreateIdempotencyConflictAndScope(t *testing.T) {
	manager := NewManager()
	requestID, _ := NewRequestID()
	first, _, _, err := manager.Create("user:first", requestID, VisibilityAuthenticated)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Create("user:first", requestID, VisibilityPublic); err != ErrIdempotencyConflict {
		t.Fatalf("visibility mutation error = %v", err)
	}
	second, _, _, err := manager.Create("user:second", requestID, VisibilityAuthenticated)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("idempotency key was not scoped to the requester")
	}
}

func TestConcurrentCreateRetryReturnsOneChannel(t *testing.T) {
	manager := NewManager()
	requestID, _ := NewRequestID()
	const clients = 32
	records := make([]Record, clients)
	keys := make([]string, clients)
	errors := make([]error, clients)
	var wait sync.WaitGroup
	for i := range clients {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			records[i], keys[i], _, errors[i] = manager.Create("ip:127.0.0.1", requestID, VisibilityPublic)
		}(i)
	}
	wait.Wait()
	for i := range clients {
		if errors[i] != nil {
			t.Fatalf("create %d: %v", i, errors[i])
		}
		if records[i].ID != records[0].ID || keys[i] != keys[0] {
			t.Fatal("concurrent idempotent create produced different channels")
		}
	}
}

func TestRequestIDRequires128RandomBits(t *testing.T) {
	if ValidRequestID("short") {
		t.Fatal("accepted short request id")
	}
	id, err := NewRequestID()
	if err != nil || !ValidRequestID(id) {
		t.Fatalf("generated request id is invalid: %q %v", id, err)
	}
}
