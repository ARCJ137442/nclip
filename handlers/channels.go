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
	"github.com/johnwmail/nclip/storage"
)

const senderCookieName = "nclip_sender_key"
const seatTokenHeader = "X-Nclip-Seat-Token"
const claimTokenHeader = "X-Nclip-Claim-Token"

var errChannelCreateThrottled = errors.New("channel_create_throttled")

const (
	createLimitPerMinute         = 30
	createWindow                 = time.Minute
	maxTrackedCreateIPs          = 2048
	joinLimitPerIPPerMinute      = 30
	joinLimitPerChannelPerMinute = 5
)

type createWindowState struct {
	attempts []time.Time
}

type ChannelHandler struct {
	manager            *channels.Manager
	config             *config.Config
	accounts           *AccountHandler
	shares             *SharesHandler
	store              storage.PasteStore
	channelWriter      storage.ChannelWriterStore
	contentInitialized bool
	disabledReason     string
	cleanupMu          sync.Mutex
	nextCleanup        time.Time
	janitorInterval    time.Duration
	createMu           sync.Mutex
	createWindows      map[string]createWindowState
	joinMu             sync.Mutex
	joinIPWindows      map[string]createWindowState
	joinChannelWindows map[string]createWindowState
	contentChunkSlots  chan struct{}
}

func (h *ChannelHandler) SetSharesHandler(shares *SharesHandler) {
	h.shares = shares
}

func (h *ChannelHandler) Disable(reason string) {
	h.disabledReason = reason
}

func (h *ChannelHandler) ReleaseContentWriter() error {
	if h.channelWriter == nil {
		return nil
	}
	err := h.channelWriter.ReleaseChannelWriter()
	if err == nil {
		h.channelWriter = nil
		h.contentInitialized = false
	}
	return err
}

func (h *ChannelHandler) ContentJanitorReady() bool {
	return h.contentInitialized && h.channelWriter != nil && h.channelWriter.OwnsChannelWriter()
}

func NewChannelHandler(manager *channels.Manager, cfg *config.Config, accounts *AccountHandler) *ChannelHandler {
	return &ChannelHandler{
		manager:            manager,
		config:             cfg,
		accounts:           accounts,
		createWindows:      make(map[string]createWindowState),
		joinIPWindows:      make(map[string]createWindowState),
		joinChannelWindows: make(map[string]createWindowState),
		contentChunkSlots:  make(chan struct{}, maxConcurrentChannelChunks),
		janitorInterval:    channelLivenessSweepInterval,
	}
}

// Gate owns channel access decisions so the account Gate cannot reject an
// explicitly public channel before the handler has resolved its visibility.
func (h *ChannelHandler) Gate() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if h.disabledReason != "" && channelRoutePath(path) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "channel_unavailable"})
			return
		}
		if path != "/api/v1/channels" && path != "/api/v1/channels/:id" && path != "/c/:id" &&
			path != "/api/v1/channels/:id/join" && path != "/api/v1/channels/:id/admit" &&
			path != "/api/v1/channels/:id/heartbeat" && path != "/api/v1/channels/:id/seat/heartbeat" &&
			path != "/api/v1/channels/:id/leave" && path != "/api/v1/channels/:id/kick" && !channelContentPath(path) {
			c.Next()
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Content-Security-Policy", "frame-ancestors 'none'")
		c.Header("Referrer-Policy", "same-origin")
		c.Header("X-Content-Type-Options", "nosniff")
		if channelContentPath(path) {
			if !h.authorizeContent(c, path) {
				return
			}
			c.Next()
			return
		}

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
		if path == "/api/v1/channels/:id/join" {
			if c.Request.Method != http.MethodPost {
				c.AbortWithStatus(http.StatusMethodNotAllowed)
				return
			}
			record, ok := h.manager.Get(c.Param("id"))
			if !ok {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "channel_not_found"})
				return
			}
			if !h.authorizeJoin(c, record) {
				return
			}
			c.Next()
			return
		}
		if path == "/api/v1/channels/:id/admit" || path == "/api/v1/channels/:id/heartbeat" || path == "/api/v1/channels/:id/kick" {
			if c.Request.Method != http.MethodPost {
				c.AbortWithStatus(http.StatusMethodNotAllowed)
				return
			}
			if !h.sameOrigin(c) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf_failed"})
				return
			}
			if _, ok := h.manager.Get(c.Param("id")); !ok {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "channel_not_found"})
				return
			}
			if !h.senderAuthorized(c, c.Param("id")) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": channels.ErrUnauthorized.Error()})
				return
			}
			c.Next()
			return
		}
		if path == "/api/v1/channels/:id/leave" || path == "/api/v1/channels/:id/seat/heartbeat" {
			if c.Request.Method != http.MethodPost {
				c.AbortWithStatus(http.StatusMethodNotAllowed)
				return
			}
			if !h.sameOrigin(c) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf_failed"})
				return
			}
			if _, ok := h.manager.Get(c.Param("id")); !ok {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "channel_not_found"})
				return
			}
			if c.GetHeader(seatTokenHeader) == "" {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": channels.ErrUnauthorized.Error()})
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
		if !h.authorizeRead(c, record) {
			return
		}
		c.Next()
	}
}

