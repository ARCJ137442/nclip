package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnwmail/nclip/storage"
)

const channelUploadTempPrefix = ".nclip-channel-upload-"

func (h *ChannelHandler) InitializeContent(store storage.PasteStore) error {
	h.store = store
	h.contentInitialized = false
	writer, ok := store.(storage.ChannelWriterStore)
	if !ok {
		return fmt.Errorf("storage backend cannot prove exclusive channel ownership")
	}
	if err := writer.AcquireChannelWriter(); err != nil {
		return fmt.Errorf("acquire exclusive channel writer: %w", err)
	}
	h.channelWriter = writer
	initialized := false
	defer func() {
		if !initialized {
			_ = h.ReleaseContentWriter()
		}
	}()
	if !writer.OwnsChannelWriter() {
		return fmt.Errorf("exclusive channel writer ownership could not be confirmed")
	}
	channelStore, ok := store.(storage.ChannelContentStore)
	if !ok {
		return fmt.Errorf("storage backend cannot sweep pending channel finalizations")
	}
	if err := channelStore.SweepPendingChannelContent(); err != nil {
		return fmt.Errorf("sweep pending channel finalizations: %w", err)
	}
	if err := removeChannelUploadTemps(h.config.DataDir); err != nil {
		return err
	}
	lister, ok := store.(storage.EphemeralPasteLister)
	if !ok {
		return fmt.Errorf("storage backend cannot sweep orphan channel content")
	}
	pastes, err := lister.ListEphemeralPastes()
	if err != nil {
		return fmt.Errorf("list orphan channel content: %w", err)
	}
	for _, paste := range pastes {
		if err := store.Delete(paste.ID); err != nil {
			return fmt.Errorf("delete orphan channel content: %w", err)
		}
	}
	freeBytes, blockSize, err := storage.FilesystemStats(h.config.DataDir)
	if err != nil {
		return fmt.Errorf("read channel filesystem free space: %w", err)
	}
	if err := h.manager.InitializeContentCapacity(freeBytes, blockSize); err != nil {
		return fmt.Errorf("initialize channel filesystem budget: %w", err)
	}
	h.cleanupMu.Lock()
	h.nextCleanup = time.Now().Add(channelOrphanSweepInterval)
	h.cleanupMu.Unlock()
	h.contentInitialized = true
	initialized = true
	return nil
}

const channelOrphanSweepInterval = time.Hour
const channelLivenessSweepInterval = 5 * time.Second

func removeChannelUploadTemps(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), channelUploadTempPrefix) && !strings.HasPrefix(entry.Name(), storage.ChannelContentTempPrefix+"s3-upload-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, entry.Name())); err != nil {
			return fmt.Errorf("remove orphan channel upload temp: %w", err)
		}
	}
	return nil
}

func (h *ChannelHandler) cleanupExpiredEphemeral(now time.Time) {
	if !h.ContentJanitorReady() {
		return
	}
	if err := h.channelWriter.WithChannelWriter(func() error {
		h.cleanupExpiredEphemeralOwned(now)
		return nil
	}); err != nil {
		log.Printf("[WARN] channel janitor could not confirm writer lock: %v", err)
	}
}

func (h *ChannelHandler) cleanupExpiredEphemeralOwned(now time.Time) {
	h.cleanupMu.Lock()
	if now.Before(h.nextCleanup) {
		h.cleanupMu.Unlock()
		return
	}
	h.nextCleanup = now.Add(channelOrphanSweepInterval)
	h.cleanupMu.Unlock()
	lister, ok := h.store.(storage.EphemeralPasteLister)
	if !ok {
		return
	}
	pastes, err := lister.ListEphemeralPastes()
	if err != nil {
		log.Printf("[WARN] channel orphan sweep: list failed: %v", err)
		return
	}
	for _, paste := range pastes {
		if !paste.IsExpired() || h.manager.ContentActive(paste.ID) {
			continue
		}
		if err := h.store.Delete(paste.ID); err != nil {
			log.Printf("[WARN] channel orphan sweep: delete failed: %v", err)
			continue
		}
		h.manager.ReleaseHeldContent(paste.ID)
	}
}

func (h *ChannelHandler) RunEphemeralJanitor(ctx context.Context) {
	if !h.ContentJanitorReady() {
		return
	}
	interval := h.janitorInterval
	if interval <= 0 {
		interval = channelOrphanSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !h.ContentJanitorReady() {
				return
			}
			h.maintainChannelLiveness(now)
			h.cleanupExpiredEphemeral(now)
		}
	}
}

func (h *ChannelHandler) maintainChannelLiveness(now time.Time) {
	if !h.ContentJanitorReady() {
		return
	}
	processed := make(map[string]struct{})
	err := h.channelWriter.WithChannelWriter(func() error {
		for _, release := range h.manager.SweepReceiverLiveness(now) {
			processed[release.ChannelID] = struct{}{}
			h.manager.CleanupContentRelease(release)
			if err := h.deleteReleasedContent(release); err != nil {
				log.Printf("[WARN] channel released-content cleanup failed: %v", err)
			}
		}
		if h.shares != nil {
			for _, release := range h.manager.PendingContentReleases() {
				if _, handled := processed[release.ChannelID]; handled {
					continue
				}
				if err := h.deleteReleasedContent(release); err != nil {
					log.Printf("[WARN] channel pending-content cleanup retry failed: %v", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("[WARN] channel liveness sweep lost writer ownership: %v", err)
	}
}

// StartEphemeralJanitor starts cleanup only while this handler has a live
// initialized writer lock. The returned stop function waits for an in-progress
// sweep to finish so callers can release the lock afterward safely.
func (h *ChannelHandler) StartEphemeralJanitor() func() {
	if !h.ContentJanitorReady() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.RunEphemeralJanitor(ctx)
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
}
