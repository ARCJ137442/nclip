package channels

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnwmail/nclip/models"
)

func createPairedContentChannel(t *testing.T, manager *Manager, name string, now time.Time) (Record, string) {
	t.Helper()
	createID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	record, senderKey, _, err := manager.Create("sender:"+name, createID, VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	joinID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Join(record.ID, "account:"+name, joinID, name, now); err != nil {
		t.Fatal(err)
	}
	return record, senderKey
}

func contentManifest(size int64) models.ShareManifest {
	return models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind:  "file",
		Name:  "payload.bin",
		Parts: []models.SharePart{{Key: "part1", MIME: "application/octet-stream", Size: size}},
	}}}
}

func beginContentForTest(t *testing.T, manager *Manager, record Record, senderKey, requestID string, manifest models.ShareManifest, limit, reservation, free int64) ContentInitResult {
	t.Helper()
	manifestHash := sha256.Sum256([]byte(requestID + ":manifest"))
	result, err := manager.BeginContent(record.ID, senderKey, requestID, manifest, manifestHash, manifest.Items[0].Parts[0].Size, limit, reservation, free, t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestContentReservationIsAtomicAndChecksCurrentFreeSpace(t *testing.T) {
	manager := NewManager()
	if err := manager.InitializeContentCapacity(1000, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a, senderA := createPairedContentChannel(t, manager, "a", now)
	b, senderB := createPairedContentChannel(t, manager, "b", now)
	c, senderC := createPairedContentChannel(t, manager, "c", now)
	initA, _ := NewRequestID()
	sessionA := beginContentForTest(t, manager, a, senderA, initA, contentManifest(20), 100, 300, 1000)
	chunk := []byte("12345678901234567890")
	if _, err := manager.AppendContentChunk(a.ID, senderA, sessionA.UploadID, "part1", 0, chunk, sha256.Sum256(chunk)); err != nil {
		t.Fatal(err)
	}
	initB, _ := NewRequestID()
	beginContentForTest(t, manager, b, senderB, initB, contentManifest(20), 100, 300, 980)
	initC, _ := NewRequestID()
	manifestHash := sha256.Sum256([]byte(initC + ":manifest"))
	if _, err := manager.BeginContent(c.ID, senderC, initC, contentManifest(1), manifestHash, 1, 100, 1, 679, t.TempDir(), now); err != ErrContentNotReady {
		t.Fatalf("init below protected free-space floor = %v", err)
	}
	ready, floor, budget, held, reserved := manager.ContentSlotCapacity()
	if !ready || floor != 100 || budget != 900 || held != 0 || reserved != 600 {
		t.Fatalf("unexpected budget accounting: ready=%t floor=%d budget=%d held=%d reserved=%d", ready, floor, budget, held, reserved)
	}
}

func TestContentInitAndChunkReplayKeepTheirOriginalSession(t *testing.T) {
	manager := NewManager()
	if err := manager.InitializeContentCapacity(1<<20, 1); err != nil {
		t.Fatal(err)
	}
	record, senderKey := createPairedContentChannel(t, manager, "replay", time.Now())
	requestID, _ := NewRequestID()
	manifest := contentManifest(4)
	manifestHash := sha256.Sum256([]byte("fixed manifest"))
	directory := t.TempDir()
	first, err := manager.BeginContent(record.ID, senderKey, requestID, manifest, manifestHash, 4, 100, 1000, 1<<20, directory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := manager.BeginContent(record.ID, senderKey, requestID, manifest, manifestHash, 4, 1, 0, 0, t.TempDir(), time.Now())
	if err != nil || !replay.Replayed || replay.UploadID != first.UploadID {
		t.Fatalf("init replay changed session or quota snapshot: %+v %v", replay, err)
	}
	part := []byte("data")
	hash := sha256.Sum256(part)
	if replayed, err := manager.AppendContentChunk(record.ID, senderKey, first.UploadID, "part1", 0, part, hash); err != nil || replayed {
		t.Fatalf("first chunk = replayed:%t err:%v", replayed, err)
	}
	if replayed, err := manager.AppendContentChunk(record.ID, senderKey, first.UploadID, "part1", 0, part, hash); err != nil || !replayed {
		t.Fatalf("same chunk retry = replayed:%t err:%v", replayed, err)
	}
	if _, err := manager.AppendContentChunk(record.ID, senderKey, first.UploadID, "part1", 0, []byte("evil"), sha256.Sum256([]byte("evil"))); err != ErrIdempotencyConflict {
		t.Fatalf("different bytes at accepted offset = %v", err)
	}
	if _, err := manager.AppendContentChunk(record.ID, senderKey, first.UploadID, "part1", 5, []byte("x"), sha256.Sum256([]byte("x"))); err != ErrContentOffset {
		t.Fatalf("gap offset = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "part1.part")); err != nil {
		t.Fatalf("chunk file not materialized: %v", err)
	}
}

func TestAbortIncompleteContentReleasesSlotReservationAndStaging(t *testing.T) {
	manager := NewManager()
	if err := manager.InitializeContentCapacity(1<<20, 1); err != nil {
		t.Fatal(err)
	}
	record, senderKey := createPairedContentChannel(t, manager, "abort", time.Now())
	requestID, _ := NewRequestID()
	directory := t.TempDir()
	manifest := contentManifest(2)
	manifestHash := sha256.Sum256([]byte("abort-manifest"))
	result, err := manager.BeginContent(record.ID, senderKey, requestID, manifest, manifestHash, 4, 100, 1000, 1<<20, directory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	part := []byte("da")
	if _, err := manager.AppendContentChunk(record.ID, senderKey, result.UploadID, "part1", 0, part, sha256.Sum256(part)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unrelated-stage.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AbortContentUpload(record.ID, senderKey, result.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("abort kept upload staging directory: %v", err)
	}
	ready, _, _, held, reserved := manager.ContentSlotCapacity()
	if !ready || held != 0 || reserved != 0 {
		t.Fatalf("abort kept content reservation: ready=%t held=%d reserved=%d", ready, held, reserved)
	}
	newRequestID, _ := NewRequestID()
	if _, err := manager.BeginContent(record.ID, senderKey, newRequestID, manifest, sha256.Sum256([]byte("new manifest")), 4, 100, 1000, 1<<20, t.TempDir(), time.Now()); err != nil {
		t.Fatalf("content slot was not returned to empty after abort: %v", err)
	}
}

func TestFinalizeHoldsPhysicalBytesUntilContentReleaseCompletes(t *testing.T) {
	manager := NewManager()
	if err := manager.InitializeContentCapacity(1<<20, 1); err != nil {
		t.Fatal(err)
	}
	createID, _ := NewRequestID()
	record, senderKey, _, err := manager.Create("sender:held", createID, VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	joinID, _ := NewRequestID()
	joined, err := manager.Join(record.ID, "account:receiver", joinID, "receiver", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	manifest := contentManifest(4)
	initID, _ := NewRequestID()
	initHash := sha256.Sum256([]byte("held-manifest"))
	sessionResult, err := manager.BeginContent(record.ID, senderKey, initID, manifest, initHash, 4, 100, 1000, 1<<20, t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	part := []byte("held")
	if _, err := manager.AppendContentChunk(record.ID, senderKey, sessionResult.UploadID, "part1", 0, part, sha256.Sum256(part)); err != nil {
		t.Fatal(err)
	}
	finalizeID, _ := NewRequestID()
	session, _, replay, err := manager.BeginContentFinalize(record.ID, senderKey, sessionResult.UploadID, finalizeID)
	if err != nil || replay {
		t.Fatalf("begin finalize = replay:%t err:%v", replay, err)
	}
	if err := manager.CompleteContentFinalize(record.ID, senderKey, session, "HLD55", int64(len(part))); err != nil {
		t.Fatal(err)
	}
	if !manager.ContentActive("HLD55") {
		t.Fatal("finalized content was not marked active")
	}
	ready, _, _, held, reserved := manager.ContentSlotCapacity()
	if !ready || held != int64(len(part)) || reserved != 0 {
		t.Fatalf("finalize did not convert reservation to held bytes: held=%d reserved=%d", held, reserved)
	}
	leaveID, _ := NewRequestID()
	release, err := manager.LeaveWithContent(record.ID, leaveID, joined.SeatToken, time.Now())
	if err != nil || release.Slug != "HLD55" || release.HeldBytes != int64(len(part)) {
		t.Fatalf("leave content release = %+v, %v", release, err)
	}
	if manager.ContentActive("HLD55") {
		t.Fatal("released content remained active")
	}
	_, _, _, held, reserved = manager.ContentSlotCapacity()
	if held != int64(len(part)) || reserved != 0 {
		t.Fatalf("physical bytes were freed before storage deletion: held=%d reserved=%d", held, reserved)
	}
	manager.ContentReleaseDeleted(release)
	_, _, _, held, reserved = manager.ContentSlotCapacity()
	if held != 0 || reserved != 0 {
		t.Fatalf("storage deletion did not release held bytes: held=%d reserved=%d", held, reserved)
	}
}
