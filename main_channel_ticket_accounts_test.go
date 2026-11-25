package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPrivateTicketRechecksRevokedDownloadPermission(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	readerID := createAccount(t, admin, "ticket-reader", gin.H{"download": true})
	reader := signIn(t, router, "ticket-reader", "test-user-password")
	created := accountChannelCreate(router, admin, channelRequestID(t), "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	id := decodeChannelCreate(t, created).ID
	base := "/api/v1/channels/" + id
	headers := map[string]string{"Idempotency-Key": channelRequestID(t), "X-CSRF-Token": reader.csrf, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin"}
	joined := channelRequestWithCookie(router, http.MethodPost, base+"/join", nil, reader.cookie, headers)
	var seat struct {
		Seat  string `json:"seat_token"`
		Claim string `json:"claim_token"`
	}
	if joined.Code != http.StatusCreated || json.Unmarshal(joined.Body.Bytes(), &seat) != nil {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	// The upload fixture uses the actual HTTP authorization middleware with
	// the sender's account session and channel-scoped capability.
	senderRouter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(admin.cookie)
		r.Header.Set("X-CSRF-Token", admin.csrf)
		router.ServeHTTP(w, r)
	})
	f := channelDeliveryFixture{router: senderRouter, channelID: id, contentPath: base + "/content", senderCookie: created.Result().Cookies()[0]}
	uploadChannelFixtureContent(t, f, []byte("private content"))
	headers["X-Nclip-Seat-Token"], headers["X-Nclip-Claim-Token"] = seat.Seat, seat.Claim
	issue := channelRequestWithCookie(router, http.MethodPost, base+"/content/ticket", nil, reader.cookie, headers)
	var ticket struct {
		URL string `json:"download_url"`
	}
	if issue.Code != http.StatusCreated || json.Unmarshal(issue.Body.Bytes(), &ticket) != nil {
		t.Fatalf("issue: %d %s", issue.Code, issue.Body.String())
	}
	// A valid ticket alone never supplies login for a private channel.
	if r := channelRequestWithCookies(router, http.MethodGet, ticket.URL, nil, nil, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous ticket GET: %d", r.Code)
	}
	partial := channelRequestWithCookie(router, http.MethodGet, ticket.URL, nil, reader.cookie, map[string]string{"Range": "bytes=0-2"})
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("valid ticket GET: %d %s", partial.Code, partial.Body.String())
	}
	revoked := admin.call(http.MethodPut, "/api/v1/users/"+readerID, gin.H{"username": "ticket-reader", "permissions": gin.H{"download": false}, "version": 1}, true)
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", revoked.Code, revoked.Body.String())
	}
	stale := channelRequestWithCookie(router, http.MethodGet, ticket.URL, nil, reader.cookie, nil)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session GET: %d %s", stale.Code, stale.Body.String())
	}
	reader = signIn(t, router, "ticket-reader", "test-user-password")
	denied := channelRequestWithCookie(router, http.MethodGet, ticket.URL, nil, reader.cookie, nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("download revoked GET: %d %s", denied.Code, denied.Body.String())
	}
	headers["X-CSRF-Token"] = reader.csrf
	denied = channelRequestWithCookie(router, http.MethodPost, base+"/content/ticket", nil, reader.cookie, headers)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("download revoked issuance: %d %s", denied.Code, denied.Body.String())
	}
}
