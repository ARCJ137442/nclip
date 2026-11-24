package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

func channelCreateRequest(router http.Handler, requestID string, headers map[string]string) *httptest.ResponseRecorder {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Idempotency-Key"] = requestID
	return shareRequest(router, http.MethodPost, "/api/v1/channels", nil, headers)
}

func decodeChannelCreate(t *testing.T, response *httptest.ResponseRecorder) struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	Visibility string `json:"visibility"`
} {
	t.Helper()
	var body struct {
		ID         string `json:"id"`
		URL        string `json:"url"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode channel response: %v: %s", err, response.Body.String())
	}
	return body
}

func accountChannelCreate(router http.Handler, client accountClient, requestID, visibility string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/channels", nil)
	request.Header.Set("Idempotency-Key", requestID)
	request.Header.Set("Origin", "https://clip.test")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("X-CSRF-Token", client.csrf)
	if visibility != "" {
		request.Header.Set("X-Nclip-Visibility", visibility)
	}
	request.AddCookie(client.cookie)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestChannelCreateCookieScopeAndIdempotency(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	requestID := "AQEBAQEBAQEBAQEBAQEBAQ"
	headers := map[string]string{"X-Nclip-Visibility": "public"}
	first := channelCreateRequest(router, requestID, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.Code, first.Body.String())
	}
	body := decodeChannelCreate(t, first)
	if len(body.ID) != 22 || body.Visibility != "public" {
		t.Fatalf("unexpected channel: %+v", body)
	}
	if body.URL != "https://clip.test/c/"+body.ID {
		t.Fatalf("channel URL = %q", body.URL)
	}
	if strings.Contains(first.Body.String(), "nclip_sender_key") {
		t.Fatal("sender capability leaked into response body")
	}
	cookies := first.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d", len(cookies))
	}
	cookie := cookies[0]
	if strings.Contains(first.Body.String(), cookie.Value) {
		t.Fatal("sender capability leaked into response body")
	}
	if cookie.Name != "nclip_sender_key" || cookie.Path != "/api/v1/channels/"+body.ID || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Fatalf("unsafe sender cookie: %+v", cookie)
	}

	retry := channelCreateRequest(router, requestID, headers)
	if retry.Code != http.StatusCreated || decodeChannelCreate(t, retry).ID != body.ID {
		t.Fatalf("idempotent retry: %d %s", retry.Code, retry.Body.String())
	}
	if retry.Result().Cookies()[0].Value != cookie.Value {
		t.Fatal("idempotent retry changed sender capability")
	}

	status := shareRequest(router, http.MethodGet, "/api/v1/channels/"+body.ID, nil, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status: %d %s", status.Code, status.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || state["state"] != "open" {
		t.Fatalf("unauthenticated status exposed extra data: %v", state)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/c/"+body.ID, nil)
	pageRequest.Header.Set("Accept", "text/html")
	page := httptest.NewRecorder()
	router.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), body.ID) {
		t.Fatalf("channel page: %d %s", page.Code, page.Body.String())
	}
}

func TestLambdaModeFailsClosedForProcessLocalChannels(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "isolated-channel-test")
	router := shareTestRouter(t, 4<<20, false)
	requestID, _ := channels.NewRequestID()
	response := channelCreateRequest(router, requestID, nil)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "channel_unavailable") {
		t.Fatalf("Lambda channel create = %d %s", response.Code, response.Body.String())
	}
}

func TestChannelCreateRequiresIdempotencyKey(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	response := shareRequest(router, http.MethodPost, "/api/v1/channels", nil, nil)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_idempotency_key") {
		t.Fatalf("missing key: %d %s", response.Code, response.Body.String())
	}
}

func TestChannelUploadAuthDoesNotProtectPublicRead(t *testing.T) {
	router := shareTestRouter(t, 4<<20, true)
	requestID := "AgICAgICAgICAgICAgICAg"
	if response := channelCreateRequest(router, requestID, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("create without UploadAuth: %d", response.Code)
	}
	response := channelCreateRequest(router, requestID, map[string]string{"X-Api-Key": "test-key"})
	if response.Code != http.StatusCreated {
		t.Fatalf("create with UploadAuth: %d %s", response.Code, response.Body.String())
	}
	body := decodeChannelCreate(t, response)
	status := shareRequest(router, http.MethodGet, "/api/v1/channels/"+body.ID, nil, nil)
	page := shareRequest(router, http.MethodGet, "/c/"+body.ID, nil, map[string]string{"Accept": "text/html"})
	if status.Code != http.StatusOK || page.Code != http.StatusOK {
		t.Fatalf("public reads required upload key: status=%d page=%d", status.Code, page.Code)
	}
}

func TestAccountChannelVisibilityAndLoggedPublicAccess(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	privateResponse := accountChannelCreate(router, admin, "AwMDAwMDAwMDAwMDAwMDAw", "")
	if privateResponse.Code != http.StatusCreated {
		t.Fatalf("private create: %d %s", privateResponse.Code, privateResponse.Body.String())
	}
	privateChannel := decodeChannelCreate(t, privateResponse)
	if privateChannel.Visibility != "authenticated" {
		t.Fatalf("default visibility = %q", privateChannel.Visibility)
	}

	anonymousPage := httptest.NewRequest(http.MethodGet, "/c/"+privateChannel.ID, nil)
	anonymousPage.Header.Set("Accept", "text/html")
	anonymousResponse := httptest.NewRecorder()
	router.ServeHTTP(anonymousResponse, anonymousPage)
	if anonymousResponse.Code != http.StatusSeeOther || !strings.Contains(anonymousResponse.Header().Get("Location"), "/login?next=") {
		t.Fatalf("private channel anonymous access = %d %s", anonymousResponse.Code, anonymousResponse.Header().Get("Location"))
	}

	publicResponse := accountChannelCreate(router, admin, "BAQEBAQEBAQEBAQEBAQEBA", "public")
	if publicResponse.Code != http.StatusCreated {
		t.Fatalf("public create: %d %s", publicResponse.Code, publicResponse.Body.String())
	}
	publicChannel := decodeChannelCreate(t, publicResponse)
	anonymousPublic := httptest.NewRequest(http.MethodGet, "/c/"+publicChannel.ID, nil)
	anonymousPublic.Header.Set("Accept", "text/html")
	anonymousPublicResponse := httptest.NewRecorder()
	router.ServeHTTP(anonymousPublicResponse, anonymousPublic)
	if anonymousPublicResponse.Code != http.StatusOK {
		t.Fatalf("public anonymous access = %d", anonymousPublicResponse.Code)
	}

	_ = createAccount(t, admin, "reader", gin.H{"upload": false, "download": true, "manage": false})
	reader := signIn(t, router, "reader", "test-user-password")
	loggedPublic := httptest.NewRequest(http.MethodGet, "/c/"+publicChannel.ID, nil)
	loggedPublic.Header.Set("Accept", "text/html")
	loggedPublic.AddCookie(reader.cookie)
	loggedPublicResponse := httptest.NewRecorder()
	router.ServeHTTP(loggedPublicResponse, loggedPublic)
	if loggedPublicResponse.Code != http.StatusOK {
		t.Fatalf("logged public channel access = %d", loggedPublicResponse.Code)
	}
}

func TestAccountChannelRequiresUploadAndPrivateReadRequiresDownload(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	_ = createAccount(t, admin, "uploader", gin.H{"upload": true, "download": false, "manage": false})
	_ = createAccount(t, admin, "reader", gin.H{"upload": false, "download": true, "manage": false})
	uploader := signIn(t, router, "uploader", "test-user-password")
	reader := signIn(t, router, "reader", "test-user-password")

	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/channels", nil)
	missingCSRF.Header.Set("Idempotency-Key", "CwsLCwsLCwsLCwsLCwsLCw")
	missingCSRF.Header.Set("Origin", "https://clip.test")
	missingCSRF.AddCookie(uploader.cookie)
	missingCSRFResponse := httptest.NewRecorder()
	router.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d", missingCSRFResponse.Code)
	}

	created := accountChannelCreate(router, uploader, "BQUFBQUFBQUFBQUFBQUFBQ", "")
	if created.Code != http.StatusCreated {
		t.Fatalf("upload-only create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)

	secondCreated := accountChannelCreate(router, uploader, "CgoKCgoKCgoKCgoKCgoKCg", "")
	if secondCreated.Code != http.StatusCreated {
		t.Fatalf("second sender channel: %d %s", secondCreated.Code, secondCreated.Body.String())
	}
	wrongSender := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	wrongSender.AddCookie(uploader.cookie)
	wrongSender.AddCookie(secondCreated.Result().Cookies()[0])
	wrongSenderResponse := httptest.NewRecorder()
	router.ServeHTTP(wrongSenderResponse, wrongSender)
	if wrongSenderResponse.Code != http.StatusForbidden {
		t.Fatalf("other channel sender cookie = %d", wrongSenderResponse.Code)
	}

	ownSender := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	ownSender.AddCookie(created.Result().Cookies()[0])
	ownSenderResponse := httptest.NewRecorder()
	router.ServeHTTP(ownSenderResponse, ownSender)
	if ownSenderResponse.Code != http.StatusOK {
		t.Fatalf("sender capability private status = %d %s", ownSenderResponse.Code, ownSenderResponse.Body.String())
	}

	if denied := accountChannelCreate(router, reader, "BgYGBgYGBgYGBgYGBgYGBg", ""); denied.Code != http.StatusForbidden {
		t.Fatalf("download-only create = %d %s", denied.Code, denied.Body.String())
	}
	anonymousStatus := shareRequest(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, nil)
	if anonymousStatus.Code != http.StatusUnauthorized {
		t.Fatalf("private status without login = %d", anonymousStatus.Code)
	}

	uploaderPageRequest := httptest.NewRequest(http.MethodGet, "/c/"+channel.ID, nil)
	uploaderPageRequest.Header.Set("Accept", "text/html")
	uploaderPageRequest.AddCookie(uploader.cookie)
	uploaderPageRequest.AddCookie(created.Result().Cookies()[0])
	uploaderPage := httptest.NewRecorder()
	router.ServeHTTP(uploaderPage, uploaderPageRequest)
	if uploaderPage.Code != http.StatusForbidden {
		t.Fatalf("upload-only private page = %d %s", uploaderPage.Code, uploaderPage.Body.String())
	}

	readerPageRequest := httptest.NewRequest(http.MethodGet, "/c/"+channel.ID, nil)
	readerPageRequest.Header.Set("Accept", "text/html")
	readerPageRequest.AddCookie(reader.cookie)
	readerPage := httptest.NewRecorder()
	router.ServeHTTP(readerPage, readerPageRequest)
	if readerPage.Code != http.StatusOK {
		t.Fatalf("download user private page = %d %s", readerPage.Code, readerPage.Body.String())
	}
}

func TestChannelCreationRateLimitAndIdempotentReplay(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	firstID := ""
	firstChannel := ""
	for i := 0; i < 30; i++ {
		requestID, err := channels.NewRequestID()
		if err != nil {
			t.Fatal(err)
		}
		response := channelCreateRequest(router, requestID, nil)
		if response.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, response.Code, response.Body.String())
		}
		if i == 0 {
			firstID = requestID
			firstChannel = decodeChannelCreate(t, response).ID
		}
	}
	replay := channelCreateRequest(router, firstID, nil)
	if replay.Code != http.StatusCreated || decodeChannelCreate(t, replay).ID != firstChannel {
		t.Fatalf("idempotent replay after rate limit: %d %s", replay.Code, replay.Body.String())
	}
	limitedID, err := channels.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	limited := channelCreateRequest(router, limitedID, nil)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate limit response: %d %s retry=%s", limited.Code, limited.Body.String(), limited.Header().Get("Retry-After"))
	}
}

func TestChannelCloseClearsCookieAndFreesSlot(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, map[string]string{"X-Nclip-Visibility": "public"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]

	closeID, _ := channels.NewRequestID()
	closeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/channels/"+channel.ID, nil)
	closeRequest.Header.Set("Idempotency-Key", closeID)
	closeRequest.Header.Set("Origin", "https://clip.test")
	closeRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	closeRequest.AddCookie(senderCookie)
	closed := httptest.NewRecorder()
	router.ServeHTTP(closed, closeRequest)
	if closed.Code != http.StatusNoContent {
		t.Fatalf("close: %d %s", closed.Code, closed.Body.String())
	}
	cleared := closed.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != senderCookie.Name || cleared[0].Path != senderCookie.Path || cleared[0].MaxAge != -1 || cleared[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("sender cookie was not actively cleared: %+v", cleared)
	}

	retryRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/channels/"+channel.ID, nil)
	retryRequest.Header.Set("Idempotency-Key", closeID)
	retryRequest.Header.Set("Origin", "https://clip.test")
	retryRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	retried := httptest.NewRecorder()
	router.ServeHTTP(retried, retryRequest)
	if retried.Code != http.StatusNoContent {
		t.Fatalf("idempotent close retry: %d %s", retried.Code, retried.Body.String())
	}
	closedReplay := channelCreateRequest(router, createID, map[string]string{"X-Nclip-Visibility": "public"})
	if closedReplay.Code != http.StatusConflict || !strings.Contains(closedReplay.Body.String(), "channel_create_closed") {
		t.Fatalf("close did not suppress same-key replay: %d %s", closedReplay.Code, closedReplay.Body.String())
	}
	newCreateID, _ := channels.NewRequestID()
	recreated := channelCreateRequest(router, newCreateID, map[string]string{"X-Nclip-Visibility": "public"})
	if recreated.Code != http.StatusCreated || decodeChannelCreate(t, recreated).ID == channel.ID {
		t.Fatalf("closed channel slot was not released: %d %s", recreated.Code, recreated.Body.String())
	}
}

func TestChannelCloseRejectsOtherChannelCapability(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	first, _ := channels.NewRequestID()
	second, _ := channels.NewRequestID()
	a := channelCreateRequest(router, first, nil)
	b := channelCreateRequest(router, second, nil)
	channelA := decodeChannelCreate(t, a)
	closeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/channels/"+channelA.ID, nil)
	closeID, _ := channels.NewRequestID()
	closeRequest.Header.Set("Idempotency-Key", closeID)
	closeRequest.Header.Set("Origin", "https://clip.test")
	closeRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	closeRequest.AddCookie(b.Result().Cookies()[0])
	response := httptest.NewRecorder()
	router.ServeHTTP(response, closeRequest)
	if response.Code != http.StatusForbidden {
		t.Fatalf("other channel sender capability close = %d %s", response.Code, response.Body.String())
	}
}

func TestChannelKickRejectsCrossOrigin(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]
	joinID, _ := channels.NewRequestID()
	joined := channelJoinRequest(router, channel.ID, joinID, "", nil)
	if joined.Code != http.StatusCreated {
		t.Fatalf("join channel: %d %s", joined.Code, joined.Body.String())
	}
	senderStatus := channelRequestWithCookie(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, senderCookie, nil)
	var senderState struct {
		Seat *channels.SeatInfo `json:"seat"`
	}
	if senderStatus.Code != http.StatusOK || json.Unmarshal(senderStatus.Body.Bytes(), &senderState) != nil || senderState.Seat == nil {
		t.Fatalf("sender status: %d %s", senderStatus.Code, senderStatus.Body.String())
	}
	seatID := senderState.Seat.SeatID
	kickID, _ := channels.NewRequestID()
	body, _ := json.Marshal(map[string]string{"seat_id": seatID})
	kick := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/kick", body, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": kickID, "Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site",
	})
	if kick.Code != http.StatusForbidden {
		t.Fatalf("cross-origin kick = %d %s, want 403", kick.Code, kick.Body.String())
	}
	stillPending := channelRequestWithCookie(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, senderCookie, nil)
	if stillPending.Code != http.StatusOK || json.Unmarshal(stillPending.Body.Bytes(), &senderState) != nil || senderState.Seat == nil || senderState.Seat.SeatID != seatID || senderState.Seat.State != "pending_admit" {
		t.Fatalf("cross-origin kick changed the pending seat: %d %s state=%+v", stillPending.Code, stillPending.Body.String(), senderState.Seat)
	}
}

func TestConcurrentCreateRetryConsumesOneRateSlot(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	for i := 0; i < 29; i++ {
		requestID, err := channels.NewRequestID()
		if err != nil {
			t.Fatal(err)
		}
		if response := channelCreateRequest(router, requestID, nil); response.Code != http.StatusCreated {
			t.Fatalf("prefill %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	requestID, err := channels.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	const retries = 16
	responses := make([]*httptest.ResponseRecorder, retries)
	var wait sync.WaitGroup
	for i := range retries {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			responses[i] = channelCreateRequest(router, requestID, nil)
		}(i)
	}
	wait.Wait()
	channelID := ""
	for i, response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("same-key request %d: %d %s", i, response.Code, response.Body.String())
		}
		body := decodeChannelCreate(t, response)
		if channelID == "" {
			channelID = body.ID
		} else if channelID != body.ID {
			t.Fatalf("same idempotency key created multiple channels: %s vs %s", channelID, body.ID)
		}
	}
	newID, err := channels.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if response := channelCreateRequest(router, newID, nil); response.Code != http.StatusTooManyRequests {
		t.Fatalf("new request after exactly 30 unique creates = %d, want 429", response.Code)
	}
}
func TestConcurrentCloseAndSameKeyCreateAtLimitNeverReopen(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	for i := 0; i < 29; i++ {
		requestID, err := channels.NewRequestID()
		if err != nil {
			t.Fatal(err)
		}
		if response := channelCreateRequest(router, requestID, nil); response.Code != http.StatusCreated {
			t.Fatalf("prefill %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	requestID, err := channels.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	created := channelCreateRequest(router, requestID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("target create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	closeID, err := channels.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	var retry, closeResponse *httptest.ResponseRecorder
	wait.Add(2)
	go func() {
		defer wait.Done()
		retry = channelCreateRequest(router, requestID, nil)
	}()
	go func() {
		defer wait.Done()
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/channels/"+channel.ID, nil)
		request.Header.Set("Idempotency-Key", closeID)
		request.Header.Set("Origin", "https://clip.test")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.AddCookie(created.Result().Cookies()[0])
		closeResponse = httptest.NewRecorder()
		router.ServeHTTP(closeResponse, request)
	}()
	wait.Wait()

	if closeResponse.Code != http.StatusNoContent {
		t.Fatalf("close response = %d %s", closeResponse.Code, closeResponse.Body.String())
	}
	if retry.Code != http.StatusCreated && retry.Code != http.StatusConflict {
		t.Fatalf("same-key concurrent retry = %d %s", retry.Code, retry.Body.String())
	}
	if retry.Code == http.StatusCreated && decodeChannelCreate(t, retry).ID != channel.ID {
		t.Fatal("same-key concurrent retry created a different channel")
	}
	status := shareRequest(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, nil)
	if status.Code != http.StatusNotFound {
		t.Fatalf("channel reopened after close: status=%d body=%s", status.Code, status.Body.String())
	}
	newID, err := channels.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	if response := channelCreateRequest(router, newID, nil); response.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent retry bypassed full IP limit: %d %s", response.Code, response.Body.String())
	}
}
func channelJoinRequest(router http.Handler, channelID, requestID string, body string, headers map[string]string) *httptest.ResponseRecorder {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Idempotency-Key"] = requestID
	if body != "" {
		headers["Content-Type"] = "application/json"
	}
	return shareRequest(router, http.MethodPost, "/api/v1/channels/"+channelID+"/join", strings.NewReader(body), headers)
}

func channelLeaveRequest(router http.Handler, channelID, requestID, seatToken string) *httptest.ResponseRecorder {
	return shareRequest(router, http.MethodPost, "/api/v1/channels/"+channelID+"/leave", nil, map[string]string{
		"Idempotency-Key":    requestID,
		"X-Nclip-Seat-Token": seatToken,
		"Origin":             "https://clip.test",
		"Sec-Fetch-Site":     "same-origin",
	})
}

func channelRequestWithCookie(router http.Handler, method, path string, body []byte, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	return channelRequestWithCookies(router, method, path, body, []*http.Cookie{cookie}, headers)
}

func channelRequestWithCookies(router http.Handler, method, path string, body []byte, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	for _, cookie := range cookies {
		if cookie != nil {
			request.AddCookie(cookie)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

type kickInterruptWriter struct {
	header     http.Header
	started    chan struct{}
	unblocked  chan struct{}
	startedOne sync.Once
	stopOne    sync.Once
	status     int
}

func newKickInterruptWriter() *kickInterruptWriter {
	return &kickInterruptWriter{header: make(http.Header), started: make(chan struct{}), unblocked: make(chan struct{})}
}

func (w *kickInterruptWriter) Header() http.Header    { return w.header }
func (w *kickInterruptWriter) WriteHeader(status int) { w.status = status }
func (w *kickInterruptWriter) Flush()                 {}
func (w *kickInterruptWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		w.stopOne.Do(func() { close(w.unblocked) })
	}
	return nil
}
func (w *kickInterruptWriter) Write([]byte) (int, error) {
	w.startedOne.Do(func() { close(w.started) })
	<-w.unblocked
	return 0, errors.New("response interrupted by sender")
}

func (w *kickInterruptWriter) interrupted() bool {
	select {
	case <-w.unblocked:
		return true
	default:
		return false
	}
}

func TestAnonymousJoinAdmitStatusAndLeave(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	created := accountChannelCreate(router, admin, "Dg4ODg4ODg4ODg4ODg4ODg", "public")
	if created.Code != http.StatusCreated {
		t.Fatalf("channel create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)

	joinID, _ := channels.NewRequestID()
	joined := channelJoinRequest(router, channel.ID, joinID, "", nil)
	if joined.Code != http.StatusCreated {
		t.Fatalf("anonymous join: %d %s", joined.Code, joined.Body.String())
	}
	var seat map[string]any
	if err := json.Unmarshal(joined.Body.Bytes(), &seat); err != nil {
		t.Fatal(err)
	}
	seatToken, _ := seat["seat_token"].(string)
	claimToken, _ := seat["claim_token"].(string)
	pendingUntil, _ := seat["pending_until"].(string)
	if seat["state"] != "pending_admit" || seat["identity"] != "anonymous" || seat["anonymous"] != true || seatToken == "" || claimToken == "" || pendingUntil == "" {
		t.Fatalf("unexpected anonymous seat: %v", seat)
	}

	senderStatusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	senderStatusRequest.AddCookie(created.Result().Cookies()[0])
	senderStatus := httptest.NewRecorder()
	router.ServeHTTP(senderStatus, senderStatusRequest)
	var senderState map[string]any
	if senderStatus.Code != http.StatusOK || json.Unmarshal(senderStatus.Body.Bytes(), &senderState) != nil {
		t.Fatalf("sender status: %d %s", senderStatus.Code, senderStatus.Body.String())
	}
	senderSeat, _ := senderState["seat"].(map[string]any)
	if senderSeat["identity"] != "anonymous" || senderSeat["state"] != "pending_admit" {
		t.Fatalf("sender identity snapshot: %v", senderState)
	}
	seatID, _ := senderSeat["seat_id"].(string)
	if seatID == "" {
		t.Fatalf("sender pending snapshot omitted seat generation: %v", senderSeat)
	}

	decisionID, _ := channels.NewRequestID()
	decisionBody, _ := json.Marshal(map[string]string{"seat_id": seatID, "decision": "admit"})
	admitBody := strings.NewReader(string(decisionBody))
	admit := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/admit", admitBody)
	admit.Header.Set("Content-Type", "application/json")
	admit.Header.Set("Idempotency-Key", decisionID)
	admit.Header.Set("Origin", "https://clip.test")
	admit.Header.Set("Sec-Fetch-Site", "same-origin")
	admit.AddCookie(created.Result().Cookies()[0])
	admitted := httptest.NewRecorder()
	router.ServeHTTP(admitted, admit)
	if admitted.Code != http.StatusOK || !strings.Contains(admitted.Body.String(), "\"state\":\"paired\"") {
		t.Fatalf("admit: %d %s", admitted.Code, admitted.Body.String())
	}
	replayAdmit := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/admit", strings.NewReader(string(decisionBody)))
	replayAdmit.Header = admit.Header.Clone()
	replayAdmit.AddCookie(created.Result().Cookies()[0])
	replayed := httptest.NewRecorder()
	router.ServeHTTP(replayed, replayAdmit)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), "\"replayed\":true") {
		t.Fatalf("admit replay: %d %s", replayed.Code, replayed.Body.String())
	}

	receiverStatus := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	receiverStatus.Header.Set("X-Nclip-Seat-Token", seatToken)
	receiverStatusResponse := httptest.NewRecorder()
	router.ServeHTTP(receiverStatusResponse, receiverStatus)
	if receiverStatusResponse.Code != http.StatusOK || !strings.Contains(receiverStatusResponse.Body.String(), "\"seat_state\":\"paired\"") {
		t.Fatalf("receiver status: %d %s", receiverStatusResponse.Code, receiverStatusResponse.Body.String())
	}

	leaveID, _ := channels.NewRequestID()
	leave := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/leave", nil)
	leave.Header.Set("Idempotency-Key", leaveID)
	leave.Header.Set("X-Nclip-Seat-Token", seatToken)
	leave.Header.Set("Origin", "https://clip.test")
	leave.Header.Set("Sec-Fetch-Site", "same-origin")
	left := httptest.NewRecorder()
	router.ServeHTTP(left, leave)
	if left.Code != http.StatusNoContent {
		t.Fatalf("leave: %d %s", left.Code, left.Body.String())
	}
	replayedLeave := channelLeaveRequest(router, channel.ID, leaveID, seatToken)
	if replayedLeave.Code != http.StatusNoContent {
		t.Fatalf("leave replay: %d %s", replayedLeave.Code, replayedLeave.Body.String())
	}

	nextJoinID, _ := channels.NewRequestID()
	nextJoin := channelJoinRequest(router, channel.ID, nextJoinID, "", nil)
	if nextJoin.Code != http.StatusCreated {
		t.Fatalf("next join: %d %s", nextJoin.Code, nextJoin.Body.String())
	}
	var nextJoinBody map[string]any
	if err := json.Unmarshal(nextJoin.Body.Bytes(), &nextJoinBody); err != nil {
		t.Fatal(err)
	}
	nextSeatToken, _ := nextJoinBody["seat_token"].(string)
	if nextSeatToken == "" {
		t.Fatal("next join response omitted seat token")
	}
	oldLeaveRetry := channelLeaveRequest(router, channel.ID, leaveID, seatToken)
	if oldLeaveRetry.Code != http.StatusNoContent {
		t.Fatalf("old leave retry after next join: %d %s", oldLeaveRetry.Code, oldLeaveRetry.Body.String())
	}
	nextSeatStatus := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	nextSeatStatus.Header.Set("X-Nclip-Seat-Token", nextSeatToken)
	nextSeatResponse := httptest.NewRecorder()
	router.ServeHTTP(nextSeatResponse, nextSeatStatus)
	if nextSeatResponse.Code != http.StatusOK {
		t.Fatalf("old leave retry released next seat: %d %s", nextSeatResponse.Code, nextSeatResponse.Body.String())
	}
	closedStatus := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	closedStatus.Header.Set("X-Nclip-Seat-Token", seatToken)
	closedStatusResponse := httptest.NewRecorder()
	router.ServeHTTP(closedStatusResponse, closedStatus)
	if closedStatusResponse.Code != http.StatusForbidden {
		t.Fatalf("old seat token after next join = %d %s", closedStatusResponse.Code, closedStatusResponse.Body.String())
	}
}

func TestPublicLoggedJoinIsPairedAndNoAccountJoinNeedsAdmission(t *testing.T) {
	accountRouter := accountRouter(t)
	admin := signIn(t, accountRouter, "admin", "test-admin-password")
	publicCreated := accountChannelCreate(accountRouter, admin, "Dw8PDw8PDw8PDw8PDw8PDw", "public")
	if publicCreated.Code != http.StatusCreated {
		t.Fatalf("public create: %d %s", publicCreated.Code, publicCreated.Body.String())
	}
	publicChannel := decodeChannelCreate(t, publicCreated)
	joinID, _ := channels.NewRequestID()
	join := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+publicChannel.ID+"/join", nil)
	join.Header.Set("Idempotency-Key", joinID)
	join.Header.Set("Origin", "https://clip.test")
	join.Header.Set("Sec-Fetch-Site", "same-origin")
	join.Header.Set("X-CSRF-Token", admin.csrf)
	join.AddCookie(admin.cookie)
	joined := httptest.NewRecorder()
	accountRouter.ServeHTTP(joined, join)
	if joined.Code != http.StatusCreated || !strings.Contains(joined.Body.String(), "\"state\":\"paired\"") || !strings.Contains(joined.Body.String(), "\"identity\":\"admin\"") {
		t.Fatalf("logged public join: %d %s", joined.Code, joined.Body.String())
	}

	noAccountRouter := shareTestRouter(t, 4<<20, false)
	anonChannel := channelCreateRequest(noAccountRouter, "EBAQEBAQEBAQEBAQEBAQEA", map[string]string{"X-Nclip-Visibility": "public"})
	if anonChannel.Code != http.StatusCreated {
		t.Fatalf("no-account create: %d %s", anonChannel.Code, anonChannel.Body.String())
	}
	anon := decodeChannelCreate(t, anonChannel)
	anonJoinID, _ := channels.NewRequestID()
	anonJoined := channelJoinRequest(noAccountRouter, anon.ID, anonJoinID, "", nil)
	if anonJoined.Code != http.StatusCreated || !strings.Contains(anonJoined.Body.String(), "\"state\":\"pending_admit\"") {
		t.Fatalf("no-account join: %d %s", anonJoined.Code, anonJoined.Body.String())
	}
}

func TestJoinSeatFullAttemptsAreRateLimitedOnce(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	firstID, _ := channels.NewRequestID()
	first := channelJoinRequest(router, channel.ID, firstID, "", nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first join: %d %s", first.Code, first.Body.String())
	}
	for i := 0; i < 4; i++ {
		requestID, _ := channels.NewRequestID()
		response := channelJoinRequest(router, channel.ID, requestID, "", nil)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "seats_full") {
			t.Fatalf("seats_full attempt %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	retryID, _ := channels.NewRequestID()
	limited := channelJoinRequest(router, channel.ID, retryID, "", nil)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth unique join attempt = %d %s", limited.Code, limited.Body.String())
	}
}

func TestJoinRateLimitUsesGinClientIPInsteadOfForwardedHeader(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	const channelCount = 7
	channelIDs := make([]string, channelCount)
	for i := range channelIDs {
		createID, _ := channels.NewRequestID()
		created := channelCreateRequest(router, createID, nil)
		if created.Code != http.StatusCreated {
			t.Fatalf("create channel %d: %d %s", i, created.Code, created.Body.String())
		}
		channelIDs[i] = decodeChannelCreate(t, created).ID
	}

	attempt := 0
	join := func(channelIndex int) *httptest.ResponseRecorder {
		requestID, _ := channels.NewRequestID()
		spoofedIP := "198.51.100." + strconv.Itoa(attempt%200+1)
		attempt++
		return channelJoinRequest(router, channelIDs[channelIndex], requestID, "", map[string]string{
			"X-Forwarded-For": spoofedIP,
		})
	}
	for i := range channelIDs {
		if response := join(i); response.Code != http.StatusCreated {
			t.Fatalf("initial join %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	for i := 0; i < 30-channelCount; i++ {
		response := join(i % channelCount)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "seats_full") {
			t.Fatalf("seat-full attempt %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	if response := join(0); response.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed forwarded IP bypassed join limit: %d %s", response.Code, response.Body.String())
	}
}

func TestPrivateJoinRequiresDownloadAndSnapshotsUsername(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	created := accountChannelCreate(router, admin, "FhYWFhYWFhYWFhYWFhYWFh", "")
	if created.Code != http.StatusCreated {
		t.Fatalf("private channel create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)

	anonymousID, _ := channels.NewRequestID()
	anonymous := channelJoinRequest(router, channel.ID, anonymousID, "", nil)
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous private join = %d %s", anonymous.Code, anonymous.Body.String())
	}

	_ = createAccount(t, admin, "uploadonly", gin.H{"upload": true, "download": false, "manage": false})
	uploadOnly := signIn(t, router, "uploadonly", "test-user-password")
	uploadID, _ := channels.NewRequestID()
	uploadJoin := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/join", nil)
	uploadJoin.Header.Set("Idempotency-Key", uploadID)
	uploadJoin.Header.Set("Origin", "https://clip.test")
	uploadJoin.Header.Set("Sec-Fetch-Site", "same-origin")
	uploadJoin.Header.Set("X-CSRF-Token", uploadOnly.csrf)
	uploadJoin.AddCookie(uploadOnly.cookie)
	uploadResponse := httptest.NewRecorder()
	router.ServeHTTP(uploadResponse, uploadJoin)
	if uploadResponse.Code != http.StatusForbidden {
		t.Fatalf("upload-only private join = %d %s", uploadResponse.Code, uploadResponse.Body.String())
	}

	_ = createAccount(t, admin, "downloadonly", gin.H{"upload": false, "download": true, "manage": false})
	downloadOnly := signIn(t, router, "downloadonly", "test-user-password")
	downloadID, _ := channels.NewRequestID()
	downloadJoin := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/join", nil)
	downloadJoin.Header.Set("Idempotency-Key", downloadID)
	downloadJoin.Header.Set("Origin", "https://clip.test")
	downloadJoin.Header.Set("Sec-Fetch-Site", "same-origin")
	downloadJoin.Header.Set("X-CSRF-Token", downloadOnly.csrf)
	downloadJoin.AddCookie(downloadOnly.cookie)
	downloadResponse := httptest.NewRecorder()
	router.ServeHTTP(downloadResponse, downloadJoin)
	var privateJoinBody map[string]any
	if err := json.Unmarshal(downloadResponse.Body.Bytes(), &privateJoinBody); err != nil {
		t.Fatal(err)
	}
	if downloadResponse.Code != http.StatusCreated || privateJoinBody["identity"] != "downloadonly" || privateJoinBody["state"] != "paired" {
		t.Fatalf("download-only private join = %d %s", downloadResponse.Code, downloadResponse.Body.String())
	}
}

func TestConcurrentSameKeyJoinCreatesOneSeatAndReplaysOneResult(t *testing.T) {
	router := shareTestRouter(t, 4<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	joinID, _ := channels.NewRequestID()
	const concurrentRequests = 24
	responses := make([]*httptest.ResponseRecorder, concurrentRequests)
	var wait sync.WaitGroup
	wait.Add(concurrentRequests)
	for i := range responses {
		go func(index int) {
			defer wait.Done()
			responses[index] = channelJoinRequest(router, channel.ID, joinID, "", nil)
		}(i)
	}
	wait.Wait()

	var seatToken string
	for i, response := range responses {
		if response.Code != http.StatusCreated && response.Code != http.StatusOK {
			t.Fatalf("join %d: %d %s", i, response.Code, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode join %d: %v", i, err)
		}
		got, _ := body["seat_token"].(string)
		if got == "" || seatToken != "" && got != seatToken {
			t.Fatalf("join %d returned a different or empty seat token", i)
		}
		seatToken = got
	}
}

func TestSenderAndReceiverCredentialsSelectTheirOwnStatusRole(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	createID, _ := channels.NewRequestID()
	created := accountChannelCreate(router, admin, createID, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	joinID, _ := channels.NewRequestID()
	join := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/join", nil)
	join.Header.Set("Idempotency-Key", joinID)
	join.Header.Set("Origin", "https://clip.test")
	join.Header.Set("Sec-Fetch-Site", "same-origin")
	join.Header.Set("X-CSRF-Token", admin.csrf)
	join.AddCookie(admin.cookie)
	joined := httptest.NewRecorder()
	router.ServeHTTP(joined, join)
	if joined.Code != http.StatusCreated {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	var joinBody map[string]any
	if err := json.Unmarshal(joined.Body.Bytes(), &joinBody); err != nil {
		t.Fatal(err)
	}
	seatToken, _ := joinBody["seat_token"].(string)
	if seatToken == "" {
		t.Fatal("join response omitted seat token")
	}

	for _, test := range []struct {
		name         string
		seatToken    string
		wantReceiver bool
		wantStatus   int
	}{
		{name: "sender cookie only", wantStatus: http.StatusOK},
		{name: "receiver token takes precedence", seatToken: seatToken, wantReceiver: true, wantStatus: http.StatusOK},
		{name: "invalid receiver token does not fall back to sender", seatToken: "invalid", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
			request.AddCookie(created.Result().Cookies()[0])
			if test.seatToken != "" {
				request.Header.Set("X-Nclip-Seat-Token", test.seatToken)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			if test.wantStatus != http.StatusOK {
				return
			}
			var status map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			_, hasReceiverState := status["seat_state"]
			if hasReceiverState != test.wantReceiver {
				t.Fatalf("receiver state presence = %t, body=%v", hasReceiverState, status)
			}
			_, hasSenderState := status["sender_online"]
			if hasSenderState == test.wantReceiver {
				t.Fatalf("sender state presence = %t, body=%v", hasSenderState, status)
			}
		})
	}
}

func TestChannelContentChunkFinalizeIsIdempotentAndHiddenFromOrdinaryRoutes(t *testing.T) {
	router, store, dataDir := shareTestRouterWithStore(t, 128<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]
	otherCreateID, _ := channels.NewRequestID()
	otherCreated := channelCreateRequest(router, otherCreateID, nil)
	if otherCreated.Code != http.StatusCreated {
		t.Fatalf("create second channel: %d %s", otherCreated.Code, otherCreated.Body.String())
	}
	otherChannel := decodeChannelCreate(t, otherCreated)
	otherSenderCookie := otherCreated.Result().Cookies()[0]
	joinID, _ := channels.NewRequestID()
	joined := channelJoinRequest(router, channel.ID, joinID, "", nil)
	if joined.Code != http.StatusCreated {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	senderStatus := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	senderStatus.AddCookie(senderCookie)
	senderStatusResponse := httptest.NewRecorder()
	router.ServeHTTP(senderStatusResponse, senderStatus)
	var statusBody map[string]any
	if senderStatusResponse.Code != http.StatusOK || json.Unmarshal(senderStatusResponse.Body.Bytes(), &statusBody) != nil {
		t.Fatalf("sender status: %d %s", senderStatusResponse.Code, senderStatusResponse.Body.String())
	}
	seat, _ := statusBody["seat"].(map[string]any)
	seatID, _ := seat["seat_id"].(string)
	decisionID, _ := channels.NewRequestID()
	decisionBody, _ := json.Marshal(map[string]string{"seat_id": seatID, "decision": "admit"})
	admit := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/admit", decisionBody, senderCookie, map[string]string{
		"Content-Type":    "application/json",
		"Idempotency-Key": decisionID,
		"Origin":          "https://clip.test",
		"Sec-Fetch-Site":  "same-origin",
	})
	if admit.Code != http.StatusOK {
		t.Fatalf("admit receiver: %d %s", admit.Code, admit.Body.String())
	}

	content := make([]byte, 100<<20)
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind:  "file",
		Name:  "notes.txt",
		Parts: []models.SharePart{{Key: "part1", MIME: "text/plain", Size: int64(len(content))}},
	}}}
	initID, _ := channels.NewRequestID()
	initBody, _ := json.Marshal(gin.H{"manifest": manifest})
	contentPath := "/api/v1/channels/" + channel.ID + "/content"
	unauthorizedInitID, _ := channels.NewRequestID()
	unauthorized := channelRequestWithCookie(router, http.MethodPost, contentPath, initBody, otherSenderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": unauthorizedInitID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
		"X-Channel": otherChannel.ID,
	})
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("different sender capability attached content: %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	initRequest := func() *httptest.ResponseRecorder {
		return channelRequestWithCookie(router, http.MethodPost, contentPath, initBody, senderCookie, map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": initID,
			"Origin":          "https://clip.test",
			"Sec-Fetch-Site":  "same-origin",
			"X-Channel":       otherChannel.ID,
		})
	}
	init := initRequest()
	if init.Code != http.StatusCreated {
		t.Fatalf("content init: %d %s", init.Code, init.Body.String())
	}
	var initBodyJSON map[string]any
	if err := json.Unmarshal(init.Body.Bytes(), &initBodyJSON); err != nil {
		t.Fatal(err)
	}
	uploadID, _ := initBodyJSON["upload_id"].(string)
	if uploadID == "" || initBodyJSON["state"] != channels.ContentUploading {
		t.Fatalf("invalid upload session: %v", initBodyJSON)
	}
	replayedInit := initRequest()
	var replayedInitJSON map[string]any
	if replayedInit.Code != http.StatusOK || json.Unmarshal(replayedInit.Body.Bytes(), &replayedInitJSON) != nil || replayedInitJSON["upload_id"] != uploadID {
		t.Fatalf("init replay: %d %s", replayedInit.Code, replayedInit.Body.String())
	}

	chunkPath := contentPath + "/" + uploadID + "/parts/part1"
	sendChunk := func(offset int, part []byte) *httptest.ResponseRecorder {
		hash := sha256.Sum256(part)
		return channelRequestWithCookie(router, http.MethodPut, chunkPath, part, senderCookie, map[string]string{
			"Upload-Offset":  strconv.Itoa(offset),
			"X-Chunk-SHA256": hex.EncodeToString(hash[:]),
			"Origin":         "https://clip.test",
			"Sec-Fetch-Site": "same-origin",
			"X-Channel":      otherChannel.ID,
		})
	}
	firstEnd := min(channels.MaxContentChunkBytes, len(content))
	firstChunk := content[:firstEnd]
	for offset := 0; offset < len(content); {
		end := min(offset+channels.MaxContentChunkBytes, len(content))
		response := sendChunk(offset, content[offset:end])
		if response.Code != http.StatusNoContent {
			t.Fatalf("chunk at offset %d: %d %s", offset, response.Code, response.Body.String())
		}
		if offset == 0 {
			replay := sendChunk(0, firstChunk)
			if replay.Code != http.StatusNoContent || replay.Header().Get("X-Chunk-Replayed") != "true" {
				t.Fatalf("chunk replay: %d %s headers=%v", replay.Code, replay.Body.String(), replay.Header())
			}
		}
		offset = end
	}

	finalizeID, _ := channels.NewRequestID()
	finalizePath := contentPath + "/" + uploadID + "/finalize"
	finalizeHeaders := map[string]string{
		"Idempotency-Key": finalizeID,
		"Origin":          "https://clip.test",
		"Sec-Fetch-Site":  "same-origin",
		"X-Channel":       otherChannel.ID,
	}
	finalized := channelRequestWithCookie(router, http.MethodPost, finalizePath, nil, senderCookie, finalizeHeaders)
	if finalized.Code != http.StatusCreated || !strings.Contains(finalized.Body.String(), `"state":"held"`) {
		t.Fatalf("finalize: %d %s", finalized.Code, finalized.Body.String())
	}
	replayedFinalize := channelRequestWithCookie(router, http.MethodPost, finalizePath, nil, senderCookie, finalizeHeaders)
	if replayedFinalize.Code != http.StatusOK || !strings.Contains(replayedFinalize.Body.String(), `"replayed":true`) {
		t.Fatalf("finalize replay: %d %s", replayedFinalize.Code, replayedFinalize.Body.String())
	}

	ephemeral, err := store.ListEphemeralPastes()
	if err != nil || len(ephemeral) != 1 {
		t.Fatalf("ephemeral listing: len=%d err=%v", len(ephemeral), err)
	}
	paste := ephemeral[0]
	if paste.EphemeralChannelID != channel.ID || paste.ExpiresAt == nil || paste.ExpiresAt.Before(time.Now().Add(167*time.Hour)) {
		t.Fatalf("ephemeral metadata/fallback TTL incorrect: %+v", paste)
	}
	expiredFallback := paste
	expiredAt := time.Now().Add(-time.Second)
	expiredFallback.ExpiresAt = &expiredAt
	if err := store.Store(&expiredFallback); err != nil {
		t.Fatal(err)
	}
	if stillHeld, err := store.Get(paste.ID); err != nil || stillHeld == nil || !stillHeld.IsEphemeral() {
		t.Fatalf("fallback TTL removed channel content while active: %v", err)
	}
	reader, storedSize, err := store.OpenContent(paste.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedHash := sha256.New()
	if _, err := io.Copy(storedHash, reader); err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)
	if storedSize != int64(len(content)) || !bytes.Equal(storedHash.Sum(nil), wantHash[:]) {
		t.Fatalf("stored content checksum/size mismatch: got %d bytes, want %d", storedSize, len(content))
	}
	if visible, err := store.ListPastes(); err != nil || len(visible) != 0 {
		t.Fatalf("ordinary management listing exposed ephemeral content: %v %v", visible, err)
	}
	ordinaryRoutes := []struct {
		method  string
		path    string
		headers map[string]string
	}{
		{method: http.MethodGet, path: "/api/v1/shares/" + paste.ID},
		{method: http.MethodGet, path: "/api/v1/shares/" + paste.ID + "/content"},
		{method: http.MethodPost, path: "/api/v1/shares/" + paste.ID + "/receive", headers: map[string]string{"X-Nclip-Confirm": "1"}},
		{method: http.MethodGet, path: "/" + paste.ID},
		{method: http.MethodGet, path: "/raw/" + paste.ID},
		{method: http.MethodGet, path: "/json/" + paste.ID},
		{method: http.MethodGet, path: "/api/v1/meta/" + paste.ID},
		{method: http.MethodDelete, path: "/" + paste.ID},
	}
	for _, route := range ordinaryRoutes {
		response := shareRequest(router, route.method, route.path, nil, route.headers)
		if response.Code != http.StatusNotFound {
			t.Errorf("ordinary route %s %s exposed ephemeral paste: %d %s", route.method, route.path, response.Code, response.Body.String())
		}
	}

	closeID, _ := channels.NewRequestID()
	closed := channelRequestWithCookie(router, http.MethodDelete, "/api/v1/channels/"+channel.ID, nil, senderCookie, map[string]string{
		"Idempotency-Key": closeID,
		"Origin":          "https://clip.test",
		"Sec-Fetch-Site":  "same-origin",
	})
	if closed.Code != http.StatusNoContent {
		t.Fatalf("close channel cleanup: %d %s", closed.Code, closed.Body.String())
	}
	if _, err := store.Get(paste.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("close left ephemeral content behind: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dataDir, ".nclip-channel-upload-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("close left upload staging directories behind: %v %v", leftovers, err)
	}
}

func TestChannelContentHonorsUploadAuthAndPairedSeatRequirement(t *testing.T) {
	router, store, dataDir := shareTestRouterWithStore(t, 4<<20, true)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, map[string]string{"X-Api-Key": "test-key"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create with upload key: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]
	joinID, _ := channels.NewRequestID()
	if joined := channelJoinRequest(router, channel.ID, joinID, "", nil); joined.Code != http.StatusCreated {
		t.Fatalf("anonymous join without UploadAuth key: %d %s", joined.Code, joined.Body.String())
	}
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "data.bin", Parts: []models.SharePart{{Key: "part1", MIME: "application/octet-stream", Size: 4}},
	}}}
	body, _ := json.Marshal(gin.H{"manifest": manifest})
	requestID, _ := channels.NewRequestID()
	path := "/api/v1/channels/" + channel.ID + "/content"
	baseHeaders := map[string]string{
		"Content-Type":    "application/json",
		"Idempotency-Key": requestID,
		"Origin":          "https://clip.test",
		"Sec-Fetch-Site":  "same-origin",
	}
	withoutKey := channelRequestWithCookie(router, http.MethodPost, path, body, senderCookie, baseHeaders)
	if withoutKey.Code != http.StatusUnauthorized {
		t.Fatalf("content init without UploadAuth key = %d %s", withoutKey.Code, withoutKey.Body.String())
	}
	withKeyHeaders := map[string]string{}
	for key, value := range baseHeaders {
		withKeyHeaders[key] = value
	}
	withKeyHeaders["X-Api-Key"] = "test-key"
	whileUnpaired := channelRequestWithCookie(router, http.MethodPost, path, body, senderCookie, withKeyHeaders)
	if whileUnpaired.Code != http.StatusConflict || !strings.Contains(whileUnpaired.Body.String(), "content_conflict") {
		t.Fatalf("content init before receiver is paired = %d %s", whileUnpaired.Code, whileUnpaired.Body.String())
	}
	if ephemerals, err := store.ListEphemeralPastes(); err != nil || len(ephemerals) != 0 {
		t.Fatalf("rejected content init left a paste: %v %v", ephemerals, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dataDir, ".nclip-channel-upload-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("rejected content init left staging data: %v %v", leftovers, err)
	}
}

func TestChannelContentAbortAndSenderCloseRemoveIncompleteStaging(t *testing.T) {
	router, store, dataDir := shareTestRouterWithStore(t, 4<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]
	joinID, _ := channels.NewRequestID()
	if response := channelJoinRequest(router, channel.ID, joinID, "", nil); response.Code != http.StatusCreated {
		t.Fatalf("join: %d %s", response.Code, response.Body.String())
	}
	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID, nil)
	statusRequest.AddCookie(senderCookie)
	statusResponse := httptest.NewRecorder()
	router.ServeHTTP(statusResponse, statusRequest)
	var senderStatus map[string]any
	if statusResponse.Code != http.StatusOK || json.Unmarshal(statusResponse.Body.Bytes(), &senderStatus) != nil {
		t.Fatalf("sender status: %d %s", statusResponse.Code, statusResponse.Body.String())
	}
	seatInfo, _ := senderStatus["seat"].(map[string]any)
	seatID, _ := seatInfo["seat_id"].(string)
	decisionID, _ := channels.NewRequestID()
	decision, _ := json.Marshal(map[string]string{"seat_id": seatID, "decision": "admit"})
	if response := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/admit", decision, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": decisionID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	}); response.Code != http.StatusOK {
		t.Fatalf("admit: %d %s", response.Code, response.Body.String())
	}
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "partial.bin", Parts: []models.SharePart{{Key: "part1", MIME: "application/octet-stream", Size: 8}},
	}}}
	initBody, _ := json.Marshal(gin.H{"manifest": manifest})
	contentPath := "/api/v1/channels/" + channel.ID + "/content"
	init := func(id string) *httptest.ResponseRecorder {
		return channelRequestWithCookie(router, http.MethodPost, contentPath, initBody, senderCookie, map[string]string{
			"Content-Type": "application/json", "Idempotency-Key": id, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
		})
	}
	firstID, _ := channels.NewRequestID()
	first := init(firstID)
	var firstSession map[string]any
	if first.Code != http.StatusCreated || json.Unmarshal(first.Body.Bytes(), &firstSession) != nil {
		t.Fatalf("first init: %d %s", first.Code, first.Body.String())
	}
	firstUploadID, _ := firstSession["upload_id"].(string)
	chunk := []byte("12345678")
	chunkHash := sha256.Sum256(chunk)
	chunkPath := contentPath + "/" + firstUploadID + "/parts/part1"
	chunkHeaders := map[string]string{
		"Upload-Offset": "0", "X-Chunk-SHA256": hex.EncodeToString(chunkHash[:]), "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	}
	if response := channelRequestWithCookie(router, http.MethodPut, chunkPath, chunk, senderCookie, chunkHeaders); response.Code != http.StatusNoContent {
		t.Fatalf("first chunk: %d %s", response.Code, response.Body.String())
	}
	if response := channelRequestWithCookie(router, http.MethodDelete, contentPath+"/"+firstUploadID, nil, senderCookie, map[string]string{
		"Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	}); response.Code != http.StatusNoContent {
		t.Fatalf("abort: %d %s", response.Code, response.Body.String())
	}
	if leftovers, err := filepath.Glob(filepath.Join(dataDir, ".nclip-channel-upload-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("abort left staging data: %v %v", leftovers, err)
	}
	if ephemeral, err := store.ListEphemeralPastes(); err != nil || len(ephemeral) != 0 {
		t.Fatalf("abort left an ephemeral paste: %v %v", ephemeral, err)
	}

	secondID, _ := channels.NewRequestID()
	second := init(secondID)
	var secondSession map[string]any
	if second.Code != http.StatusCreated || json.Unmarshal(second.Body.Bytes(), &secondSession) != nil {
		t.Fatalf("slot not reusable after abort: %d %s", second.Code, second.Body.String())
	}
	secondUploadID, _ := secondSession["upload_id"].(string)
	secondChunkPath := contentPath + "/" + secondUploadID + "/parts/part1"
	if response := channelRequestWithCookie(router, http.MethodPut, secondChunkPath, chunk, senderCookie, chunkHeaders); response.Code != http.StatusNoContent {
		t.Fatalf("second chunk: %d %s", response.Code, response.Body.String())
	}
	closeID, _ := channels.NewRequestID()
	closed := channelRequestWithCookie(router, http.MethodDelete, "/api/v1/channels/"+channel.ID, nil, senderCookie, map[string]string{
		"Idempotency-Key": closeID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if closed.Code != http.StatusNoContent {
		t.Fatalf("close during upload: %d %s", closed.Code, closed.Body.String())
	}
	if leftovers, err := filepath.Glob(filepath.Join(dataDir, ".nclip-channel-upload-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("sender close left staging data: %v %v", leftovers, err)
	}
}

func TestChannelDeliveryRequiresClaimAndResumesMultipartRange(t *testing.T) {
	router, store, _ := shareTestRouterWithStore(t, 8<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]
	joinID, _ := channels.NewRequestID()
	joined := channelJoinRequest(router, channel.ID, joinID, "", nil)
	var joinBody struct {
		SeatToken  string `json:"seat_token"`
		ClaimToken string `json:"claim_token"`
	}
	if joined.Code != http.StatusCreated || json.Unmarshal(joined.Body.Bytes(), &joinBody) != nil {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	senderStatus := channelRequestWithCookie(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, senderCookie, nil)
	var senderState struct {
		Seat *channels.SeatInfo `json:"seat"`
	}
	if senderStatus.Code != http.StatusOK || json.Unmarshal(senderStatus.Body.Bytes(), &senderState) != nil || senderState.Seat == nil {
		t.Fatalf("sender status: %d %s", senderStatus.Code, senderStatus.Body.String())
	}
	decisionID, _ := channels.NewRequestID()
	decisionBody, _ := json.Marshal(map[string]string{"seat_id": senderState.Seat.SeatID, "decision": "admit"})
	decision := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/admit", decisionBody, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": decisionID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if decision.Code != http.StatusOK {
		t.Fatalf("admit: %d %s", decision.Code, decision.Body.String())
	}

	payload := []byte("range-bound-payload")
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "payload.txt", Parts: []models.SharePart{{Key: "part1", MIME: "text/plain", Size: int64(len(payload))}},
	}}}
	manifestBody, _ := json.Marshal(gin.H{"manifest": manifest})
	initID, _ := channels.NewRequestID()
	contentPath := "/api/v1/channels/" + channel.ID + "/content"
	init := channelRequestWithCookie(router, http.MethodPost, contentPath, manifestBody, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": initID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	var initBody struct {
		UploadID string `json:"upload_id"`
	}
	if init.Code != http.StatusCreated || json.Unmarshal(init.Body.Bytes(), &initBody) != nil {
		t.Fatalf("content init: %d %s", init.Code, init.Body.String())
	}
	chunkHash := sha256.Sum256(payload)
	chunkPath := contentPath + "/" + initBody.UploadID + "/parts/part1"
	chunk := channelRequestWithCookie(router, http.MethodPut, chunkPath, payload, senderCookie, map[string]string{
		"Upload-Offset": "0", "X-Chunk-SHA256": hex.EncodeToString(chunkHash[:]), "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if chunk.Code != http.StatusNoContent {
		t.Fatalf("content chunk: %d %s", chunk.Code, chunk.Body.String())
	}
	finalizeID, _ := channels.NewRequestID()
	finalize := channelRequestWithCookie(router, http.MethodPost, contentPath+"/"+initBody.UploadID+"/finalize", nil, senderCookie, map[string]string{
		"Idempotency-Key": finalizeID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if finalize.Code != http.StatusCreated {
		t.Fatalf("finalize: %d %s", finalize.Code, finalize.Body.String())
	}

	download := func(rangeHeader, ifRange, claimToken string) *httptest.ResponseRecorder {
		headers := map[string]string{
			"X-Nclip-Seat-Token":  joinBody.SeatToken,
			"X-Nclip-Claim-Token": claimToken,
			"Origin":              "https://clip.test",
			"Sec-Fetch-Site":      "same-origin",
		}
		if rangeHeader != "" {
			headers["Range"] = rangeHeader
		}
		if ifRange != "" {
			headers["If-Range"] = ifRange
		}
		return channelRequestWithCookies(router, http.MethodGet, contentPath, nil, nil, headers)
	}
	if response := download("", "", "wrong-claim"); response.Code != http.StatusForbidden {
		t.Fatalf("wrong claim token read = %d %s", response.Code, response.Body.String())
	}
	if response := download("bytes=8-", "", joinBody.ClaimToken); response.Code != http.StatusConflict {
		t.Fatalf("range skipped bytes not yet delivered = %d %s", response.Code, response.Body.String())
	}
	first := download("bytes=0-63", "", joinBody.ClaimToken)
	if first.Code != http.StatusPartialContent {
		t.Fatalf("first range: %d %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	contentRange := first.Header().Get("Content-Range")
	var start, end, total int
	if _, err := fmt.Sscanf(contentRange, "bytes %d-%d/%d", &start, &end, &total); err != nil || start != 0 || end != 63 || total <= end || etag == "" || first.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("invalid first range response: range=%q etag=%q headers=%v err=%v", contentRange, etag, first.Header(), err)
	}
	second := download("bytes=64-", etag, joinBody.ClaimToken)
	if second.Code != http.StatusPartialContent || int64(len(first.Body.Bytes())+len(second.Body.Bytes())) != int64(total) {
		t.Fatalf("resumed range: status=%d bytes=%d+%d total=%d body=%s", second.Code, first.Body.Len(), second.Body.Len(), total, second.Body.String())
	}
	fullBody := append(append([]byte(nil), first.Body.Bytes()...), second.Body.Bytes()...)
	if !bytes.Contains(fullBody, payload) {
		t.Fatal("multipart range round trip lost the original payload")
	}
	if remaining, err := store.ListEphemeralPastes(); err != nil || len(remaining) != 0 {
		t.Fatalf("completed delivery retained ephemeral content: remaining=%d err=%v", len(remaining), err)
	}
	afterComplete := channelRequestWithCookies(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, nil, map[string]string{
		"X-Nclip-Seat-Token": joinBody.SeatToken, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	var terminalError struct {
		Error string `json:"error"`
	}
	if afterComplete.Code != http.StatusGone || json.Unmarshal(afterComplete.Body.Bytes(), &terminalError) != nil || terminalError.Error != "delivery_complete" {
		t.Fatalf("completed claim terminal code = %d %s", afterComplete.Code, afterComplete.Body.String())
	}
}

func TestChannelKickInterruptsActiveContentResponse(t *testing.T) {
	router, store, _ := shareTestRouterWithStore(t, 8<<20, false)
	createID, _ := channels.NewRequestID()
	created := channelCreateRequest(router, createID, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	senderCookie := created.Result().Cookies()[0]
	joinID, _ := channels.NewRequestID()
	joined := channelJoinRequest(router, channel.ID, joinID, "", nil)
	var joinBody struct {
		SeatToken  string `json:"seat_token"`
		ClaimToken string `json:"claim_token"`
	}
	if joined.Code != http.StatusCreated || json.Unmarshal(joined.Body.Bytes(), &joinBody) != nil {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	senderStatus := channelRequestWithCookie(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, senderCookie, nil)
	var senderState struct {
		Seat *channels.SeatInfo `json:"seat"`
	}
	if senderStatus.Code != http.StatusOK || json.Unmarshal(senderStatus.Body.Bytes(), &senderState) != nil || senderState.Seat == nil {
		t.Fatalf("sender status: %d %s", senderStatus.Code, senderStatus.Body.String())
	}
	decisionID, _ := channels.NewRequestID()
	decisionBody, _ := json.Marshal(map[string]string{"seat_id": senderState.Seat.SeatID, "decision": "admit"})
	decision := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/admit", decisionBody, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": decisionID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if decision.Code != http.StatusOK {
		t.Fatalf("admit: %d %s", decision.Code, decision.Body.String())
	}

	payload := bytes.Repeat([]byte("kick-me"), 2048)
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "payload.txt", Parts: []models.SharePart{{Key: "part1", MIME: "text/plain", Size: int64(len(payload))}},
	}}}
	contentPath := "/api/v1/channels/" + channel.ID + "/content"
	manifestBody, _ := json.Marshal(gin.H{"manifest": manifest})
	initID, _ := channels.NewRequestID()
	init := channelRequestWithCookie(router, http.MethodPost, contentPath, manifestBody, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": initID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	var initBody struct {
		UploadID string `json:"upload_id"`
	}
	if init.Code != http.StatusCreated || json.Unmarshal(init.Body.Bytes(), &initBody) != nil {
		t.Fatalf("init: %d %s", init.Code, init.Body.String())
	}
	hash := sha256.Sum256(payload)
	chunk := channelRequestWithCookie(router, http.MethodPut, contentPath+"/"+initBody.UploadID+"/parts/part1", payload, senderCookie, map[string]string{
		"Upload-Offset": "0", "X-Chunk-SHA256": hex.EncodeToString(hash[:]), "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if chunk.Code != http.StatusNoContent {
		t.Fatalf("chunk: %d %s", chunk.Code, chunk.Body.String())
	}
	finalizeID, _ := channels.NewRequestID()
	finalize := channelRequestWithCookie(router, http.MethodPost, contentPath+"/"+initBody.UploadID+"/finalize", nil, senderCookie, map[string]string{
		"Idempotency-Key": finalizeID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if finalize.Code != http.StatusCreated {
		t.Fatalf("finalize: %d %s", finalize.Code, finalize.Body.String())
	}

	writer := newKickInterruptWriter()
	get := httptest.NewRequest(http.MethodGet, contentPath, nil)
	get.Header.Set("X-Nclip-Seat-Token", joinBody.SeatToken)
	get.Header.Set("X-Nclip-Claim-Token", joinBody.ClaimToken)
	get.Header.Set("Origin", "https://clip.test")
	get.Header.Set("Sec-Fetch-Site", "same-origin")
	getDone := make(chan struct{})
	go func() {
		router.ServeHTTP(writer, get)
		close(getDone)
	}()
	select {
	case <-writer.started:
	case <-time.After(3 * time.Second):
		t.Fatal("content response did not begin writing")
	}
	select {
	case <-getDone:
		t.Fatal("content response finished before the sender action")
	default:
	}
	if get.Context().Err() != nil || writer.interrupted() {
		t.Fatalf("response stopped before sender kick: request context=%v writer interrupted=%t", get.Context().Err(), writer.interrupted())
	}
	kickID, _ := channels.NewRequestID()
	kickBody, _ := json.Marshal(map[string]string{"seat_id": senderState.Seat.SeatID})
	kick := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/kick", kickBody, senderCookie, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": kickID, "Origin": "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	if kick.Code != http.StatusNoContent {
		t.Fatalf("kick: %d %s", kick.Code, kick.Body.String())
	}
	select {
	case <-getDone:
	case <-time.After(3 * time.Second):
		t.Fatal("sender kick did not interrupt the active response")
	}
	afterKick := channelRequestWithCookies(router, http.MethodGet, "/api/v1/channels/"+channel.ID, nil, nil, map[string]string{
		"X-Nclip-Seat-Token": joinBody.SeatToken,
		"Origin":             "https://clip.test", "Sec-Fetch-Site": "same-origin",
	})
	var terminalError struct {
		Error string `json:"error"`
	}
	if afterKick.Code != http.StatusGone || json.Unmarshal(afterKick.Body.Bytes(), &terminalError) != nil || terminalError.Error != "receiver_kicked" {
		t.Fatalf("kicked claim terminal code = %d %s", afterKick.Code, afterKick.Body.String())
	}
	if remaining, err := store.ListEphemeralPastes(); err != nil || len(remaining) != 0 {
		t.Fatalf("kick left temporary content: remaining=%d err=%v", len(remaining), err)
	}
}

func TestChannelContentUsesPerUserEffectiveUploadLimitAtInit(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	uploadID := createAccount(t, admin, "small-sender", gin.H{"upload": true, "download": true, "manage": false})
	quota := int64(1 << 20)
	updated := admin.call(http.MethodPut, "/api/v1/users/"+uploadID, gin.H{
		"username":           "small-sender",
		"permissions":        gin.H{"upload": true, "download": true, "manage": false},
		"upload_limit_bytes": quota,
		"version":            1,
	}, true)
	if updated.Code != http.StatusOK {
		t.Fatalf("set per-user cap: %d %s", updated.Code, updated.Body.String())
	}
	sender := signIn(t, router, "small-sender", "test-user-password")
	createID, _ := channels.NewRequestID()
	created := accountChannelCreate(router, sender, createID, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", created.Code, created.Body.String())
	}
	channel := decodeChannelCreate(t, created)
	joinID, _ := channels.NewRequestID()
	join := channelRequestWithCookie(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/join", nil, sender.cookie, map[string]string{
		"Idempotency-Key": joinID,
		"Origin":          "https://clip.test",
		"Sec-Fetch-Site":  "same-origin",
		"X-CSRF-Token":    sender.csrf,
	})
	if join.Code != http.StatusCreated {
		t.Fatalf("self join: %d %s", join.Code, join.Body.String())
	}
	manifest := models.ShareManifest{Version: 1, Items: []models.ShareItem{{
		Kind: "file", Name: "over-limit.bin", Parts: []models.SharePart{{Key: "part1", MIME: "application/octet-stream", Size: quota + 1}},
	}}}
	body, _ := json.Marshal(gin.H{"manifest": manifest})
	initID, _ := channels.NewRequestID()
	response := channelRequestWithCookies(router, http.MethodPost, "/api/v1/channels/"+channel.ID+"/content", body, []*http.Cookie{sender.cookie, created.Result().Cookies()[0]}, map[string]string{
		"Content-Type":    "application/json",
		"Idempotency-Key": initID,
		"Origin":          "https://clip.test",
		"Sec-Fetch-Site":  "same-origin",
		"X-CSRF-Token":    sender.csrf,
	})
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "content_too_large") {
		t.Fatalf("content init ignored effective user limit: %d %s", response.Code, response.Body.String())
	}
}

func TestStartupRemovesOrphanChannelContentAndChunkDirectories(t *testing.T) {
	dataDir := t.TempDir()
	store, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pasteID := "RPHN5"
	expires := time.Now().Add(24 * time.Hour)
	paste := &models.Paste{ID: pasteID, CreatedAt: time.Now(), ExpiresAt: &expires, Size: 4, ContentType: "text/plain", EphemeralChannelID: "old-channel"}
	if err := store.StoreContent(pasteID, []byte("lost")); err != nil {
		t.Fatal(err)
	}
	if err := store.Store(paste); err != nil {
		t.Fatal(err)
	}
	beforeSweep, err := store.ListEphemeralPastes()
	if err != nil || len(beforeSweep) != 1 {
		t.Fatalf("test setup did not store an ephemeral paste: %v %v", beforeSweep, err)
	}
	staging := filepath.Join(dataDir, ".nclip-channel-upload-stale")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "part1.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalTemp := filepath.Join(dataDir, storage.ChannelContentTempPrefix+"stale")
	if err := os.WriteFile(finalTemp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = setupRouter(store, &config.Config{DataDir: dataDir, URL: "https://clip.test", BufferSize: 4 << 20, DefaultTTL: 24 * time.Hour})
	if _, err := store.Get(pasteID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("startup kept orphan channel paste: %v", err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup kept orphan chunk directory: %v", err)
	}
	if _, err := os.Stat(finalTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup kept orphan finalization temp: %v", err)
	}
}

func TestSecondInstanceSharingFilesystemFailsClosedBeforeChannelSweep(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &config.Config{DataDir: dataDir, URL: "https://clip.test", BufferSize: 4 << 20, DefaultTTL: 24 * time.Hour, MaxRenderSize: 1024}

	firstStore, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	firstRouter, _ := setupRouterWithChannels(firstStore, cfg)
	t.Cleanup(func() { _ = firstStore.Close() })
	created := channelCreateRequest(firstRouter, "AQEBAQEBAQEBAQEBAQEBAQ", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("first instance channel create: %d %s", created.Code, created.Body.String())
	}
	channelID := decodeChannelCreate(t, created).ID
	activePaste := &models.Paste{ID: "ACTV5", CreatedAt: time.Now(), Size: 6, ContentType: "text/plain", EphemeralChannelID: channelID}
	if err := firstStore.StoreContent(activePaste.ID, []byte("active")); err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Store(activePaste); err != nil {
		t.Fatal(err)
	}

	secondStore, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	secondRouter, _ := setupRouterWithChannels(secondStore, cfg)
	t.Cleanup(func() { _ = secondStore.Close() })
	if _, err := secondStore.Get(activePaste.ID); err != nil {
		t.Fatalf("second startup deleted active ephemeral content: %v", err)
	}
	requestID, _ := channels.NewRequestID()
	response := channelCreateRequest(secondRouter, requestID, nil)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "channel_unavailable") {
		t.Fatalf("second instance channel route = %d %s", response.Code, response.Body.String())
	}
}

func TestStartupRemovesFinalizationContentWrittenBeforeMetadata(t *testing.T) {
	dataDir := t.TempDir()
	store, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "ARPH5"
	if err := store.StoreChannelContentStream(id, strings.NewReader("orphan"), int64(len("orphan"))); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(dataDir, storage.ChannelContentTempPrefix+id+".pending")
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("failed to create pre-metadata finalization object: %v", err)
	}
	finalPath := filepath.Join(dataDir, id)
	if err := os.WriteFile(finalPath, []byte("unowned-final"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{DataDir: dataDir, URL: "https://clip.test", BufferSize: 4 << 20, DefaultTTL: 24 * time.Hour, MaxRenderSize: 1024}
	_ = setupRouter(store, cfg)
	if exists, _, err := store.StatContent(id); err != nil || !exists {
		t.Fatalf("startup deleted final content without matching metadata: exists=%t err=%v", exists, err)
	}
	content, err := store.GetContent(id)
	if err != nil || string(content) != "unowned-final" {
		t.Fatalf("startup changed unowned final content: %q err=%v", content, err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup kept pending finalization object: %v", err)
	}
}

func TestPendingChannelSlugCannotBeReusedAndSweepPreservesOrdinaryShare(t *testing.T) {
	dataDir := t.TempDir()
	store, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "CLD52"
	if err := store.StoreChannelContentStream(id, strings.NewReader("channel-staging"), int64(len("channel-staging"))); err != nil {
		t.Fatal(err)
	}
	otherStore, err := storage.NewFilesystemStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStore.Close() })
	if exists, err := store.Exists(id); err != nil || !exists {
		t.Fatalf("pending channel slug not considered occupied: exists=%t err=%v", exists, err)
	}
	if exists, err := otherStore.Exists(id); err != nil || !exists {
		t.Fatalf("pending channel slug not visible to another store instance: exists=%t err=%v", exists, err)
	}
	if err := otherStore.StoreContentStream(id, strings.NewReader("other-instance"), int64(len("other-instance"))); !errors.Is(err, storage.ErrSlugReserved) {
		t.Fatalf("ordinary upload from another store instance reused pending slug: %v", err)
	}
	if err := store.StoreContentStream(id, strings.NewReader("ordinary-share"), int64(len("ordinary-share"))); !errors.Is(err, storage.ErrSlugReserved) {
		t.Fatalf("ordinary upload reused a reserved pending slug: %v", err)
	}
	// Simulate an older/independent writer that had passed the pre-reservation
	// existence check before the channel pending object appeared.
	if err := os.WriteFile(filepath.Join(dataDir, id), []byte("ordinary-share"), 0o600); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	ordinary := &models.Paste{ID: id, CreatedAt: time.Now(), ExpiresAt: &expires, Size: int64(len("ordinary-share")), ContentType: "text/plain"}
	if err := store.Store(ordinary); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dataDir, URL: "https://clip.test", BufferSize: 4 << 20, DefaultTTL: 24 * time.Hour, MaxRenderSize: 1024}
	_ = setupRouter(store, cfg)
	paste, err := store.Get(id)
	if err != nil || paste.IsEphemeral() {
		t.Fatalf("startup sweep deleted or changed ordinary share metadata: paste=%+v err=%v", paste, err)
	}
	content, err := store.GetContent(id)
	if err != nil || string(content) != "ordinary-share" {
		t.Fatalf("startup sweep deleted ordinary share bytes: %q err=%v", content, err)
	}
	if exists, err := store.Exists(id); err != nil || !exists {
		t.Fatalf("ordinary share disappeared after pending cleanup: exists=%t err=%v", exists, err)
	}
}
