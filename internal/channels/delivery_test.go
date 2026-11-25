package channels

import (
	"crypto/sha256"
	"sync"
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
	first := assertDeliveryClaimAndConcurrency(t, manager, record, joined, now)
	finishPartialDelivery(t, manager, record, first, now)
	release := finishResumedDelivery(t, manager, record, joined, now.Add(2*time.Second))
	if release.Slug != "DLV52" {
		t.Fatalf("completed delivery released wrong content: %+v", release)
	}
	manager.ContentReleaseDeleted(release)
	_, _, _, held, _ := manager.ContentSlotCapacity()
	if held != 0 {
		t.Fatalf("delivery deletion did not release held capacity: %d", held)
	}
}

func TestDeliveryRepresentationLocksOnFirstSuccessfulLease(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	initial, _ := manager.Get(record.ID)
	lastSeen := initial.seat.lastSeen
	ticket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(time.Second))
	if err != nil || ticket == "" {
		t.Fatalf("issue ticket = %q, %v", ticket, err)
	}
	issued, _ := manager.Get(record.ID)
	if issued.seat.deliveryMode != DeliveryModeUnset || !issued.seat.lastSeen.Equal(lastSeen) {
		t.Fatalf("ticket issuance changed mode or liveness: mode=%q lastSeen=%v", issued.seat.deliveryMode, issued.seat.lastSeen)
	}

	first, err := manager.BeginTicketDelivery(record.ID, ticket, now.Add(2*time.Second))
	if err != nil || first.Lease.Mode != DeliveryModeBrowserAttachment {
		t.Fatalf("first ticket lease = %+v, %v", first, err)
	}
	if _, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(3*time.Second)); err != ErrDeliveryModeConflict {
		t.Fatalf("cross-representation app GET should conflict: %v", err)
	}
	if _, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(3*time.Second)); err != nil {
		t.Fatalf("rotate active ticket: %v", err)
	}
	_, _, finishErr := manager.FinishDelivery(first.Lease, first.ContentBytes, true, now.Add(4*time.Second))
	if finishErr != ErrContentClosed {
		t.Fatalf("cancelled lease finished as success: %v", finishErr)
	}
	state, _ := manager.Get(record.ID)
	if state.seat == nil || state.ContentState != ContentHeld || state.seat.deliveryMode != DeliveryModeBrowserAttachment {
		t.Fatalf("late cancelled finish changed ticket seat/content state: %+v", state)
	}

	rotated, err := manager.BeginTicketDelivery(record.ID, ticket, now.Add(5*time.Second))
	if err != ErrUnauthorized || rotated.Lease != nil {
		t.Fatalf("old rotated ticket remained usable: start=%+v err=%v", rotated, err)
	}
	newTicket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.BeginTicketDelivery(record.ID, newTicket, now.Add(6*time.Second))
	if err != nil || resumed.Lease.Mode != DeliveryModeBrowserAttachment {
		t.Fatalf("rotated ticket did not resume in same representation: %+v, %v", resumed, err)
	}
	if _, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(7*time.Second)); err != ErrDeliveryModeConflict {
		t.Fatalf("app representation switched after ticket lock: %v", err)
	}
}

func TestTicketIssueDoesNotCancelOrReplaceAnAppLease(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	app, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(time.Second)); err != ErrDeliveryModeConflict {
		t.Fatalf("ticket issuance over an app lease = %v", err)
	}
	if app.Lease.Context().Err() != nil {
		t.Fatal("failed ticket issuance cancelled the app lease")
	}
}

func TestTicketAndAppFirstGETCompeteForOneRepresentation(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	ticket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var starts [2]DeliveryStart
	var errs [2]error
	gate := make(chan struct{})
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-gate
		starts[0], errs[0] = manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	}()
	go func() {
		defer wait.Done()
		<-gate
		starts[1], errs[1] = manager.BeginTicketDelivery(record.ID, ticket, now)
	}()
	close(gate)
	wait.Wait()
	for i := range starts {
		if errs[i] == nil {
			if starts[i].Lease == nil || errs[1-i] != ErrDeliveryModeConflict {
				t.Fatalf("first GET competition: %v", errs)
			}
			return
		}
	}
	t.Fatalf("neither GET acquired delivery: %v", errs)
}

