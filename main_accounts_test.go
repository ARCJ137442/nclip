package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/storage"
)

type accountClient struct {
	router http.Handler
	cookie *http.Cookie
	csrf   string
}

func accountRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	secret := filepath.Join(dir, "bootstrap")
	if err := os.WriteFile(secret, []byte("test-admin-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFilesystemStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return setupRouter(store, &config.Config{DataDir: dir, AccountsEnabled: true, AdminUsername: "admin", AdminPasswordFile: secret, URL: "https://clip.test", BufferSize: 4 * config.MiB, DefaultTTL: 72 * time.Hour, MaxRenderSize: 1024, Version: "accounts-test"})
}

func (c accountClient) call(method, path string, value any, csrf bool) *httptest.ResponseRecorder {
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://clip.test")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	if csrf {
		r.Header.Set("X-CSRF-Token", c.csrf)
	}
	w := httptest.NewRecorder()
	c.router.ServeHTTP(w, r)
	return w
}

func signIn(t *testing.T, router http.Handler, name, password string) accountClient {
	t.Helper()
	client := accountClient{router: router}
	w := client.call("POST", "/api/v1/login", gin.H{"username": name, "password": password}, false)
	if w.Code != 200 {
		t.Fatalf("login %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	client.cookie = cookies[0]
	var body struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	client.csrf = body.CSRF
	return client
}

func createAccount(t *testing.T, admin accountClient, name string, permissions gin.H) string {
	t.Helper()
	w := admin.call("POST", "/api/v1/users", gin.H{"username": name, "password": "test-user-password", "permissions": permissions}, true)
	if w.Code != 200 {
		t.Fatalf("create user %d %s", w.Code, w.Body.String())
	}
	var body struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.User.ID
}

func TestAccountsFormSessionAndCSRF(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	slug := accountLegacyPaste(t, admin, false)
	request := httptest.NewRequest("GET", "/"+slug, nil)
	request.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "next=%2F"+slug) {
		t.Fatalf("share login redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = shareRequest(router, "GET", "/login", nil, nil)
	for _, expected := range []string{`name="username"`, `autocomplete="username"`, `type="password"`, `autocomplete="current-password"`, `method="post"`} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("form missing %s", expected)
		}
	}
	if !admin.cookie.HttpOnly || !admin.cookie.Secure || admin.cookie.Name != "__Host-nclip_session" || admin.cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe session cookie")
	}
	if w := admin.call("POST", "/api/v1/users", gin.H{}, false); w.Code != 403 {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	if w := admin.call("POST", "/api/v1/logout", nil, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := admin.call("GET", "/api/v1/users", nil, false); w.Code != 401 {
		t.Fatalf("logged-out session accepted: %d", w.Code)
	}
}

func TestAccountsPermissionMatrixAndPublicShares(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	uploadID := createAccount(t, admin, "uploadonly", gin.H{"upload": true})
	downloadID := createAccount(t, admin, "downloadonly", gin.H{"download": true})
	upload := signIn(t, router, "uploadonly", "test-user-password")
	download := signIn(t, router, "downloadonly", "test-user-password")
	manager := admin
	assertUserManagementDenied(t, upload, download)
	assertReaderCannotUpload(t, download)
	assertAdministratorCanStoreAndList(t, manager)
	assertInvalidUserCapRejected(t, manager, uploadID, downloadID)
	private := accountLegacyPaste(t, upload, false)
	assertPrivateShareAccess(t, router, upload, download, manager, private)
	public := accountLegacyPaste(t, upload, true)
	assertPublicShareAccessible(t, router, public)
	if w := manager.call("GET", "/api/v1/admin/shares", nil, false); w.Code != 200 || strings.Contains(w.Body.String(), "password_hash") {
		t.Fatal("bad administration listing")
	}
}

func assertUserManagementDenied(t *testing.T, clients ...accountClient) {
	t.Helper()
	for _, client := range clients {
		if w := client.call("GET", "/api/v1/users", nil, false); w.Code != 403 {
			t.Fatalf("user management leaked %d", w.Code)
		}
	}
}

func assertReaderCannotUpload(t *testing.T, reader accountClient) {
	t.Helper()
	if w := reader.call("POST", "/", nil, true); w.Code != 403 {
		t.Fatalf("reader uploaded %d", w.Code)
	}
}

func assertAdministratorCanStoreAndList(t *testing.T, manager accountClient) {
	t.Helper()
	adminPaste := accountLegacyPaste(t, manager, false)
	if w := manager.call("GET", "/raw/"+adminPaste, nil, false); w.Code != http.StatusOK {
		t.Fatalf("administrator upload permission did not store content: %d", w.Code)
	}
	if w := manager.call("GET", "/api/v1/users", nil, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func assertInvalidUserCapRejected(t *testing.T, manager accountClient, uploadID, downloadID string) {
	t.Helper()
	for _, id := range []string{uploadID, downloadID} {
		w := manager.call("PUT", "/api/v1/users/"+id, gin.H{"username": map[string]string{uploadID: "uploadonly", downloadID: "downloadonly"}[id], "permissions": gin.H{"upload": id == uploadID, "download": id == downloadID}, "upload_limit_bytes": int64(1024), "version": 1}, true)
		if w.Code != 400 {
			t.Fatalf("invalid user cap returned %d: %s", w.Code, w.Body.String())
		}
	}
}

func assertPrivateShareAccess(t *testing.T, router http.Handler, owner, reader, manager accountClient, private string) {
	t.Helper()
	for _, client := range []accountClient{owner, {router: router}} {
		if w := client.call("GET", "/raw/"+private, nil, false); w.Code == 200 {
			t.Fatal("private content leaked")
		}
	}
	if w := reader.call("GET", "/raw/"+private, nil, false); w.Code != 200 {
		t.Fatalf("reader failed %d", w.Code)
	}
	if w := manager.call("GET", "/raw/"+private, nil, false); w.Code != 200 {
		t.Fatalf("administrator read failed %d", w.Code)
	}
	if w := reader.call("DELETE", "/"+private, nil, true); w.Code != 403 {
		t.Fatal("reader deleted content")
	}
}

func assertPublicShareAccessible(t *testing.T, router http.Handler, public string) {
	t.Helper()
	if w := (accountClient{router: router}).call("GET", "/raw/"+public, nil, false); w.Code != 200 {
		t.Fatalf("public share blocked %d", w.Code)
	}
}

func accountSignupClients(t *testing.T, router http.Handler) (accountClient, accountClient, accountClient, string, string) {
	t.Helper()
	admin := signIn(t, router, "admin", "test-admin-password")
	adminID := adminSessionUserID(t, admin)
	uploadID := createAccount(t, admin, "smallupload", gin.H{"upload": true})
	_ = createAccount(t, admin, "smallread", gin.H{"download": true})
	quota := int64(config.MiB)
	if w := admin.call("PUT", "/api/v1/users/"+uploadID, gin.H{"username": "smallupload", "permissions": gin.H{"upload": true}, "upload_limit_bytes": quota, "version": 1}, true); w.Code != http.StatusOK {
		t.Fatalf("set user quota %d: %s", w.Code, w.Body.String())
	}
	return admin, signIn(t, router, "smallupload", "test-user-password"), signIn(t, router, "smallread", "test-user-password"), adminID, uploadID
}

func accountLegacyPaste(t *testing.T, client accountClient, public bool) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader("account test content"))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Origin", "https://clip.test")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("X-CSRF-Token", client.csrf)
	r.AddCookie(client.cookie)
	if public {
		r.Header.Set("X-Nclip-Visibility", "public")
	}
	w := httptest.NewRecorder()
	client.router.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("paste %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Slug
}

func TestPerUserUploadLimitAllLegacyEndpoints(t *testing.T) {
	router := accountRouter(t)
	_, uploader, _, _, _ := accountSignupClients(t, router)
	atLimit := []struct {
		path   string
		body   []byte
		base64 bool
	}{
		{path: "/", body: bytes.Repeat([]byte("x"), int(config.MiB))},
		{path: "/burn/", body: bytes.Repeat([]byte("x"), int(config.MiB))},
		{path: "/base64", body: []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), int(config.MiB)))), base64: true},
	}
	for _, tc := range atLimit {
		request := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(tc.body))
		request.Header.Set("Content-Type", "text/plain")
		request.Header.Set("Origin", "https://clip.test")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("X-CSRF-Token", uploader.csrf)
		if tc.base64 {
			request.Header.Set("X-Base64", "true")
		}
		request.AddCookie(uploader.cookie)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("%s rejected exact user limit: %d: %s", tc.path, response.Code, response.Body.String())
		}
	}
	tests := []struct {
		path   string
		body   []byte
		base64 bool
	}{
		{path: "/", body: bytes.Repeat([]byte("x"), int(config.MiB)+1)},
		{path: "/burn/", body: bytes.Repeat([]byte("x"), int(config.MiB)+1)},
		{path: "/base64", body: []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), int(config.MiB)+1))), base64: true},
	}
	for _, tc := range tests {
		request := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(tc.body))
		request.Header.Set("Content-Type", "text/plain")
		request.Header.Set("Origin", "https://clip.test")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("X-CSRF-Token", uploader.csrf)
		if tc.base64 {
			request.Header.Set("X-Base64", "true")
		}
		request.AddCookie(uploader.cookie)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s returned %d, want 413: %s", tc.path, response.Code, response.Body.String())
		}
	}
}

