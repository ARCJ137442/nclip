package handlers

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/accounts"
	"github.com/johnwmail/nclip/storage"
)

const sessionCookie = "nclip_session"
const accountContext = "account.user"

type loginFailures struct {
	count int
	until time.Time
}

// AccountHandler is the HTTP/session boundary, separate from the account store.
type AccountHandler struct {
	store         *accounts.Store
	pastes        storage.PasteStore
	config        *config.Config
	mu            sync.Mutex
	failures      map[string]loginFailures
	passwordSlots chan struct{}
}

// NewAccountHandler configures a bounded password-verification budget.
func NewAccountHandler(store *accounts.Store, pastes storage.PasteStore, cfg *config.Config) *AccountHandler {
	return &AccountHandler{store: store, pastes: pastes, config: cfg, failures: map[string]loginFailures{}, passwordSlots: make(chan struct{}, 2)}
}

// CurrentAccount returns the request's credential-free user snapshot.
func CurrentAccount(c *gin.Context) (accounts.User, bool) {
	value, ok := c.Get(accountContext)
	if !ok {
		return accounts.User{}, false
	}
	user, ok := value.(accounts.User)
	return user, ok
}

func (h *AccountHandler) cookie(c *gin.Context) (accounts.User, accounts.Session, bool) {
	raw, err := c.Cookie(h.cookieName(c))
	if err != nil {
		return accounts.User{}, accounts.Session{}, false
	}
	return h.store.Current(raw)
}

func (h *AccountHandler) blocked(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for key, item := range h.failures {
		if !item.until.After(now) {
			delete(h.failures, key)
		}
	}
	item := h.failures[ip]
	return item.count >= 10 && item.until.After(now) || len(h.failures) >= 2048 && item.count == 0
}

func (h *AccountHandler) recordFailure(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	item := h.failures[ip]
	if !item.until.After(time.Now()) {
		item = loginFailures{until: time.Now().Add(15 * time.Minute)}
	}
	item.count++
	h.failures[ip] = item
}

func (h *AccountHandler) authenticate(c *gin.Context, username, password string) (accounts.Authentication, bool) {
	if h.blocked(c.ClientIP()) {
		c.Header("Retry-After", "900")
		c.AbortWithStatusJSON(429, gin.H{"error": "login_throttled"})
		return accounts.Authentication{}, false
	}
	select {
	case h.passwordSlots <- struct{}{}:
		defer func() { <-h.passwordSlots }()
	default:
		c.AbortWithStatusJSON(429, gin.H{"error": "login_throttled"})
		return accounts.Authentication{}, false
	}
	auth, err := h.store.Authenticate(username, password)
	if err != nil {
		h.recordFailure(c.ClientIP())
		return accounts.Authentication{}, false
	}
	return auth, true
}

func (h *AccountHandler) requestUser(c *gin.Context) (accounts.User, accounts.Session, bool) {
	if user, session, ok := h.cookie(c); ok {
		return user, session, true
	}
	// Browsers may retain the old proxy Basic header. Ignore it for browser
	// requests so logout and the new HTML login page remain authoritative.
	if c.GetHeader("Sec-Fetch-Site") != "" {
		return accounts.User{}, accounts.Session{}, false
	}
	username, password, ok := c.Request.BasicAuth()
	if !ok {
		return accounts.User{}, accounts.Session{}, false
	}
	auth, ok := h.authenticate(c, username, password)
	return auth.User, accounts.Session{}, ok
}

func publicPath(path string) bool {
	return strings.HasPrefix(path, "/static/") || path == "/favicon.ico" || path == "/health" || path == "/login" || path == "/api/v1/login" || path == "/api/v1/session"
}

func shareRead(c *gin.Context) bool {
	path := c.FullPath()
	if c.Request.Method == http.MethodPost {
		return path == "/api/v1/shares/:slug/receive"
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		return false
	}
	switch path {
	case "/:slug", "/raw/:slug", "/json/:slug", "/api/v1/meta/:slug", "/api/v1/shares/:slug", "/api/v1/shares/:slug/content":
		return true
	}
	return false
}

func (h *AccountHandler) publicOrMissingShare(c *gin.Context) bool {
	if !shareRead(c) {
		return false
	}
	paste, err := h.pastes.Get(c.Param("slug"))
	if errors.Is(err, storage.ErrNotFound) {
		return true
	}
	return err == nil && paste != nil && (paste.Public || paste.IsExpired())
}

func requiredPermission(c *gin.Context) string {
	path := c.Request.URL.Path
	if path == "/admin" || strings.HasPrefix(path, "/api/v1/users") || strings.HasPrefix(path, "/api/v1/admin/") {
		return "manage"
	}
	if c.Request.Method == http.MethodDelete {
		return "manage"
	}
	if shareRead(c) || path == "/receive" {
		return "download"
	}
	if c.Request.Method == http.MethodPost && (path == "/" || path == "/burn/" || path == "/base64" || path == "/api/v1/shares") {
		return "upload"
	}
	return ""
}

func allowed(user accounts.User, permission string) bool {
	switch permission {
	case "upload":
		return user.Permissions.Upload
	case "download":
		return user.Permissions.Download
	case "manage":
		return user.Permissions.Manage
	}
	return true
}

func browserNavigation(c *gin.Context) bool {
	return c.Request.Method == http.MethodGet && strings.Contains(c.GetHeader("Accept"), "text/html")
}

// SafeNext accepts only local application paths, never protocol-relative URLs.
func SafeNext(value string) string {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(value)
	if err != nil || u.Host != "" || u.Scheme != "" || u.Path == "/login" {
		return "/"
	}
	return u.RequestURI()
}

func (h *AccountHandler) csrf(c *gin.Context, session accounts.Session) bool {
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions || session.UserID == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(c.GetHeader("X-CSRF-Token")), []byte(session.CSRF)) == 1 && h.sameOrigin(c)
}

func (h *AccountHandler) sameOrigin(c *gin.Context) bool {
	origin := c.GetHeader("Origin")
	if origin == "" {
		return c.GetHeader("Sec-Fetch-Site") != "cross-site"
	}
	expected := strings.TrimRight(h.config.URL, "/")
	if expected == "" {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		expected = scheme + "://" + c.Request.Host
	}
	return origin == expected
}

// Gate protects every legacy and new content route, not just the visible UI.
func (h *AccountHandler) Gate() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if strings.HasPrefix(c.Request.URL.Path, "/static/") {
			c.Header("Cache-Control", "no-cache")
		}
		if publicPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		user, session, ok := h.requestUser(c)
		if c.IsAborted() {
			return
		}
		if ok {
			c.Set(accountContext, user)
		}
		// Missing/expired links have no protected content; let the normal 404
		// page explain them, including a public one-time link after consumption.
		if h.publicOrMissingShare(c) {
			c.Next()
			return
		}
		if !ok {
			h.loginRequired(c)
			return
		}
		if !allowed(user, requiredPermission(c)) {
			h.forbidden(c)
			return
		}
		if !h.csrf(c, session) {
			c.AbortWithStatusJSON(403, gin.H{"error": "csrf_failed"})
			return
		}
		c.Next()
	}
}

func (h *AccountHandler) loginRequired(c *gin.Context) {
	if browserNavigation(c) {
		c.Redirect(303, "/login?next="+url.QueryEscape(SafeNext(c.Request.URL.RequestURI())))
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(401, gin.H{"error": "login_required"})
}

func (h *AccountHandler) forbidden(c *gin.Context) {
	if browserNavigation(c) {
		c.HTML(403, "account-error.html", gin.H{"Version": h.config.Version})
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(403, gin.H{"error": "permission_denied"})
}