func channelRoutePath(path string) bool {
	return path == "/api/v1/channels" || strings.HasPrefix(path, "/api/v1/channels/") || path == "/c/:id"
}

func channelContentPath(path string) bool {
	switch path {
	case "/api/v1/channels/:id/content",
		"/api/v1/channels/:id/content/:uploadID/parts/:partKey",
		"/api/v1/channels/:id/content/:uploadID/finalize",
		"/api/v1/channels/:id/content/:uploadID":
		return true
	default:
		return false
	}
}

func (h *ChannelHandler) authorizeContent(c *gin.Context, path string) bool {
	if path == "/api/v1/channels/:id/content" && c.Request.Method == http.MethodGet {
		return h.authorizeContentRead(c)
	}
	method := http.MethodPost
	if path == "/api/v1/channels/:id/content/:uploadID/parts/:partKey" {
		method = http.MethodPut
	} else if path == "/api/v1/channels/:id/content/:uploadID" {
		method = http.MethodDelete
	}
	if c.Request.Method != method {
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return false
	}
	if !h.sameOrigin(c) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf_failed"})
		return false
	}
	if _, ok := h.manager.Get(c.Param("id")); !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": channels.ErrChannelNotFound.Error()})
		return false
	}
	if !h.senderAuthorized(c, c.Param("id")) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": channels.ErrUnauthorized.Error()})
		return false
	}
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

func (h *ChannelHandler) authorizeContentRead(c *gin.Context) bool {
	if !h.sameOrigin(c) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cross_origin_denied"})
		return false
	}
	record, ok := h.manager.Get(c.Param("id"))
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": channels.ErrChannelNotFound.Error()})
		return false
	}
	if h.accounts != nil {
		user, _, authenticated := h.accounts.requestUser(c)
		if c.IsAborted() {
			return false
		}
		if authenticated {
			c.Set(accountContext, user)
		}
		if record.Visibility != channels.VisibilityPublic {
			if !authenticated {
				h.accounts.loginRequired(c)
				return false
			}
			if !user.Permissions.Download {
				h.accounts.forbidden(c)
				return false
			}
		}
	}
	if c.GetHeader(seatTokenHeader) == "" || c.GetHeader(claimTokenHeader) == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": channels.ErrUnauthorized.Error()})
		return false
	}
	return true
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
	if record.Visibility == channels.VisibilityPublic {
		return true
	}
	if c.FullPath() == "/api/v1/channels/:id" {
		if h.senderAuthorized(c, record.ID) || h.manager.SeatAuthorized(record.ID, c.GetHeader(seatTokenHeader)) {
			return true
		}
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

func (h *ChannelHandler) authorizeJoin(c *gin.Context, record channels.Record) bool {
	if h.accounts == nil {
		return true
	}
	user, session, ok := h.accounts.requestUser(c)
	if c.IsAborted() {
		return false
	}
	if ok {
		c.Set(accountContext, user)
	}
	if record.Visibility != channels.VisibilityPublic {
		if !ok {
			h.accounts.loginRequired(c)
			return false
		}
		if !user.Permissions.Download {
			h.accounts.forbidden(c)
			return false
		}
	}
	if ok && !h.accounts.csrf(c, session) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf_failed"})
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
	release, err := h.manager.CloseWithContent(c.Param("id"), senderKey)
	h.createMu.Unlock()
	if err != nil {
		if errors.Is(err, channels.ErrUnauthorized) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "channel_close_failed"})
		return
	}
	h.manager.CleanupContentRelease(release)
	if err := h.deleteReleasedContent(release); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "channel_close_cleanup_failed"})
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

func (h *ChannelHandler) deleteReleasedContent(release channels.ContentRelease) error {
	if release.Slug == "" {
		return nil
	}
	if h.shares == nil {
		return errors.New("channel share service is unavailable")
	}
	if err := h.shares.service.DeletePaste(release.Slug); err != nil {
		return err
	}
	h.manager.ContentReleaseDeleted(release)
	return nil
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
	if seatToken := c.GetHeader(seatTokenHeader); seatToken != "" {
		record, err := h.manager.ReceiverStatus(c.Param("id"), seatToken, time.Now())
		if err != nil {
			h.channelError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"state":         record.State,
			"seat_state":    record.Seat.State,
			"pending_until": record.Seat.PendingUntil,
			"content_state": record.ContentState,
		})
		return
	}
	if senderKey, err := c.Cookie(senderCookieName); err == nil {
		record, err := h.manager.SenderStatus(c.Param("id"), senderKey, time.Now())
		if err != nil {
			h.channelError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"state":         record.State,
			"sender_online": record.SenderOnline,
			"content_state": record.ContentState,
			"seat":          record.Seat,
		})
		return
	}
	state, err := h.manager.PublicState(c.Param("id"), time.Now())
	if err != nil {
		h.channelError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"state": state})
}
