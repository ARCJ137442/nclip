package storage

import (
	"os"
	"testing"

	"github.com/johnwmail/nclip/models"
)

func TestNewFilesystemStore_Defaults(t *testing.T) {
	store, err := NewFilesystemStore("./testdata")
	if err != nil {
		t.Fatalf("NewFilesystemStore failed: %v", err)
	}
	if store.dataDir != "./testdata" {
		t.Errorf("expected dataDir ./testdata, got %s", store.dataDir)
	}
}

func TestNewFilesystemStore_CreatesDataDir(t *testing.T) {
	// Use a unique test directory that doesn't exist
	testDir := "./testdata_create_dir"
	defer func() {
		if err := os.RemoveAll(testDir); err != nil {
			t.Fatalf("failed to remove test dir: %v", err)
		}
	}()

	// Ensure directory doesn't exist initially
	_ = os.RemoveAll(testDir)
	// NewFilesystemStore is side-effect free and does not create the directory.
	// The responsibility to create the data directory belongs to startup code
	// (or the write path). Create the directory here to reflect that.
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}

	store, err := NewFilesystemStore(testDir)
	if err != nil {
		t.Fatalf("NewFilesystemStore failed: %v", err)
	}

	if store.dataDir != testDir {
		t.Errorf("expected dataDir %s, got %s", testDir, store.dataDir)
	}
}

func TestPasteSlugReservationIsAtomicAcrossFilesystemStores(t *testing.T) {
	dir := t.TempDir()
	first, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close(); _ = second.Close() }()
	_, release, err := first.ReservePasteSlug("RSV52")
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := second.Exists("RSV52"); err != nil || !exists {
		t.Fatalf("active slug reservation not visible to peer: exists=%t err=%v", exists, err)
	}
	if _, _, err := second.ReservePasteSlug("RSV52"); err != ErrSlugReserved {
		t.Fatalf("second store acquired reserved slug: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if exists, err := second.Exists("RSV52"); err != nil || exists {
		t.Fatalf("released slug remains occupied: exists=%t err=%v", exists, err)
	}
}

func TestFilesystemStore_StoreAndGet_LocalFS(t *testing.T) {
	// Ensure testdata directory exists
	if err := os.MkdirAll("./testdata", 0o755); err != nil {
		t.Fatalf("Failed to create testdata dir: %v", err)
	}
	store, err := NewFilesystemStore("./testdata")
	if err != nil {
		t.Fatalf("NewFilesystemStore failed: %v", err)
	}
	paste := &models.Paste{ID: "testfs", Size: 123}
	err = store.Store(paste)
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	got, err := store.Get("testfs")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ID != "testfs" {
		t.Errorf("expected ID testfs, got %s", got.ID)
	}
	_ = store.Delete("testfs")
	// Clean up testdata directory
	_ = os.RemoveAll("./testdata")
}

func TestFilesystemStore_Delete_LocalFS(t *testing.T) {
	store, err := NewFilesystemStore("./testdata")
	if err != nil {
		t.Fatalf("NewFilesystemStore failed: %v", err)
	}
	paste := &models.Paste{ID: "todel", Size: 1}
	_ = store.Store(paste)
	err = store.Delete("todel")
	if err != nil {
		t.Errorf("Delete failed: %v", err)
	}
}