func accountShareRequest(router http.Handler, client accountClient, size int) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("manifest", `{"version":1,"items":[{"kind":"file","name":"payload.bin","parts":[{"key":"file","mime":"application/octet-stream"}]}]}`)
	part, _ := writer.CreateFormFile("file", "payload.bin")
	_, _ = part.Write(bytes.Repeat([]byte("x"), size))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/shares", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "https://clip.test")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("X-CSRF-Token", client.csrf)
	request.AddCookie(client.cookie)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestPerUserUploadLimitBrowserShares(t *testing.T) {
	router := accountRouter(t)
	admin, _, _, _, uploadID := accountSignupClients(t, router)
	if response := admin.call("PUT", "/api/v1/users/"+uploadID, gin.H{"username": "smallupload", "permissions": gin.H{"upload": true}, "upload_limit_bytes": 2 * config.MiB, "version": 2}, true); response.Code != http.StatusOK {
		t.Fatalf("raise user quota above global setting %d: %s", response.Code, response.Body.String())
	}
	uploader := signIn(t, router, "smallupload", "test-user-password")
	if response := admin.call("PUT", "/api/v1/settings/upload", gin.H{"upload_limit_mib": 1, "revision": 0}, true); response.Code != http.StatusOK {
		t.Fatalf("set low global share limit %d: %s", response.Code, response.Body.String())
	}
	if response := accountShareRequest(router, uploader, int(config.MiB+1)); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit edge share returned %d: %s", response.Code, response.Body.String())
	}
	if response := accountShareRequest(router, uploader, int(config.MiB)); response.Code != http.StatusCreated {
		t.Fatalf("at-limit edge share returned %d: %s", response.Code, response.Body.String())
	}
	if response := accountShareRequest(router, admin, 2*config.MiB); response.Code != http.StatusCreated {
		t.Fatalf("administrator was constrained by global browser-share setting: %d %s", response.Code, response.Body.String())
	}
}

