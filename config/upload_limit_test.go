package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUploadLimitPersistsAndRestores(t *testing.T) {
	dir := t.TempDir()
	limit, err := OpenUploadLimit(dir, 5*MiB, MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	if value, _, revision := limit.Snapshot(); value != 5*MiB || revision != 0 {
		t.Fatalf("initial snapshot = (%d, %d), want (%d, 0)", value, revision, 5*MiB)
	}
	if revision, err := limit.Set(20*MiB, 0); err != nil || revision != 1 {
		t.Fatalf("Set() = (%d, %v), want (1, nil)", revision, err)
	}

	restored, err := OpenUploadLimit(dir, 10*MiB, MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	if value, ceiling, revision := restored.Snapshot(); value != 20*MiB || ceiling != MaxUploadLimitBytes || revision != 1 {
		t.Fatalf("restored snapshot = (%d, %d, %d)", value, ceiling, revision)
	}
}

func TestUploadLimitRejectsInvalidBoundsAndStaleRevision(t *testing.T) {
	if _, err := OpenUploadLimit(t.TempDir(), 0, MaxUploadLimitBytes); err == nil {
		t.Fatal("OpenUploadLimit accepted a zero initial limit")
	}
	if _, err := OpenUploadLimit(t.TempDir(), 2*MiB, 101*MiB); err == nil {
		t.Fatal("OpenUploadLimit accepted a ceiling above the service maximum")
	}
	limit, err := OpenUploadLimit(t.TempDir(), 5*MiB, 20*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if revision, err := limit.Set(21*MiB, 0); err == nil || revision != 0 {
		t.Fatalf("Set above ceiling = (%d, %v), want revision 0 and error", revision, err)
	}
	if revision, err := limit.Set(5*MiB+1, 0); err == nil || revision != 0 {
		t.Fatalf("Set fractional MiB = (%d, %v), want revision 0 and error", revision, err)
	}
	if revision, err := limit.Set(10*MiB, 1); err == nil || revision != 0 {
		t.Fatalf("Set with stale revision = (%d, %v), want revision 0 and error", revision, err)
	}
	if revision, err := limit.Set(10*MiB, 0); err != nil || revision != 1 {
		t.Fatalf("Set with current revision = (%d, %v), want (1, nil)", revision, err)
	}
}

func TestUploadLimitWriteFailureKeepsCurrentValue(t *testing.T) {
	limit, err := OpenUploadLimit(t.TempDir(), 5*MiB, MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(limit.path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(limit.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := limit.Set(10*MiB, 0); err == nil {
		t.Fatal("Set succeeded with a directory at the settings file path")
	}
	if value, _, revision := limit.Snapshot(); value != 5*MiB || revision != 0 {
		t.Fatalf("failed write changed in-memory snapshot = (%d, %d)", value, revision)
	}
}

func TestUploadLimitFilePermissions(t *testing.T) {
	dir := t.TempDir()
	limit, err := OpenUploadLimit(dir, 5*MiB, MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limit.Set(10*MiB, 0); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(dir, ".settings")
	for path, want := range map[string]os.FileMode{
		settingsDir: 0o700,
		limit.path:  0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("permissions for %s = %04o, want %04o", filepath.Base(path), got, want)
		}
	}
}
