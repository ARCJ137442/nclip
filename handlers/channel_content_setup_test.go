package handlers

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

type blockingJanitorFilesystemStore struct {
	*storage.FilesystemStore
	block     atomic.Bool
	listCalls atomic.Int32
	entered   chan struct{}
	proceed   chan struct{}
	once      sync.Once
}

func (s *blockingJanitorFilesystemStore) ListEphemeralPastes() ([]models.Paste, error) {
	s.listCalls.Add(1)
	if s.block.Load() {
		s.once.Do(func() { close(s.entered) })
		<-s.proceed
	}
	return s.FilesystemStore.ListEphemeralPastes()
}

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
	if err := store.AcquireChannelWriter(); err != nil {
		t.Fatal(err)
	}
	handler.channelWriter = store
	handler.contentInitialized = true
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

func TestSecondHandlerWithoutWriterLockCannotRunJanitor(t *testing.T) {
	dataDir := t.TempDir()
	first, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err := first.AcquireChannelWriter(); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour)
	paste := &models.Paste{ID: "OLD52", CreatedAt: time.Now().Add(-9 * 24 * time.Hour), ExpiresAt: &expired, Size: 3, ContentType: "text/plain", EphemeralChannelID: "active-channel"}
	if err := first.StoreContent(paste.ID, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := first.Store(paste); err != nil {
		t.Fatal(err)
	}

	secondBase, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second := &blockingJanitorFilesystemStore{FilesystemStore: secondBase, entered: make(chan struct{}), proceed: make(chan struct{})}
	t.Cleanup(func() { _ = second.Close() })
	handler := NewChannelHandler(channels.NewManager(), &config.Config{DataDir: dataDir}, nil)
	if err := handler.InitializeContent(second); err == nil {
		t.Fatal("second handler initialized without the shared writer lock")
	}
	handler.janitorInterval = time.Millisecond
	stop := handler.StartEphemeralJanitor()
	stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		handler.RunEphemeralJanitor(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("direct janitor run did not fail closed without writer ownership")
	}
	cancel()
	handler.cleanupExpiredEphemeral(time.Now().Add(2 * channelOrphanSweepInterval))
	if calls := second.listCalls.Load(); calls != 0 {
		t.Fatalf("janitor listed orphan content without writer ownership: %d calls", calls)
	}
	if _, err := second.Get(paste.ID); err != nil {
		t.Fatalf("handler without the writer lock deleted active content: %v", err)
	}
}

func TestStopJanitorWaitsForSweepBeforeWriterLockRelease(t *testing.T) {
	dataDir := t.TempDir()
	base, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	store := &blockingJanitorFilesystemStore{
		FilesystemStore: base,
		entered:         make(chan struct{}),
		proceed:         make(chan struct{}),
	}
	t.Cleanup(func() { _ = store.Close() })
	handler := NewChannelHandler(channels.NewManager(), &config.Config{DataDir: dataDir}, nil)
	if err := handler.InitializeContent(store); err != nil {
		t.Fatal(err)
	}
	handler.janitorInterval = time.Millisecond
	handler.cleanupMu.Lock()
	handler.nextCleanup = time.Time{}
	handler.cleanupMu.Unlock()
	store.block.Store(true)
	stop := handler.StartEphemeralJanitor()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not enter its blocked sweep")
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		if err := handler.ReleaseContentWriter(); err != nil {
			t.Errorf("release writer after janitor stop: %v", err)
		}
		close(stopped)
	}()
	select {
	case <-stopped:
		close(store.proceed)
		t.Fatal("janitor stop returned while a sweep was still running")
	case <-time.After(20 * time.Millisecond):
	}
	second, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		close(store.proceed)
		t.Fatal(err)
	}
	if err := second.AcquireChannelWriter(); err == nil {
		_ = second.Close()
		close(store.proceed)
		t.Fatal("second writer acquired the lock during the blocked sweep")
	}
	close(store.proceed)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not stop after the sweep completed")
	}
	if err := second.AcquireChannelWriter(); err != nil {
		t.Fatalf("writer lock was not released after janitor completion: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
