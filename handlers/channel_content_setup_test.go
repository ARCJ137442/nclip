package handlers

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

func TestEphemeralJanitorKeepsHeldContentAndDeletesExpiredOrphan(t *testing.T) {
	dataDir := t.TempDir()
	store, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := channels.NewManager()
	if err := manager.InitializeContentCapacity(1<<20, 1); err != nil {
		t.Fatal(err)
	}
	createID, _ := channels.NewRequestID()
	record, senderKey, _, err := manager.Create("sender", createID, channels.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	joinID, _ := channels.NewRequestID()
	if _, err := manager.Join(record.ID, "account:receiver", joinID, "receiver", time.Now()); err != nil {
		t.Fatal(err)
	}
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "held.bin", Parts: []models.SharePart{{Key: "part1", MIME: "application/octet-stream", Size: 1}},
	}}}
	initID, _ := channels.NewRequestID()
	directory := t.TempDir()
	sessionResult, err := manager.BeginContent(record.ID, senderKey, initID, manifest, sha256.Sum256([]byte("manifest")), 1, 10, 100, 1<<20, directory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	part := []byte("x")
	if _, err := manager.AppendContentChunk(record.ID, senderKey, sessionResult.UploadID, "part1", 0, part, sha256.Sum256(part)); err != nil {
		t.Fatal(err)
	}
	finalizeID, _ := channels.NewRequestID()
	session, _, _, err := manager.BeginContentFinalize(record.ID, senderKey, sessionResult.UploadID, finalizeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RemoveStaging(); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteContentFinalize(record.ID, senderKey, session, "ACTV5", 1); err != nil {
		t.Fatal(err)
	}
	oldExpiry := time.Now().Add(-time.Hour)
	paste := &models.Paste{ID: "ACTV5", CreatedAt: time.Now().Add(-8 * 24 * time.Hour), ExpiresAt: &oldExpiry, Size: 1, ContentType: "application/octet-stream", EphemeralChannelID: record.ID}
	if err := store.StoreContent(paste.ID, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := store.Store(paste); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListEphemeralPastes(); err != nil || len(rows) != 1 || !rows[0].IsExpired() {
		t.Fatalf("expired ephemeral fixture: rows=%+v err=%v", rows, err)
	}
	handler := NewChannelHandler(manager, &config.Config{DataDir: dataDir}, nil)
	handler.store = store
	firstSweep := time.Now()
	handler.cleanupExpiredEphemeral(firstSweep)
	if _, err := store.Get(paste.ID); err != nil {
		t.Fatalf("active content was removed at its fallback TTL: %v", err)
	}

	if _, err := manager.CloseWithContent(record.ID, senderKey); err != nil {
		t.Fatal(err)
	}
	if manager.ContentActive(paste.ID) {
		t.Fatal("closed channel still marks content active")
	}
	handler.cleanupExpiredEphemeral(firstSweep.Add(2 * channelOrphanSweepInterval))
	if _, err := store.Get(paste.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired orphan was not removed: %v", err)
	}
	_, _, _, held, reserved := manager.ContentSlotCapacity()
	if held != 0 || reserved != 0 {
		t.Fatalf("orphan cleanup did not release physical accounting: held=%d reserved=%d", held, reserved)
	}
}
