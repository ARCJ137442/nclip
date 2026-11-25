package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/johnwmail/nclip/internal/channels"
)

func TestAnonymousRejectionReturnsCompatibleBoundReason(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	created := channelCreateRequest(router, channelRequestID(t), nil)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	id := decodeChannelCreate(t, created).ID
	base := "/api/v1/channels/" + id
	cookie := created.Result().Cookies()[0]
	joined := channelJoinRequest(router, id, channelRequestID(t), "", nil)
	var seat struct {
		Token string `json:"seat_token"`
	}
	if json.Unmarshal(joined.Body.Bytes(), &seat) != nil || seat.Token == "" {
		t.Fatal("missing seat")
	}
	status := channelRequestWithCookie(router, http.MethodGet, base, nil, cookie, nil)
	var sender struct {
		Seat *channels.SeatInfo `json:"seat"`
	}
	if json.Unmarshal(status.Body.Bytes(), &sender) != nil || sender.Seat == nil {
		t.Fatal("missing pending seat")
	}
	body, _ := json.Marshal(map[string]string{"seat_id": sender.Seat.SeatID, "decision": "reject"})
	decision := channelRequestWithCookie(router, http.MethodPost, base+"/admit", body, cookie, map[string]string{"Content-Type": "application/json", "Idempotency-Key": channelRequestID(t), "Origin": "https://clip.test"})
	if decision.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", decision.Code, decision.Body.String())
	}
	for i := 0; i < 2; i++ {
		assertRejectionHTTP(t, router, base, seat.Token)
	}
	assertNoRejectionHTTP(t, router, base, "forged-token", http.StatusForbidden)
	other := channelCreateRequest(router, channelRequestID(t), nil)
	otherID := decodeChannelCreate(t, other).ID
	assertNoRejectionHTTP(t, router, "/api/v1/channels/"+otherID, seat.Token, http.StatusForbidden)
	public := channelRequestWithCookies(router, http.MethodGet, base, nil, nil, nil)
	var anonymous map[string]any
	if json.Unmarshal(public.Body.Bytes(), &anonymous) != nil || anonymous["reason"] != nil {
		t.Fatal("public status disclosed reason")
	}
	owner := channelRequestWithCookie(router, http.MethodGet, base, nil, cookie, nil)
	var ownerBody map[string]any
	if json.Unmarshal(owner.Body.Bytes(), &ownerBody) != nil || ownerBody["reason"] != nil {
		t.Fatal("sender status disclosed receiver reason")
	}
}

func assertRejectionHTTP(t *testing.T, router http.Handler, base, token string) {
	t.Helper()
	r := channelRequestWithCookies(router, http.MethodGet, base, nil, nil, map[string]string{"X-Nclip-Seat-Token": token})
	var body map[string]any
	if r.Code != http.StatusGone || json.Unmarshal(r.Body.Bytes(), &body) != nil || body["error"] != "join_closed" || body["reason"] != "sender_rejected" {
		t.Fatalf("rejected status: %d %s", r.Code, r.Body.String())
	}
}

func assertNoRejectionHTTP(t *testing.T, router http.Handler, base, token string, status int) {
	t.Helper()
	r := channelRequestWithCookies(router, http.MethodGet, base, nil, nil, map[string]string{"X-Nclip-Seat-Token": token})
	var body map[string]any
	if r.Code != status || json.Unmarshal(r.Body.Bytes(), &body) != nil || body["reason"] != nil {
		t.Fatalf("invalid seat disclosed reason: %d %s", r.Code, r.Body.String())
	}
}

func TestGenericJoinClosedDoesNotBecomeRejection(t *testing.T) {
	fixture := newChannelDeliveryFixture(t, []byte("data"))
	leave := channelRequestWithCookies(fixture.router, http.MethodPost, "/api/v1/channels/"+fixture.channelID+"/leave", nil, nil, map[string]string{"X-Nclip-Seat-Token": fixture.seatToken, "Idempotency-Key": channelRequestID(t), "Origin": "https://clip.test"})
	if leave.Code != http.StatusNoContent {
		t.Fatal(leave.Body.String())
	}
	assertNoRejectionHTTP(t, fixture.router, "/api/v1/channels/"+fixture.channelID, fixture.seatToken, http.StatusGone)
}
