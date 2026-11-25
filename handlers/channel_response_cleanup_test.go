package handlers

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

func cleanupDeliveryFixture(t *testing.T) (*channels.Manager, *channels.DeliveryLease) {
	t.Helper()
	m := channels.NewManager()
	if err := m.InitializeContentCapacity(1<<20, 1); err != nil {
		t.Fatal(err)
	}
	key, _ := channels.NewRequestID()
	r, sender, _, err := m.Create("cleanup-sender", key, channels.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	key, _ = channels.NewRequestID()
	j, err := m.Join(r.ID, "account:cleanup", key, "cleanup", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{Kind: "file", Name: "data", Parts: []models.SharePart{{Key: "p0", MIME: "text/plain", Size: 4}}}}}
	key, _ = channels.NewRequestID()
	u, err := m.BeginContent(r.ID, sender, key, manifest, sha256.Sum256([]byte("manifest")), 4, 100, 100, 1<<20, t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendContentChunk(r.ID, sender, u.UploadID, "p0", 0, []byte("data"), sha256.Sum256([]byte("data"))); err != nil {
		t.Fatal(err)
	}
	key, _ = channels.NewRequestID()
	s, _, _, err := m.BeginContentFinalize(r.ID, sender, u.UploadID, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveStaging(); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteContentFinalize(r.ID, sender, s, "CLN52", 4); err != nil {
		t.Fatal(err)
	}
	d, err := m.BeginDelivery(r.ID, j.SeatToken, j.ClaimToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ReserveDeliverySpace(d.Lease, 100, 1<<20); err != nil {
		t.Fatal(err)
	}
	return m, d.Lease
}

func TestResponseCleanupFailureRetainsOwnershipAndReservationUntilRetry(t *testing.T) {
	m, lease := cleanupDeliveryFixture(t)
	h := NewChannelHandler(m, &config.Config{}, nil)
	dir := filepath.Join(t.TempDir(), channelResponseTempPrefix+"failed")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "response")
	if err := os.WriteFile(file, []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &preparedChannelResponse{directory: dir, removeDirectory: func(string) error { return errors.New("injected removal failure") }}
	if err := h.releasePreparedResponse(lease, p); err == nil {
		t.Fatal("expected failed removal")
	}
	assertPendingResponseBudget(t, h, m, 1, 100)
	p.removeDirectory = os.RemoveAll
	h.maintainChannelLiveness(time.Now())
	assertPendingResponseBudget(t, h, m, 1, 100)
	attachCleanupWriter(t, h)
	h.maintainChannelLiveness(time.Now())
	assertPendingResponseBudget(t, h, m, 0, 0)
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("response remains: %v", err)
	}
	h.maintainChannelLiveness(time.Now())
	assertPendingResponseBudget(t, h, m, 0, 0)
}

func attachCleanupWriter(t *testing.T, h *ChannelHandler) {
	t.Helper()
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireChannelWriter(); err != nil {
		t.Fatal(err)
	}
	h.channelWriter = store
	h.store = store
	h.contentInitialized = true
	t.Cleanup(func() { _ = store.Close() })
}

func assertPendingResponseBudget(t *testing.T, h *ChannelHandler, m *channels.Manager, pending int, reserved int64) {
	t.Helper()
	_, _, _, _, actual := m.ContentSlotCapacity()
	if len(h.pendingResponses) != pending || actual != reserved {
		t.Fatalf("pending=%d reserved=%d", len(h.pendingResponses), actual)
	}
}

func TestStartupSweepsOnlyChannelResponseDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{channelResponseTempPrefix + "orphan", ".nclip-share-ordinary"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "response"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := NewChannelHandler(channels.NewManager(), &config.Config{DataDir: root}, nil)
	if err := h.InitializeContent(store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.ReleaseContentWriter() })
	if _, err := os.Stat(filepath.Join(root, channelResponseTempPrefix+"orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("channel response orphan remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".nclip-share-ordinary", "response")); err != nil {
		t.Fatalf("ordinary share temp changed: %v", err)
	}
}
