package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
)

const senderCookieName = "nclip_sender_key"

var errChannelCreateThrottled = errors.New("channel_create_throttled")

const (
	createLimitPerMinute = 30
	createWindow         = time.Minute
	maxTrackedCreateIPs  = 2048
)

type createWindowState struct {
	attempts []time.Time
}

type ChannelHandler struct {
	manager       *channels.Manager
	config        *config.Config
	accounts      *AccountHandler
	createMu      sync.Mutex
	createWindows map[string]createWindowState
}

func NewChannelHandler(manager *channels.Manager, cfg *config.Config, accounts *AccountHandler) *ChannelHandler {
	return &ChannelHandler{
		manager:       manager,
		config:        cfg,
		accounts:      accounts,
		createWindows: make(map[string]createWindowState),
	}
}

// Gate owns channel access decisions so the account Gate cannot reject an
// explicitly public channel before the handler has resolved its visibility.
func (h *ChannelHandler) Gate() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path != "/api/v1/channels" && path != "/api/v1/channels/:id" && path != "/c/:id" {
			c.Next()
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Content-Security-Policy", "frame-ancestors 'none'")
		c.Header("Referrer-Policy", "same-origin")
		c.Header("X-Content-Type-Options", "nosniff")

		if path == "/api/v1/channels" {
			if c.Request.Method != http.MethodPost {
				c.AbortWithStatus(http.StatusMethodNotAllowed)
				return
			}
			if !h.authorizeCreate(c) {
				return
			}
			c.Next()
			return
		}

		if path == "/api/v1/channels/:id" && c.Request.Method == http.MethodDelete {
			if !h.sameOrigin(c) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf_failed"})
				return
			}
			if !channels.ValidRequestID(c.GetHeader("Idempotency-Key")) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": channels.ErrInvalidRequestID.Error()})
				return
			}
			if _, exists := h.manager.Get(c.Param("id")); exists && !h.senderAuthorized(c, c.Param("id")) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": channels.ErrUnauthorized.Error()})
				return
			}
			c.Next()
			return
		}

		if c.Request.Method != http.MethodGet {
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		record, ok := h.manager.Get(c.Param("id"))
		if !ok {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "channel_not_found"})
			return
		}
		if path == "/c/:id" || path == "/api/v1/channels/:id" {
			if !h.authorizeRead(c, record) {
				return
			}
		}
		c.Next()
	}
}

func (h *ChannelHandler) authorizeCreate(c *gin.Context) bool {
	if h.accounts == nil {
		return true
	}
	user, session, ok := h.accounts.requestUser(c)
	if c.IsAborted() {
		return false
	}
	if !ok {
		h.accounts.loginRequired(c)
		return false
	}
	c.Set(accountContext, user)
	if !user.Permissions.Upload {
		h.accounts.forbidden(c)
		return false
	}
	if !h.accounts.csrf(c, session) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf_failed"})
		return false
	}
	return true
}

func (h *ChannelHandler) authorizeRead(c *gin.Context, record channels.Record) bool {
	if h.accounts == nil {
		return true
	}
	user, _, ok := h.accounts.requestUser(c)
	if c.IsAborted() {
		return false
	}
	if ok {
		c.Set(accountContext, user)
	}
	if record.Visibility == channels.VisibilityPublic || c.FullPath() == "/api/v1/channels/:id" && h.senderAuthorized(c, record.ID) {
		return true
	}
	if !ok {
		h.accounts.loginRequired(c)
		return false
	}
	if !user.Permissions.Download {
		h.accounts.forbidden(c)
		return false
	}
	return true
}

func (h *ChannelHandler) Create(c *gin.Context) {
	requestID := c.GetHeader("Idempotency-Key")
	if !channels.ValidRequestID(requestID) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": channels.ErrInvalidRequestID.Error()})
		return
	}

	visibility := channels.VisibilityAuthenticated
	value := c.GetHeader("X-Nclip-Visibility")
	if value == string(channels.VisibilityPublic) {
		visibility = channels.VisibilityPublic
	} else if value != "" && value != string(channels.VisibilityAuthenticated) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_visibility"})
		return
	}
	if h.accounts == nil {
		visibility = channels.VisibilityPublic
	}

	record, senderKey, _, err := h.createChannel(channelCreateScope(c, h.config), requestID, c.ClientIP(), visibility)
	if errors.Is(err, errChannelCreateThrottled) {
		c.Header("Retry-After", "60")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, channels.ErrInvalidVisibility) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, channels.ErrCapacity) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, channels.ErrCreateClosed) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, channels.ErrIdempotencyConflict) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "channel_create_failed"})
		return
	}

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     senderCookieName,
		Value:    senderKey,
		Path:     "/api/v1/channels/" + record.ID,
		HttpOnly: true,
		Secure:   h.secureRequest(c),
		SameSite: http.SameSiteStrictMode,
	})
	baseURL := strings.TrimRight(h.config.URL, "/")
	if baseURL == "" {
		scheme := "http"
		if h.secureRequest(c) {
			scheme = "https"
		}
		baseURL = scheme + "://" + c.Request.Host
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":         record.ID,
		"url":        baseURL + "/c/" + record.ID,
		"visibility": record.Visibility,
	})
}

