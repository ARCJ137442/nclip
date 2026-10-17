package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return setupRouter(store, &config.Config{DataDir: dir, AccountsEnabled: true, AdminUsername: "admin", AdminPasswordFile: secret, URL: "https://clip.test", BufferSize: 1 << 20, DefaultTTL: 72 * time.Hour, MaxRenderSize: 1024, Version: "accounts-test"})
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
	createAccount(t, admin, "uploadonly", gin.H{"upload": true})
	createAccount(t, admin, "downloadonly", gin.H{"download": true})
	createAccount(t, admin, "manageonly", gin.H{"manage": true})
	upload := signIn(t, router, "uploadonly", "test-user-password")
	download := signIn(t, router, "downloadonly", "test-user-password")
	manager := signIn(t, router, "manageonly", "test-user-password")
	for _, client := range []accountClient{upload, download} {
		if w := client.call("GET", "/api/v1/users", nil, false); w.Code != 403 {
			t.Fatalf("user management leaked %d", w.Code)
		}
	}
	if w := download.call("POST", "/", nil, true); w.Code != 403 {
		t.Fatalf("reader uploaded %d", w.Code)
	}
	if w := manager.call("POST", "/", nil, true); w.Code != 403 {
		t.Fatalf("manage granted upload %d", w.Code)
	}
	if w := manager.call("GET", "/api/v1/users", nil, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	private := accountLegacyPaste(t, upload, false)
	for _, client := range []accountClient{upload, manager, {router: router}} {
		if w := client.call("GET", "/raw/"+private, nil, false); w.Code == 200 {
			t.Fatal("private content leaked")
		}
	}
	if w := download.call("GET", "/raw/"+private, nil, false); w.Code != 200 {
		t.Fatalf("reader failed %d", w.Code)
	}
	if w := download.call("DELETE", "/"+private, nil, true); w.Code != 403 {
		t.Fatal("reader deleted content")
	}
	public := accountLegacyPaste(t, upload, true)
	if w := (accountClient{router: router}).call("GET", "/raw/"+public, nil, false); w.Code != 200 {
		t.Fatalf("public share blocked %d", w.Code)
	}
	if w := manager.call("GET", "/api/v1/admin/shares", nil, false); w.Code != 200 || strings.Contains(w.Body.String(), "password_hash") {
		t.Fatal("bad administration listing")
	}
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
