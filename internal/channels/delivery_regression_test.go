package channels

import (
	"testing"
	"time"
)

func TestRegressionModeConflictRestoresReleasedSeat(t *testing.T) {
	now := time.Now()
	m, r, _, j := heldDeliveryFixture(t, now)
	ticket, err := m.IssueDeliveryTicket(r.ID, j.SeatToken, j.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.BeginTicketDelivery(r.ID, ticket, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = m.FinishDelivery(first.Lease, 4, false, now)
	later := now.Add(ReceiverHeartbeatTimeout + time.Second)
	m.SweepReceiverLiveness(later)
	if _, err = m.BeginDelivery(r.ID, j.SeatToken, j.ClaimToken, later); err != ErrDeliveryModeConflict {
		t.Fatal(err)
	}
	if start, err := m.BeginTicketDelivery(r.ID, ticket, later); err == nil {
		t.Fatalf("failed cross-mode GET restored released seat; native lease admitted: %v", start.Lease != nil)
	}
}

func TestRegressionReleasedRecoveryRetainsUnwindingLease(t *testing.T) {
	now := time.Now()
	m, r, _, j := heldDeliveryFixture(t, now)
	ticket, err := m.IssueDeliveryTicket(r.ID, j.SeatToken, j.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.BeginTicketDelivery(r.ID, ticket, now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(ReceiverHeartbeatTimeout + time.Second)
	m.SweepReceiverLiveness(later)
	if first.Lease.Context().Err() == nil {
		t.Fatal("old lease not canceled")
	}
	if _, err := m.ReceiverStatus(r.ID, j.SeatToken, later); err != nil {
		t.Fatal(err)
	}
	if start, err := m.BeginTicketDelivery(r.ID, ticket, later); err != ErrDeliveryBusy {
		t.Fatalf("replacement admitted before canceled lease Finish: err=%v newLease=%v", err, start.Lease != nil)
	}
	if _, done, err := m.FinishDelivery(first.Lease, 4, true, later); done || err != ErrContentClosed {
		t.Fatalf("cancelled finish: %t %v", done, err)
	}
	second, err := m.BeginTicketDelivery(r.ID, ticket, later)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, _ := m.FinishDelivery(first.Lease, 0, true, later); done {
		t.Fatal("late old finish settled replacement")
	}
	state, _ := m.Get(r.ID)
	if state.seat.delivery != second.Lease || state.ContentState != ContentHeld {
		t.Fatal("old finish changed replacement")
	}
}

func TestRegressionSuccessfulDeliveryDropsUploadRegistry(t *testing.T) {
	now := time.Now()
	m, r, _, j := heldDeliveryFixture(t, now)
	ticket, err := m.IssueDeliveryTicket(r.ID, j.SeatToken, j.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	start, err := m.BeginTicketDelivery(r.ID, ticket, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RecordDeliveryBytes(start.Lease, 4, now); err != nil {
		t.Fatal(err)
	}
	release, done, err := m.FinishDelivery(start.Lease, 4, true, now)
	if err != nil || !done {
		t.Fatalf("finish: %v %v", done, err)
	}
	m.ContentReleaseDeleted(release)
	if len(m.contentUploads) != 0 {
		t.Fatalf("success/delete leaves %d contentUploads record(s)", len(m.contentUploads))
	}
}

func TestSenderCanKickReleasedSeatWithoutWaitingForRecovery(t *testing.T) {
	now := time.Now()
	m, record, sender, joined := heldDeliveryFixture(t, now)
	start, err := m.BeginDelivery(record.ID, joined.SeatToken, joined.ClaimToken, now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(ReceiverHeartbeatTimeout + time.Second)
	m.SweepReceiverLiveness(later)
	state, _ := m.Get(record.ID)
	if state.Seat.State != "released" {
		t.Fatal("seat did not become released")
	}
	key, _ := NewRequestID()
	release, err := m.KickWithContent(record.ID, sender, state.Seat.SeatID, key, later)
	if err != nil || release.Slug == "" {
		t.Fatalf("kick released seat: %v", err)
	}
	if start.Lease.Context().Err() == nil {
		t.Fatal("released delivery was not canceled")
	}
	after, _ := m.Get(record.ID)
	if after.Seat != nil || after.ContentState != ContentEmpty {
		t.Fatal("kick retained released seat or content")
	}
	m.ContentReleaseDeleted(release)
	_, _, _, held, _ := m.ContentSlotCapacity()
	if held != 0 {
		t.Fatal("successful deletion did not release held bytes")
	}
}
