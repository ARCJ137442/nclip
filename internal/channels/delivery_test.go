package channels

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/johnwmail/nclip/models"
)

func heldDeliveryFixture(t *testing.T, now time.Time) (*Manager, Record, string, JoinResult) {
	t.Helper()
	manager := NewManager()
	if err := manager.InitializeContentCapacity(1<<20, 1); err != nil {
		t.Fatal(err)
	}
	createID, _ := NewRequestID()
	record, senderKey, _, err := manager.Create("sender:delivery", createID, VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	joinID, _ := NewRequestID()
	joined, err := manager.Join(record.ID, "account:receiver", joinID, "receiver", now)
	if err != nil {
		t.Fatal(err)
	}
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "data.bin", Parts: []models.SharePart{{Key: "part1", MIME: "application/octet-stream", Size: 4}},
	}}}
	initID, _ := NewRequestID()
	init, err := manager.BeginContent(record.ID, senderKey, initID, manifest, sha256.Sum256([]byte("manifest")), 4, 16, 100, 1<<20, t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("data")
	if _, err := manager.AppendContentChunk(record.ID, senderKey, init.UploadID, "part1", 0, data, sha256.Sum256(data)); err != nil {
		t.Fatal(err)
	}
	finalizeID, _ := NewRequestID()
	session, _, replayed, err := manager.BeginContentFinalize(record.ID, senderKey, init.UploadID, finalizeID)
	if err != nil || replayed {
		t.Fatalf("begin delivery fixture finalize: replayed=%t err=%v", replayed, err)
	}
	if err := session.RemoveStaging(); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteContentFinalize(record.ID, senderKey, session, "DLV52", 80); err != nil {
		t.Fatal(err)
	}
	return manager, record, senderKey, joined
}

func TestClaimBoundRangeDeliveryResumesAndDeletesAfterCompleteBody(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	if _, err := manager.BeginDelivery(record.ID, joined.SeatToken, "wrong-claim", now); err != ErrUnauthorized {
		t.Fatalf("wrong claim token = %v", err)
	}
	first, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now); err != ErrDeliveryBusy {
		t.Fatalf("parallel request for one claim = %v", err)
	}
	if err := manager.SetDeliveryRange(first.Lease, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordDeliveryBytes(first.Lease, 4, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, complete, err := manager.FinishDelivery(first.Lease, 10, false, now.Add(time.Second)); err != nil || complete {
		t.Fatalf("partial delivery finish = complete:%t err:%v", complete, err)
	}
	current, _ := manager.Get(record.ID)
	if current.ContentState != ContentHeld || current.seat == nil || current.seat.deliveredThrough != 4 {
		t.Fatalf("partial delivery lost its content/offset: state=%s seat=%+v", current.ContentState, current.seat)
	}

	second, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDeliveryRange(second.Lease, 5); err != ErrDeliveryGap {
		t.Fatalf("range skipped unsent bytes: %v", err)
	}
	if err := manager.SetDeliveryRange(second.Lease, 4); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordDeliveryBytes(second.Lease, 6, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	release, complete, err := manager.FinishDelivery(second.Lease, 10, true, now.Add(3*time.Second))
	if err != nil || !complete || release.Slug != "DLV52" {
		t.Fatalf("completed delivery = release:%+v complete:%t err:%v", release, complete, err)
	}
	current, _ = manager.Get(record.ID)
	if current.ContentState != ContentEmpty || current.seat != nil {
		t.Fatalf("completed delivery did not release channel: %+v", current)
	}
	manager.ContentReleaseDeleted(release)
	_, _, _, held, _ := manager.ContentSlotCapacity()
	if held != 0 {
		t.Fatalf("delivery deletion did not release held capacity: %d", held)
	}
}

func TestKickCancelsActiveDeliveryAndIsBoundToItsSeat(t *testing.T) {
	now := time.Now()
	manager, record, senderKey, joined := heldDeliveryFixture(t, now)
	start, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := make(chan struct{})
	start.Lease.SetInterrupt(func() { close(interrupted) })
	seat, _ := manager.Get(record.ID)
	kickID, _ := NewRequestID()
	release, err := manager.KickWithContent(record.ID, senderKey, seat.Seat.SeatID, kickID, now.Add(time.Second))
	if err != nil || release.Slug != "DLV52" {
		t.Fatalf("kick = release:%+v err:%v", release, err)
	}
	select {
	case <-start.Lease.Context().Done():
	default:
		t.Fatal("kick did not cancel the active delivery context")
	}
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("kick did not interrupt the active response writer")
	}
	if _, err := manager.KickWithContent(record.ID, senderKey, seat.Seat.SeatID, kickID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("same kick retry did not replay: %v", err)
	}
	newJoinID, _ := NewRequestID()
	if _, err := manager.Join(record.ID, "account:next", newJoinID, "next", now.Add(3*time.Second)); err != nil {
		t.Fatalf("kick did not release the seat: %v", err)
	}
}

func TestCloseCancelsActiveDelivery(t *testing.T) {
	now := time.Now()
	manager, record, senderKey, joined := heldDeliveryFixture(t, now)
	start, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := make(chan struct{})
	start.Lease.SetInterrupt(func() { close(interrupted) })
	release, err := manager.CloseWithContent(record.ID, senderKey)
	if err != nil || release.Slug != "DLV52" {
		t.Fatalf("close = release:%+v err:%v", release, err)
	}
	select {
	case <-start.Lease.Context().Done():
	default:
		t.Fatal("closing the channel did not cancel the active delivery")
	}
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("closing the channel did not interrupt the active response writer")
	}
	if current, ok := manager.Get(record.ID); ok {
		t.Fatalf("close left channel active: %+v", current)
	}
}

func TestReceiverLivenessReleaseUsesHeartbeatAndGraceWindow(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	if releases := manager.SweepReceiverLiveness(now.Add(ReceiverHeartbeatTimeout - time.Second)); len(releases) != 0 {
		t.Fatalf("receiver released before heartbeat timeout: %+v", releases)
	}
	if releases := manager.SweepReceiverLiveness(now.Add(ReceiverHeartbeatTimeout + time.Second)); len(releases) != 0 {
		t.Fatalf("receiver content released before grace window: %+v", releases)
	}
	state, _ := manager.Get(record.ID)
	if state.Seat == nil || state.Seat.State != "released" || state.ContentState != ContentHeld {
		t.Fatalf("heartbeat loss did not enter released state: %+v", state)
	}
	if err := manager.SeatHeartbeat(record.ID, joined.SeatToken, now.Add(ReceiverHeartbeatTimeout+ChannelReleaseWindow-time.Second)); err != nil {
		t.Fatalf("same seat could not restore within release window: %v", err)
	}
	state, _ = manager.Get(record.ID)
	if state.Seat == nil || state.Seat.State != "paired" {
		t.Fatalf("resume did not restore seat: %+v", state)
	}
}
