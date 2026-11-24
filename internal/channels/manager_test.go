package channels

import (
	"reflect"
	"sync"
	"testing"
	"time"
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
	first, firstKey, _, err := manager.Create("user:first", requestID, VisibilityAuthenticated)
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
	if first.ID == second.ID || manager.SenderAuthorized(second.ID, firstKey) {
		t.Fatal("request scope or sender capability crossed channels")
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

func TestChannelLimitAndCloseClearsCreateReplay(t *testing.T) {
	manager := NewManager()
	var firstID, firstKey string
	firstRequestID := ""
	for i := 0; i < MaxChannels; i++ {
		requestID, err := NewRequestID()
		if err != nil {
			t.Fatal(err)
		}
		record, senderKey, _, err := manager.Create("sender", requestID, VisibilityPublic)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if i == 0 {
			firstID, firstKey, firstRequestID = record.ID, senderKey, requestID
		}
	}
	overflowID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Create("sender", overflowID, VisibilityPublic); err != ErrCapacity {
		t.Fatalf("capacity error = %v", err)
	}
	if err := manager.Close(firstID, "wrong"); err != ErrUnauthorized {
		t.Fatalf("wrong sender close error = %v", err)
	}
	if err := manager.Close(firstID, firstKey); err != nil {
		t.Fatalf("sender close: %v", err)
	}
	if manager.HasCreate("sender", firstRequestID) || !manager.IsCreateClosed("sender", firstRequestID) || manager.SenderAuthorized(firstID, firstKey) {
		t.Fatal("closed channel replay tombstone or sender capability state is incorrect")
	}
	if _, _, _, err := manager.Create("sender", firstRequestID, VisibilityPublic); err != ErrCreateClosed {
		t.Fatalf("create retry after close = %v", err)
	}
	manager.mu.Lock()
	manager.pruneClosedCreatesLocked(time.Now().Add(ClosedCreateRetention))
	manager.mu.Unlock()
	record, _, replay, err := manager.Create("sender", firstRequestID, VisibilityPublic)
	if err != nil {
		t.Fatalf("create after replay retention: %v", err)
	}
	if replay || record.ID == firstID || firstKey == "" {
		t.Fatal("expired close tombstone incorrectly replayed a closed channel")
	}
}
func TestCreateReplayDoesNotStoreSenderKey(t *testing.T) {
	if _, ok := reflect.TypeOf(createResult{}).FieldByName("senderKey"); ok {
		t.Fatal("create replay record retains a plaintext sender capability")
	}
}
