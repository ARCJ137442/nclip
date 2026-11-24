package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
)

const senderCookieName = "nclip_sender_key"

type ChannelHandler struct {
	manager  *channels.Manager
	config   *config.Config
	accounts *AccountHandler
}

func NewChannelHandler(manager *channels.Manager, cfg *config.Config, accounts *AccountHandler) *ChannelHandler {
	return &ChannelHandler{manager: manager, config: cfg, accounts: accounts}
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
	if record.Visibility == channels.VisibilityPublic || h.senderAuthorized(c, record.ID) {
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

func (h *ChannelHandler) senderAuthorized(c *gin.Context, channelID string) bool {
	senderKey, err := c.Cookie(senderCookieName)
	return err == nil && h.manager.SenderAuthorized(channelID, senderKey)
}
func (h *ChannelHandler) Create(c *gin.Context) {
	requestID := c.GetHeader("Idempotency-Key")
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

	record, senderKey, _, err := h.manager.Create(channelCreateScope(c, h.config), requestID, visibility)
	if errors.Is(err, channels.ErrInvalidRequestID) || errors.Is(err, channels.ErrInvalidVisibility) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
