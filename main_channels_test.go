package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/channels"
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
