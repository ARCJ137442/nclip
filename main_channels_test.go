package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
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
