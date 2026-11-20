package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/services"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/utils"
)

// SharesHandler adapts multipart clipboard messages to the existing paste store.
type SharesHandler struct {
	service     *services.PasteService
	config      *config.Config
	uploadLimit *config.UploadLimit
}

// NewSharesHandler creates the web sharing API without introducing another store.
func NewSharesHandler(service *services.PasteService, cfg *config.Config) *SharesHandler {
	return &SharesHandler{service: service, config: cfg, uploadLimit: cfg.UploadLimit}
}

// UIConfig contains only public, authoritative settings used by the composer.
func (h *SharesHandler) UIConfig(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"upload_max_bytes": h.effectiveUploadLimit(c), "default_ttl_seconds": int64(h.config.DefaultTTL.Seconds()), "max_items": maxShareItems, "upload_auth": h.config.UploadAuth, "clipboard_formats": []string{"text/plain", "text/html", "image/png"}})
}

func (h *SharesHandler) effectiveUploadLimit(c *gin.Context) int64 {
	user, authenticated := CurrentAccount(c)
	if !authenticated {
		limit := h.config.BufferSize
		if limit <= 0 {
			return 0
		}
		if h.uploadLimit != nil {
			if shared, _, _ := h.uploadLimit.Snapshot(); shared < limit {
				limit = shared
			}
		}
		return limit
	}

	limit := EffectiveUploadLimit(user, h.config.BufferSize)
	if user.UploadLimitBytes != nil && h.uploadLimit != nil {
		if shared, _, _ := h.uploadLimit.Snapshot(); shared < limit {
			limit = shared
		}
	}
	return limit
}

func shareError(c *gin.Context, status int, code string) { c.JSON(status, gin.H{"error": code}) }

type shareUpload struct {
	manifest     models.ShareManifest
	manifestSeen bool
	parts        map[string][]byte
	size         int64
}

func (u *shareUpload) add(key string, data []byte) error {
	if key == "manifest" {
		if u.manifestSeen || json.Unmarshal(data, &u.manifest) != nil {
			return fmt.Errorf("invalid_manifest")
		}
		u.manifestSeen = true
		return nil
	}
	if !validPartKey(key) || u.parts[key] != nil || len(u.parts) >= maxShareItems*3 {
		return fmt.Errorf("invalid_parts")
	}
	u.parts[key] = data
	u.size += int64(len(data))
	return nil
}

func readSharePart(p *multipart.Part, u *shareUpload, contentLimit int64) (string, bool) {
	key := p.FormName()
	limit := contentLimit - u.size
	if key == "manifest" {
		limit = maxManifestBytes
	} else if limit > contentLimit {
		limit = contentLimit
	}
	if limit < 0 {
		return "content_too_large", false
	}
	data, err := io.ReadAll(io.LimitReader(p, limit+1))
	if err != nil || int64(len(data)) > limit {
		if key == "manifest" {
			return "request_too_large", false
		}
		return "content_too_large", false
	}
	if key != "manifest" && u.size+int64(len(data)) > contentLimit {
		return "content_too_large", false
	}
	if err := u.add(key, data); err != nil {
		return err.Error(), false
	}
	return "", true
}

func readShareUpload(c *gin.Context, requestLimit, contentLimit int64) (*shareUpload, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, requestLimit)
	r, err := c.Request.MultipartReader()
	if err != nil {
		shareError(c, 400, "invalid_multipart")
		return nil, false
	}
	u := &shareUpload{parts: map[string][]byte{}}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			shareError(c, 413, "request_too_large_or_invalid")
			return nil, false
		}
		code, ok := readSharePart(p, u, contentLimit)
		if !ok {
			status := http.StatusBadRequest
			if code == "content_too_large" || code == "request_too_large" {
				status = http.StatusRequestEntityTooLarge
			}
			shareError(c, status, code)
			return nil, false
		}
	}
	if !u.manifestSeen {
		shareError(c, 400, "invalid_manifest")
		return nil, false
	}
	return u, true
}

func shareTTL(c *gin.Context, defaultTTL time.Duration) (time.Duration, error) {
	v := c.GetHeader("X-TTL")
	if v == "" {
		return defaultTTL, nil
	}
	ttl, err := time.ParseDuration(v)
	if err != nil || ttl < time.Hour || ttl > 7*24*time.Hour {
		return 0, fmt.Errorf("invalid_ttl")
	}
	return ttl, nil
}

func (h *SharesHandler) shareURL(c *gin.Context, slug string) string {
	base := strings.TrimRight(h.config.URL, "/")
	if base == "" {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + c.Request.Host
	}
	return base + "/" + slug
}