func TestUploadSettingsManagerPermissionCSRFAndPersistence(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	adminID := adminSessionUserID(t, admin)
	uploaderID := createAccount(t, admin, "uploadonly", gin.H{"upload": true})
	uploader := signIn(t, router, "uploadonly", "test-user-password")

	assertUploadSettingsAccess(t, router, admin, uploader)
	assertUploadSettingsSave(t, admin)
	assertUploadSettingsPersistence(t, admin, admin)
	if w := admin.call("PUT", "/api/v1/users/"+uploaderID, gin.H{"username": "uploadonly", "permissions": gin.H{"upload": true}, "upload_limit_bytes": nil, "version": 1}, true); w.Code != http.StatusOK {
		t.Fatalf("admin could not clear per-user limit: %d %s", w.Code, w.Body.String())
	}
	if w := admin.call("PUT", "/api/v1/users/"+adminID, gin.H{"username": "admin", "permissions": gin.H{"upload": true, "download": true, "manage": true}, "upload_limit_bytes": config.MiB, "version": 1}, true); w.Code != http.StatusConflict {
		t.Fatalf("admin per-user cap change returned %d: %s", w.Code, w.Body.String())
	}
}

func adminSessionUserID(t *testing.T, admin accountClient) string {
	t.Helper()
	w := admin.call("GET", "/api/v1/session", nil, false)
	var result struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.User.ID
}