func TestTicketCannotCrossChannelOrSurviveKick(t *testing.T) {
	now := time.Now()
	manager, record, sender, joined := heldDeliveryFixture(t, now)
	ticket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	createID, _ := NewRequestID()
	other, _, _, err := manager.Create("other-sender", createID, VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginTicketDelivery(other.ID, ticket, now); err != ErrUnauthorized {
		t.Fatalf("cross-channel ticket: %v", err)
	}
	start, err := manager.BeginTicketDelivery(record.ID, ticket, now)
	if err != nil {
		t.Fatal(err)
	}
	kickID, _ := NewRequestID()
	state, _ := manager.Get(record.ID)
	if _, err := manager.KickWithContent(record.ID, sender, state.seat.id, kickID, now); err != nil {
		t.Fatal(err)
	}
	if start.Lease.Context().Err() == nil {
		t.Fatal("kick did not cancel native lease")
	}
	if _, err := manager.BeginTicketDelivery(record.ID, ticket, now); err != ErrUnauthorized {
		t.Fatalf("ticket after kick: %v", err)
	}
	if _, delivered, _ := manager.FinishDelivery(start.Lease, 0, true, now); delivered {
		t.Fatal("late kicked lease settled delivery")
	}
}

func TestTicketDoesNotRecoverReleasedSeatButSurvivesPageHeartbeatRecovery(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	ticket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	manager.SweepReceiverLiveness(now.Add(ReceiverHeartbeatTimeout + time.Second))
	if _, err := manager.BeginTicketDelivery(record.ID, ticket, now.Add(ReceiverHeartbeatTimeout+2*time.Second)); err != ErrDeliveryNotReady {
		t.Fatalf("ticket GET recovered inactive seat: %v", err)
	}
	released, _ := manager.Get(record.ID)
	if released.Seat == nil || released.Seat.State != "released" || released.seat.ticketHash == ([sha256.Size]byte{}) {
		t.Fatalf("released recovery window lost ticket binding: %+v", released)
	}
	if _, err := manager.ReceiverStatus(record.ID, joined.SeatToken, now.Add(ReceiverHeartbeatTimeout+3*time.Second)); err != nil {
		t.Fatalf("page status did not restore the seat: %v", err)
	}
	start, err := manager.BeginTicketDelivery(record.ID, ticket, now.Add(ReceiverHeartbeatTimeout+4*time.Second))
	if err != nil || start.Lease == nil {
		t.Fatalf("same page ticket did not work after page recovery: %+v, %v", start, err)
	}
}

func TestTicketRotationAndPageReleaseInvalidateOldTicket(t *testing.T) {
	now := time.Now()
	manager, record, _, joined := heldDeliveryFixture(t, now)
	oldTicket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	start, err := manager.BeginTicketDelivery(record.ID, oldTicket, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	interrupted := make(chan struct{})
	start.Lease.SetInterrupt(func() { close(interrupted) })
	newTicket, err := manager.IssueDeliveryTicket(record.ID, joined.SeatToken, joined.ClaimToken, now.Add(2*time.Second))
	if err != nil || newTicket == oldTicket {
		t.Fatalf("ticket rotation = %q, %v", newTicket, err)
	}
	select {
	case <-start.Lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("ticket rotation did not cancel the old native response")
	}
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("ticket rotation did not interrupt the old response writer")
	}
	if _, err := manager.BeginTicketDelivery(record.ID, newTicket, now.Add(3*time.Second)); err != ErrDeliveryBusy {
		t.Fatalf("new ticket bypassed the unwinding old lease: %v", err)
	}
	if _, _, err := manager.FinishDelivery(start.Lease, start.ContentBytes, true, now.Add(4*time.Second)); err != ErrContentClosed {
		t.Fatalf("cancelled old lease settled delivery: %v", err)
	}
	leaveID, err := NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Leave(record.ID, leaveID, joined.SeatToken, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginTicketDelivery(record.ID, newTicket, now.Add(6*time.Second)); err != ErrUnauthorized {
		t.Fatalf("ticket remained valid after page leave: %v", err)
	}
}

func assertDeliveryClaimAndConcurrency(t *testing.T, manager *Manager, record Record, joined JoinResult, now time.Time) *DeliveryLease {
	t.Helper()
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
	return first.Lease
}

func finishPartialDelivery(t *testing.T, manager *Manager, record Record, first *DeliveryLease, now time.Time) {
	t.Helper()
	if err := manager.SetDeliveryRange(first, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordDeliveryBytes(first, 4, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, complete, err := manager.FinishDelivery(first, 10, false, now.Add(time.Second)); err != nil || complete {
		t.Fatalf("partial delivery finish = complete:%t err:%v", complete, err)
	}
	current, _ := manager.Get(record.ID)
	if current.ContentState != ContentHeld || current.seat == nil || current.seat.deliveredThrough != 4 {
		t.Fatalf("partial delivery lost its content/offset: state=%s seat=%+v", current.ContentState, current.seat)
	}
}

func finishResumedDelivery(t *testing.T, manager *Manager, record Record, joined JoinResult, now time.Time) ContentRelease {
	t.Helper()
	second, err := manager.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDeliveryRange(second.Lease, 5); err != ErrDeliveryGap {
		t.Fatalf("range skipped unsent bytes: %v", err)
	}
	if err := manager.SetDeliveryRange(second.Lease, 4); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordDeliveryBytes(second.Lease, 6, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	release, complete, err := manager.FinishDelivery(second.Lease, 10, true, now.Add(time.Second))
	if err != nil || !complete || release.Slug != "DLV52" {
		t.Fatalf("completed delivery = release:%+v complete:%t err:%v", release, complete, err)
	}
	current, _ := manager.Get(record.ID)
	if current.ContentState != ContentEmpty || current.seat != nil {
		t.Fatalf("completed delivery did not release channel: %+v", current)
	}
	return release
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
	_, err = manager.Join(record.ID, "account:next", newJoinID, "next", now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("kick did not release the seat: %v", err)
	}
	replacement, _ := manager.Get(record.ID)
	if replacement.Seat == nil {
		t.Fatal("replacement seat was not created")
	}
	replacementSeatID := replacement.Seat.SeatID
	if _, _, err := manager.FinishDelivery(start.Lease, start.ContentBytes, true, now.Add(4*time.Second)); err != ErrContentClosed {
		t.Fatalf("late kicked delivery settled after replacement seat joined: %v", err)
	}
	current, _ := manager.Get(record.ID)
	if current.Seat == nil || current.Seat.SeatID != replacementSeatID || current.ContentState != ContentEmpty {
		t.Fatalf("late old lease changed replacement seat/content: %+v", current)
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
