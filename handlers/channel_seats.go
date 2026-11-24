package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/channels"
)

func (h *ChannelHandler) joinChannel(channelID, scope, requestID, ip, username string, now time.Time) (channels.JoinResult, error) {
	h.joinMu.Lock()
	defer h.joinMu.Unlock()
	if !h.manager.HasJoinReplay(channelID, scope, requestID, now) {
		if !h.allowJoinLocked(ip, channelID, now) {
			return channels.JoinResult{}, errChannelCreateThrottled
		}
		h.recordJoinLocked(ip, channelID, now)
	}
	return h.manager.Join(channelID, scope, requestID, username, now)
}

func (h *ChannelHandler) allowJoinLocked(ip, channelID string, now time.Time) bool {
	cutoff := now.Add(-createWindow)
	prune := func(windows map[string]createWindowState) {
		for key, window := range windows {
			live := window.attempts[:0]
			for _, at := range window.attempts {
				if at.After(cutoff) {
					live = append(live, at)
				}
			}
			if len(live) == 0 {
				delete(windows, key)
			} else {
				window.attempts = live
				windows[key] = window
			}
		}
	}
	prune(h.joinIPWindows)
	prune(h.joinChannelWindows)

	ipWindow, hasIP := h.joinIPWindows[ip]
	channelWindow, hasChannel := h.joinChannelWindows[channelID]
	if hasIP && len(ipWindow.attempts) >= joinLimitPerIPPerMinute {
		return false
	}
	if !hasIP && len(h.joinIPWindows) >= maxTrackedCreateIPs {
		return false
	}
	if hasChannel && len(channelWindow.attempts) >= joinLimitPerChannelPerMinute {
		return false
	}
	if !hasChannel && len(h.joinChannelWindows) >= channels.MaxChannels {
		return false
	}
	return true
}

func (h *ChannelHandler) recordJoinLocked(ip, channelID string, now time.Time) {
	ipWindow := h.joinIPWindows[ip]
	ipWindow.attempts = append(ipWindow.attempts, now)
	h.joinIPWindows[ip] = ipWindow
	channelWindow := h.joinChannelWindows[channelID]
	channelWindow.attempts = append(channelWindow.attempts, now)
	h.joinChannelWindows[channelID] = channelWindow
}

func (h *ChannelHandler) Join(c *gin.Context) {
	requestID := c.GetHeader("Idempotency-Key")
	if !channels.ValidRequestID(requestID) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": channels.ErrInvalidRequestID.Error()})
		return
	}
	scope := "ip:" + c.ClientIP()
	username := ""
	if user, ok := CurrentAccount(c); ok {
		scope = "account:" + user.ID
		username = user.Username
	}
	result, err := h.joinChannel(c.Param("id"), scope, requestID, c.ClientIP(), username, time.Now())
	if err != nil {
		if errors.Is(err, errChannelCreateThrottled) {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
			return
		}
		h.channelError(c, err)
		return
	}
	body := gin.H{
		"state":       result.State,
		"identity":    result.Identity,
		"anonymous":   result.Anonymous,
		"joined_at":   result.JoinedAt,
		"seat_token":  result.SeatToken,
		"claim_token": result.ClaimToken,
	}
	if !result.PendingUntil.IsZero() {
		body["pending_until"] = result.PendingUntil
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(status, body)
}

func (h *ChannelHandler) Admit(c *gin.Context) {
	requestID := c.GetHeader("Idempotency-Key")
	if !channels.ValidRequestID(requestID) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": channels.ErrInvalidRequestID.Error()})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2048)
	var input struct {
		Decision string
	}
	if c.ShouldBindJSON(&input) != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	senderKey, _ := c.Cookie(senderCookieName)
	result, err := h.manager.Decide(c.Param("id"), senderKey, requestID, input.Decision, time.Now())
	if err != nil {
		h.channelError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"decision":  result.Decision,
		"state":     result.State,
		"identity":  result.Identity,
		"joined_at": result.JoinedAt,
		"replayed":  result.Replayed,
	})
}

func (h *ChannelHandler) Heartbeat(c *gin.Context) {
	senderKey, _ := c.Cookie(senderCookieName)
	if err := h.manager.SenderHeartbeat(c.Param("id"), senderKey, time.Now()); err != nil {
		h.channelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ChannelHandler) SeatHeartbeat(c *gin.Context) {
	if err := h.manager.SeatHeartbeat(c.Param("id"), c.GetHeader(seatTokenHeader), time.Now()); err != nil {
		h.channelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ChannelHandler) Leave(c *gin.Context) {
	if !channels.ValidRequestID(c.GetHeader("Idempotency-Key")) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": channels.ErrInvalidRequestID.Error()})
		return
	}
	if err := h.manager.Leave(c.Param("id"), c.GetHeader("Idempotency-Key"), c.GetHeader(seatTokenHeader), time.Now()); err != nil {
		h.channelError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ChannelHandler) channelError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, channels.ErrInvalidDecision):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, channels.ErrChannelNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, channels.ErrSeatsFull),
		errors.Is(err, channels.ErrDecisionConflict),
		errors.Is(err, channels.ErrIdempotencyConflict):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, channels.ErrSenderOffline),
		errors.Is(err, channels.ErrPendingExpired),
		errors.Is(err, channels.ErrJoinClosed):
		c.AbortWithStatusJSON(http.StatusGone, gin.H{"error": err.Error()})
	case errors.Is(err, channels.ErrUnauthorized):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, channels.ErrCapacity):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "channel_operation_failed"})
	}
}