func assertUploadSettingsAccess(t *testing.T, router http.Handler, manager, uploader accountClient) {
	t.Helper()
	if w := (accountClient{router: router}).call("GET", "/api/v1/settings/upload", nil, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous settings read returned %d", w.Code)
	}
	if w := uploader.call("GET", "/api/v1/settings/upload", nil, false); w.Code != http.StatusForbidden {
		t.Fatalf("upload-only settings read returned %d", w.Code)
	}
	w := manager.call("GET", "/api/v1/settings/upload", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("manager settings read returned %d: %s", w.Code, w.Body.String())
	}
	var initial struct {
		UploadLimitMiB int64  `json:"upload_limit_mib"`
		Revision       uint64 `json:"revision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial.UploadLimitMiB != 4 || initial.Revision != 0 {
		t.Fatalf("initial settings = (%d MiB, revision %d), want (4 MiB, revision 0)", initial.UploadLimitMiB, initial.Revision)
	}
}

func assertUploadSettingsSave(t *testing.T, manager accountClient) {
	t.Helper()
	if w := manager.call("PUT", "/api/v1/settings/upload", gin.H{"upload_limit_mib": 2, "revision": 0}, false); w.Code != http.StatusForbidden {
		t.Fatalf("settings update without CSRF returned %d", w.Code)
	}
	if w := manager.call("PUT", "/api/v1/settings/upload", gin.H{"upload_limit_mib": 2, "revision": 0}, true); w.Code != http.StatusOK {
		t.Fatalf("settings update returned %d: %s", w.Code, w.Body.String())
	}
}

func assertUploadSettingsPersistence(t *testing.T, admin, manager accountClient) {
	t.Helper()
	w := admin.call("GET", "/api/v1/settings/upload", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("updated settings read returned %d: %s", w.Code, w.Body.String())
	}
	var current struct {
		UploadLimitMiB int64  `json:"upload_limit_mib"`
		Revision       uint64 `json:"revision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.UploadLimitMiB != 2 || current.Revision != 1 {
		t.Fatalf("active setting = (%d MiB, revision %d), want (2 MiB, revision 1)", current.UploadLimitMiB, current.Revision)
	}
	w = manager.call("GET", "/api/v1/ui-config", nil, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"upload_max_bytes":4194304`) {
		t.Fatalf("public UI config did not reflect saved limit: %d %s", w.Code, w.Body.String())
	}
	if w := manager.call("PUT", "/api/v1/settings/upload", gin.H{"upload_limit_mib": 3, "revision": 0}, true); w.Code != http.StatusConflict {
		t.Fatalf("stale settings update returned %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiveEntryWorksBeforeJavaScript(t *testing.T) {
	router := accountRouter(t)
	admin := signIn(t, router, "admin", "test-admin-password")
	slug := accountLegacyPaste(t, admin, false)
	home := admin.call("GET", "/", nil, false)
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `id="composer"`) {
		t.Fatalf("administrator home should open the composer: %d %s", home.Code, home.Body.String())
	}
	for _, value := range []string{slug, strings.ToLower(slug), "https://clip.test/raw/" + slug} {
		w := admin.call("GET", "/receive?code="+url.QueryEscape(value), nil, false)
		if w.Code != 303 || w.Header().Get("Location") != "/"+slug {
			t.Fatalf("native receive failed: %d %s", w.Code, w.Header().Get("Location"))
		}
	}
	w := admin.call("GET", "/receive?code="+url.QueryEscape("https://untrusted.invalid/"+slug), nil, false)
	if w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatal("receive entry allowed an external redirect")
	}
}
