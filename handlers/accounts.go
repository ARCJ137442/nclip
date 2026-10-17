package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/accounts"
	"github.com/johnwmail/nclip/utils"
)

func (h *AccountHandler) secureCookie(c *gin.Context) bool {
	return strings.HasPrefix(h.config.URL, "https://") || c.Request.TLS != nil
}

func (h *AccountHandler) cookieName(c *gin.Context) string {
	if h.secureCookie(c) {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

// Session exposes only the current identity and its CSRF token to this browser.
func (h *AccountHandler) Session(c *gin.Context) {
	user, session, ok := h.cookie(c)
	if !ok {
		c.JSON(200, gin.H{"enabled": true, "user": nil})
		return
	}
	c.JSON(200, gin.H{"enabled": true, "user": user, "csrf_token": session.CSRF, "expires_at": session.ExpiresAt})
}

func (h *AccountHandler) loginPage(c *gin.Context, errorCode string, status int) {
	next := c.Query("next")
	if next == "" {
		next = c.PostForm("next")
	}
	c.HTML(status, "login.html", gin.H{"Version": h.config.Version, "Next": SafeNext(next), "LoginError": errorCode, "Username": c.PostForm("username")})
}

// LoginPage uses a real HTML form, stable field names, and standard autocomplete.
func (h *AccountHandler) LoginPage(c *gin.Context) {
	if _, _, ok := h.cookie(c); ok {
		c.Redirect(303, SafeNext(c.Query("next")))
		return
	}
	h.loginPage(c, "", 200)
}

// Login accepts both native form posts and same-origin JSON clients.
func (h *AccountHandler) Login(c *gin.Context) {
	if !h.sameOrigin(c) {
		c.JSON(403, gin.H{"error": "csrf_failed"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Next     string `json:"next"`
	}
	jsonClient := strings.Contains(c.GetHeader("Content-Type"), "application/json")
	if jsonClient {
		if c.ShouldBindJSON(&input) != nil {
			c.JSON(400, gin.H{"error": "invalid_request"})
			return
		}
	} else {
		if c.Request.ParseForm() != nil {
			h.loginPage(c, "invalid_request", 400)
			return
		}
		input.Username = c.PostForm("username")
		input.Password = c.PostForm("password")
		input.Next = c.PostForm("next")
	}
	auth, err := h.authenticate(c, input.Username, input.Password)
	if err != nil {
		status := 401
		if errors.Is(err, accounts.ErrInput("login_throttled")) {
			status = 429
		}
		h.loginFailure(c, err.Error(), status, jsonClient)
		return
	}
	raw, session, err := h.store.NewSession(auth)
	if err != nil {
		h.loginFailure(c, "session_failed", 500, jsonClient)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: h.cookieName(c), Value: raw, Path: "/", HttpOnly: true, Secure: h.secureCookie(c), SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt, MaxAge: int(time.Until(session.ExpiresAt).Seconds())})
	next := SafeNext(input.Next)
	if jsonClient {
		c.JSON(200, gin.H{"user": auth.User, "csrf_token": session.CSRF, "next": next})
		return
	}
	c.Redirect(303, next)
}

func (h *AccountHandler) loginFailure(c *gin.Context, code string, status int, jsonClient bool) {
	if status == 429 {
		c.Header("Retry-After", "900")
	}
	if jsonClient {
		c.JSON(status, gin.H{"error": code})
		return
	}
	h.loginPage(c, code, status)
}

// Logout invalidates the server session before clearing the browser cookie.
func (h *AccountHandler) Logout(c *gin.Context) {
	raw, _ := c.Cookie(h.cookieName(c))
	if err := h.store.Logout(raw); err != nil {
		c.JSON(500, gin.H{"error": "session_failed"})
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: h.cookieName(c), Value: "", Path: "/", HttpOnly: true, Secure: h.secureCookie(c), SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	c.JSON(200, gin.H{"ok": true})
}

// Home selects the appropriate task without exposing inaccessible actions.
func (h *AccountHandler) Home(c *gin.Context) {
	user, _ := CurrentAccount(c)
	web := NewWebUIHandler(h.config)
	if web.isCli(c) {
		web.Index(c)
		return
	}
	if user.Permissions.Upload {
		NewWebUIHandler(h.config).Index(c)
		return
	}
	if user.Permissions.Download {
		h.ReceivePage(c)
		return
	}
	if user.Permissions.Manage {
		c.Redirect(303, "/admin")
		return
	}
	c.HTML(200, "account-error.html", gin.H{"Version": h.config.Version})
}

// ReceivePage is the link/code entry point for users with download permission.
func (h *AccountHandler) ReceivePage(c *gin.Context) {
	value := c.Query("code")
	if value != "" {
		if code := h.receiveCode(c, value); code != "" {
			c.Redirect(303, "/"+code)
			return
		}
	}
	c.HTML(200, "download.html", gin.H{"Version": h.config.Version, "Code": value, "ReceiveError": value != ""})
}

func (h *AccountHandler) receiveCode(c *gin.Context, value string) string {
	value = strings.TrimSpace(value)
	if code := strings.ToUpper(value); utils.IsValidSlug(code) {
		return code
	}
	if !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
		return ""
	}
	base, _ := url.Parse(h.config.URL)
	if base == nil || base.Host == "" {
		base = &url.URL{Scheme: "http", Host: c.Request.Host}
	}
	reference, err := url.Parse(value)
	if err != nil {
		return ""
	}
	target := base.ResolveReference(reference)
	if target.Scheme != base.Scheme || !strings.EqualFold(target.Host, base.Host) || target.RawQuery != "" || target.Fragment != "" {
		return ""
	}
	path := strings.TrimPrefix(target.EscapedPath(), "/")
	path = strings.TrimPrefix(path, "raw/")
	path = strings.TrimSuffix(path, "/")
	code := strings.ToUpper(path)
	if !utils.IsValidSlug(code) {
		return ""
	}
	return code
}

// AdminPage provides separate user and share administration views.
func (h *AccountHandler) AdminPage(c *gin.Context) {
	c.HTML(200, "admin.html", gin.H{"Version": h.config.Version})
}

// Users lists public user records only.
func (h *AccountHandler) Users(c *gin.Context) { c.JSON(200, gin.H{"items": h.store.List()}) }

// SaveUser replaces the explicit account settings and revokes old sessions.
func (h *AccountHandler) SaveUser(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	var input struct {
		Username    string                `json:"username"`
		Password    string                `json:"password"`
		Permissions *accounts.Permissions `json:"permissions"`
		Disabled    bool                  `json:"disabled"`
		Version     uint64                `json:"version"`
	}
	if c.ShouldBindJSON(&input) != nil || input.Permissions == nil {
		c.JSON(400, gin.H{"error": "invalid_request"})
		return
	}
	actor, _ := CurrentAccount(c)
	user, err := h.store.SaveUser(actor.ID, accounts.UserChange{ID: c.Param("id"), Username: input.Username, Password: input.Password, Permissions: *input.Permissions, Disabled: input.Disabled, Version: input.Version})
	if err != nil {
		accountError(c, err)
		return
	}
	c.JSON(200, gin.H{"user": user})
}

// DeleteUser prevents removing the last enabled manager in the store transaction.
func (h *AccountHandler) DeleteUser(c *gin.Context) {
	actor, _ := CurrentAccount(c)
	if err := h.store.DeleteUser(actor.ID, c.Param("id")); err != nil {
		accountError(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func accountError(c *gin.Context, err error) {
	var input accounts.ErrInput
	if errors.As(err, &input) {
		status := 400
		if input == "permission_denied" {
			status = 403
		}
		if input == "username_exists" || input == "last_manager" || input == "user_changed" {
			status = 409
		}
		if input == "user_not_found" {
			status = 404
		}
		c.JSON(status, gin.H{"error": input.Error()})
		return
	}
	c.JSON(500, gin.H{"error": "account_write_failed"})
}