func (h *ChannelHandler) createChannel(scope, requestID, ip string, visibility channels.Visibility) (channels.Record, string, bool, error) {
	if !channels.ValidRequestID(requestID) {
		return channels.Record{}, "", false, channels.ErrInvalidRequestID
	}
	h.createMu.Lock()
	defer h.createMu.Unlock()
	if h.manager.IsCreateClosed(scope, requestID) {
		return channels.Record{}, "", false, channels.ErrCreateClosed
	}
	now := time.Now()
	firstRequest := !h.manager.HasCreate(scope, requestID)
	if firstRequest && !h.allowCreateLocked(ip, now) {
		return channels.Record{}, "", false, errChannelCreateThrottled
	}
	record, senderKey, replayed, err := h.manager.Create(scope, requestID, visibility)
	if err == nil && !replayed {
		h.recordCreateLocked(ip, now)
	}
	return record, senderKey, replayed, err
}

func (h *ChannelHandler) allowCreateLocked(ip string, now time.Time) bool {
	cutoff := now.Add(-createWindow)
	for trackedIP, window := range h.createWindows {
		live := window.attempts[:0]
		for _, attemptedAt := range window.attempts {
			if attemptedAt.After(cutoff) {
				live = append(live, attemptedAt)
			}
		}
		if len(live) == 0 {
			delete(h.createWindows, trackedIP)
		} else {
			window.attempts = live
			h.createWindows[trackedIP] = window
		}
	}
	window, ok := h.createWindows[ip]
	if !ok {
		return len(h.createWindows) < maxTrackedCreateIPs
	}
	return len(window.attempts) < createLimitPerMinute
}

func (h *ChannelHandler) recordCreateLocked(ip string, now time.Time) {
	window := h.createWindows[ip]
	window.attempts = append(window.attempts, now)
	h.createWindows[ip] = window
}

func (h *ChannelHandler) Close(c *gin.Context) {
	senderKey, _ := c.Cookie(senderCookieName)
	h.createMu.Lock()
	err := h.manager.Close(c.Param("id"), senderKey)
	h.createMu.Unlock()
	if err != nil {
		if errors.Is(err, channels.ErrUnauthorized) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "channel_close_failed"})
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     senderCookieName,
		Value:    "",
		Path:     "/api/v1/channels/" + c.Param("id"),
		HttpOnly: true,
		Secure:   h.secureRequest(c),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
	c.Status(http.StatusNoContent)
}

func (h *ChannelHandler) sameOrigin(c *gin.Context) bool {
	if h.accounts != nil {
		return h.accounts.sameOrigin(c)
	}
	origin := c.GetHeader("Origin")
	if origin == "" {
		return c.GetHeader("Sec-Fetch-Site") != "cross-site"
	}
	expected := strings.TrimRight(h.config.URL, "/")
	if expected == "" {
		scheme := "http"
		if h.secureRequest(c) {
			scheme = "https"
		}
		expected = scheme + "://" + c.Request.Host
	}
	return origin == expected
}

func channelCreateScope(c *gin.Context, cfg *config.Config) string {
	if user, ok := CurrentAccount(c); ok {
		return "account:" + user.ID
	}
	key := ""
	if authorization := c.GetHeader("Authorization"); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		key = strings.TrimSpace(authorization[7:])
	}
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("X-Api-Key"))
	}
	if cfg.UploadAuth && key != "" {
		digest := sha256.Sum256([]byte(key))
		return "api-key:" + hex.EncodeToString(digest[:])
	}
	return "ip:" + c.ClientIP()
}

func (h *ChannelHandler) senderAuthorized(c *gin.Context, channelID string) bool {
	senderKey, err := c.Cookie(senderCookieName)
	return err == nil && h.manager.SenderAuthorized(channelID, senderKey)
}

func (h *ChannelHandler) secureRequest(c *gin.Context) bool {
	if strings.HasPrefix(strings.ToLower(h.config.URL), "https://") || c.Request.TLS != nil {
		return true
	}
	if !strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		return false
	}
	remote := c.Request.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	// Gin's ClientIP honors only SetTrustedProxies. A difference from the
	// direct peer indicates a trusted proxy supplied the forwarded client IP.
	return c.ClientIP() != remote
}

func (h *ChannelHandler) Page(c *gin.Context) {
	record, _ := h.manager.Get(c.Param("id"))
	c.HTML(http.StatusOK, "channel.html", gin.H{
		"Title":      "NCLIP 信道",
		"Version":    h.config.Version,
		"ChannelID":  record.ID,
		"Visibility": record.Visibility,
	})
}

func (h *ChannelHandler) Status(c *gin.Context) {
	record, _ := h.manager.Get(c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"state": record.State})
}