// Create accepts a manifest and named binary fields. Aggregate part bytes are
// limited by the effective user, browser-share, and server caps.
func (h *SharesHandler) Create(c *gin.Context) {
	contentLimit := h.effectiveUploadLimit(c)
	requestLimit := contentLimit + config.MultipartOverheadBytes + maxManifestBytes
	u, ok := readShareUpload(c, requestLimit, contentLimit)
	if !ok {
		return
	}
	if err := validateManifest(&u.manifest); err != nil {
		shareError(c, 400, err.Error())
		return
	}
	if err := validateSharePayload(&u.manifest, u.parts); err != nil {
		shareError(c, 400, err.Error())
		return
	}
	ttl, err := shareTTL(c, h.config.DefaultTTL)
	if err != nil {
		shareError(c, 400, err.Error())
		return
	}
	content, contentType, err := encodeStoredShare(&u.manifest, u.parts)
	if err != nil {
		shareError(c, 500, "archive_failed")
		return
	}
	burn := c.GetHeader("X-Burn") == "true"
	visibility := c.GetHeader("X-Nclip-Visibility")
	if visibility != "" && visibility != "authenticated" && visibility != "public" {
		shareError(c, 400, "invalid_visibility")
		return
	}
	resp, err := h.service.CreatePaste(services.CreatePasteRequest{Content: content, ContentType: contentType, TTL: ttl, BurnAfterRead: burn, Share: &u.manifest, Public: visibility == "public"})
	if err != nil {
		shareError(c, 500, "store_failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"slug": resp.Slug, "url": h.shareURL(c, resp.Slug), "burn_after_read": burn, "expires_at": resp.ExpiresAt, "public": visibility == "public"})
}

func (h *SharesHandler) get(c *gin.Context) (*models.Paste, bool) {
	if !utils.IsValidSlug(c.Param("slug")) {
		shareError(c, 400, "invalid_slug")
		return nil, false
	}
	p, err := h.service.GetPaste(c.Param("slug"))
	if err != nil {
		shareError(c, 404, "not_found")
		return nil, false
	}
	c.Header("Cache-Control", "no-store")
	return p, true
}

// Metadata never reads content or consumes a one-time message.
func (h *SharesHandler) Metadata(c *gin.Context) {
	p, ok := h.get(c)
	if !ok {
		return
	}
	c.JSON(200, gin.H{"id": p.ID, "size": p.Size, "created_at": p.CreatedAt, "expires_at": p.ExpiresAt, "burn_after_read": p.BurnAfterRead, "manifest": manifestFor(p), "public": p.Public})
}

// Content preloads ordinary messages; one-time messages require explicit POST.
func (h *SharesHandler) Content(c *gin.Context) { h.sendContent(c, false) }

// Receive consumes once; browser clipboard success is a separate client fact.
func (h *SharesHandler) Receive(c *gin.Context) {
	if c.GetHeader("X-Nclip-Confirm") != "1" {
		shareError(c, 400, "confirmation_required")
		return
	}
	h.sendContent(c, true)
}

func writeShareFile(mw *multipart.Writer, key, contentType string, data []byte) error {
	header := make(textproto.MIMEHeader)
	// ASCII filenames work with browser formData(); real names live in the manifest.
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": key, "filename": key}))
	header.Set("Content-Type", contentType)
	w, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// shareBoundary returns one boundary for both the sizing and writing passes.
// Stable framing lets the response declare Content-Length, so browsers can
// show determinate download progress instead of an unknown stream.
func shareBoundary() string {
	return multipart.NewWriter(io.Discard).Boundary()
}

func writeShareMultipart(w io.Writer, boundary string, p *models.Paste, manifest []byte, m models.ShareManifest, parts map[string][]byte, content []byte) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		return err
	}
	if err := mw.WriteField("manifest", string(manifest)); err != nil {
		return err
	}
	for _, item := range m.Items {
		for _, part := range item.Parts {
			if err := writeShareFile(mw, part.Key, part.MIME, parts[part.Key]); err != nil {
				return err
			}
		}
	}
	// After consume /raw is gone. Include the ZIP for an in-memory download.
	if p.BurnAfterRead && m.Encoding == "zip-v1" {
		if err := writeShareFile(mw, "archive", "application/zip", content); err != nil {
			return err
		}
	}
	return mw.Close()
}

type byteCounter int64

func (c *byteCounter) Write(data []byte) (int, error) {
	*c += byteCounter(len(data))
	return len(data), nil
}

// shareResponseSize measures the exact multipart body before it is written.
// The measurement pass uses no payload-sized buffer; only the write pass does.
func shareResponseSize(boundary string, p *models.Paste, manifest []byte, m models.ShareManifest, parts map[string][]byte, content []byte) (int64, error) {
	var size byteCounter
	if err := writeShareMultipart(&size, boundary, p, manifest, m, parts, content); err != nil {
		return 0, err
	}
	return int64(size), nil
}

func (h *SharesHandler) sendContent(c *gin.Context, consume bool) {
	p, ok := h.get(c)
	if !ok {
		return
	}
	if p.BurnAfterRead && !consume {
		shareError(c, 409, "confirmation_required")
		return
	}
	content, err := h.service.GetPasteContent(p.ID)
	if err != nil {
		shareError(c, 404, "not_found")
		return
	}
	// Validate the full archive and measure the response before the
	// irreversible consume operation.
	m, parts, err := unpackShare(p, content)
	if err != nil {
		shareError(c, 500, "invalid_archive_or_size")
		return
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		shareError(c, 500, "invalid_archive_or_size")
		return
	}
	boundary := shareBoundary()
	size, sizeErr := shareResponseSize(boundary, p, manifest, m, parts, content)
	if p.BurnAfterRead {
		if _, _, err = h.service.ConsumePaste(p.ID); err != nil {
			shareError(c, 404, "not_found_or_consumed")
			return
		}
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Type", "multipart/form-data; boundary="+boundary)
	if sizeErr == nil {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	}
	if err := writeShareMultipart(c.Writer, boundary, p, manifest, m, parts, content); err != nil {
		// The response may be partial, so do not append JSON or replay consumption.
		_ = c.Error(err)
	}
}
