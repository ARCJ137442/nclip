package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/services"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/utils"
)

const maxShareItems = 32
const maxManifestBytes = 64 * 1024

// SharesHandler adapts multipart clipboard messages to the existing paste store.
type SharesHandler struct {
	service *services.PasteService
	config  *config.Config
}

// NewSharesHandler creates the web sharing API without introducing another store.
func NewSharesHandler(service *services.PasteService, cfg *config.Config) *SharesHandler {
	return &SharesHandler{service: service, config: cfg}
}

// UIConfig contains only public, authoritative settings used by the composer.
func (h *SharesHandler) UIConfig(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"upload_max_bytes": h.config.BufferSize, "default_ttl_seconds": int64(h.config.DefaultTTL.Seconds()), "max_items": maxShareItems, "upload_auth": h.config.UploadAuth, "clipboard_formats": []string{"text/plain", "text/html", "image/png"}})
}

func shareError(c *gin.Context, status int, code string) { c.JSON(status, gin.H{"error": code}) }

func validPartKey(key string) bool {
	if len(key) == 0 || len(key) > 40 {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}

func cleanShareName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "" || name == "/" {
		return "content"
	}
	if len(name) > 200 {
		name = string([]rune(name)[:min(len([]rune(name)), 60)])
	}
	return name
}

func validateManifest(m *models.ShareManifest) error {
	if m.Version != 1 || len(m.Items) == 0 || len(m.Items) > maxShareItems {
		return fmt.Errorf("invalid_manifest")
	}
	seen := map[string]bool{}
	for i := range m.Items {
		item := &m.Items[i]
		item.Name = cleanShareName(item.Name)
		if item.Kind != "clipboard" && item.Kind != "file" {
			return fmt.Errorf("invalid_manifest")
		}
		if len(item.Parts) == 0 || len(item.Parts) > 3 || item.Kind == "file" && len(item.Parts) != 1 {
			return fmt.Errorf("invalid_manifest")
		}
		formats := map[string]bool{}
		for j := range item.Parts {
			p := &item.Parts[j]
			if !validPartKey(p.Key) || p.Key == "manifest" || p.Key == "archive" || seen[p.Key] {
				return fmt.Errorf("invalid_manifest")
			}
			seen[p.Key] = true
			mt, _, err := mime.ParseMediaType(p.MIME)
			if err != nil || formats[mt] {
				return fmt.Errorf("invalid_manifest")
			}
			formats[mt] = true
			p.MIME = mt
			if item.Kind == "clipboard" && mt != "text/plain" && mt != "text/html" && mt != "image/png" {
				return fmt.Errorf("unsupported_format")
			}
			entryName := item.Name
			if item.Kind == "clipboard" {
				entryName = "clipboard" + utils.ExtensionByMime(p.MIME)
			}
			p.Entry = fmt.Sprintf("items/%02d/%02d-%s", i, j, entryName)
		}
	}
	return nil
}

// Create accepts a JSON manifest plus named binary fields. The entire request,
// including multipart overhead, is bounded; no clipboard data enters log output.
func (h *SharesHandler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.config.BufferSize)
	r, err := c.Request.MultipartReader()
	if err != nil {
		shareError(c, 400, "invalid_multipart")
		return
	}
	var m models.ShareManifest
	parts := map[string][]byte{}
	manifestSeen := false
	var total int64
	for {
		p, readErr := r.NextPart()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			shareError(c, 413, "request_too_large_or_invalid")
			return
		}
		key := p.FormName()
		limit := h.config.BufferSize
		if key == "manifest" {
			limit = maxManifestBytes
		}
		data, readErr := io.ReadAll(io.LimitReader(p, limit+1))
		if readErr != nil || int64(len(data)) > limit {
			shareError(c, 413, "request_too_large")
			return
		}
		if key == "manifest" {
			if manifestSeen || json.Unmarshal(data, &m) != nil {
				shareError(c, 400, "invalid_manifest")
				return
			}
			manifestSeen = true
		} else {
			if !validPartKey(key) || parts[key] != nil || len(parts) >= maxShareItems*3 {
				shareError(c, 400, "invalid_parts")
				return
			}
			parts[key] = data
			total += int64(len(data))
			if total > h.config.BufferSize {
				shareError(c, 413, "content_too_large")
				return
			}
		}
	}
	if !manifestSeen {
		shareError(c, 400, "invalid_manifest")
		return
	}
	if err := validateManifest(&m); err != nil {
		shareError(c, 400, err.Error())
		return
	}
	used := 0
	for i := range m.Items {
		for j := range m.Items[i].Parts {
			p := &m.Items[i].Parts[j]
			data, ok := parts[p.Key]
			if !ok || len(data) == 0 {
				shareError(c, 400, "empty_or_missing_part")
				return
			}
			if m.Items[i].Kind == "clipboard" {
				if strings.HasPrefix(p.MIME, "text/") && !utf8.Valid(data) || p.MIME == "image/png" && http.DetectContentType(data) != "image/png" {
					shareError(c, 400, "invalid_clipboard_data")
					return
				}
			}
			p.Size = int64(len(data))
			used++
		}
	}
	if used != len(parts) {
		shareError(c, 400, "unexpected_part")
		return
	}
	ttl := h.config.DefaultTTL
	if v := c.GetHeader("X-TTL"); v != "" {
		ttl, err = time.ParseDuration(v)
		if err != nil || ttl < time.Hour || ttl > 7*24*time.Hour {
			shareError(c, 400, "invalid_ttl")
			return
		}
	}
	contentType := "application/zip"
	var content []byte
	if len(m.Items) == 1 && len(m.Items[0].Parts) == 1 {
		m.Encoding = "raw"
		part := &m.Items[0].Parts[0]
		part.Entry = ""
		content = parts[part.Key]
		contentType = part.MIME
	} else {
		m.Encoding = "zip-v1"
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, item := range m.Items {
			for _, part := range item.Parts {
				w, writeErr := zw.CreateHeader(&zip.FileHeader{Name: part.Entry, Method: zip.Store})
				if writeErr == nil {
					_, writeErr = w.Write(parts[part.Key])
				}
				if writeErr != nil {
					shareError(c, 500, "archive_failed")
					return
				}
			}
		}
		manifest, marshalErr := json.Marshal(m)
		if marshalErr != nil {
			shareError(c, 500, "archive_failed")
			return
		}
		w, writeErr := zw.Create("manifest.json")
		if writeErr == nil {
			_, writeErr = w.Write(manifest)
		}
		if writeErr != nil || zw.Close() != nil {
			shareError(c, 500, "archive_failed")
			return
		}
		content = buf.Bytes()
	}
	burn := c.GetHeader("X-Burn") == "true"
	resp, err := h.service.CreatePaste(services.CreatePasteRequest{Content: content, ContentType: contentType, TTL: ttl, BurnAfterRead: burn, Share: &m})
	if err != nil {
		shareError(c, 500, "store_failed")
		return
	}
	base := strings.TrimRight(h.config.URL, "/")
	if base == "" {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + c.Request.Host
	}
	c.JSON(http.StatusCreated, gin.H{"slug": resp.Slug, "url": base + "/" + resp.Slug, "burn_after_read": burn, "expires_at": resp.ExpiresAt})
}

func manifestFor(p *models.Paste) models.ShareManifest {
	if p.Share != nil {
		return *p.Share
	}
	kind := "file"
	contentType, _, err := mime.ParseMediaType(p.ContentType)
	if err != nil {
		contentType = "application/octet-stream"
	}
	if strings.HasPrefix(contentType, "text/") || contentType == "image/png" {
		kind = "clipboard"
	}
	return models.ShareManifest{Version: 1, Encoding: "raw", Items: []models.ShareItem{{Kind: kind, Name: p.ID + utils.ExtensionByMime(contentType), Parts: []models.SharePart{{Key: "original", MIME: contentType, Size: p.Size}}}}}
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
	c.JSON(200, gin.H{"id": p.ID, "size": p.Size, "created_at": p.CreatedAt, "expires_at": p.ExpiresAt, "burn_after_read": p.BurnAfterRead, "manifest": manifestFor(p)})
}

func unpackShare(p *models.Paste, content []byte) (models.ShareManifest, map[string][]byte, error) {
	m := manifestFor(p)
	parts := map[string][]byte{}
	if int64(len(content)) != p.Size {
		return m, nil, fmt.Errorf("size_mismatch")
	}
	if m.Encoding == "raw" {
		if len(m.Items) != 1 || len(m.Items[0].Parts) != 1 {
			return m, nil, fmt.Errorf("invalid_archive")
		}
		parts[m.Items[0].Parts[0].Key] = content
		return m, parts, nil
	}
	if m.Encoding != "zip-v1" {
		return m, nil, fmt.Errorf("unsupported_version")
	}
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return m, nil, err
	}
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	for _, item := range m.Items {
		for _, part := range item.Parts {
			f := entries[part.Entry]
			if f == nil || part.Size < 0 || part.Size > int64(len(content)) {
				return m, nil, fmt.Errorf("invalid_archive")
			}
			r, openErr := f.Open()
			if openErr != nil {
				return m, nil, openErr
			}
			data, readErr := io.ReadAll(io.LimitReader(r, part.Size+1))
			closeErr := r.Close()
			if readErr != nil || closeErr != nil || int64(len(data)) != part.Size {
				return m, nil, fmt.Errorf("invalid_archive")
			}
			parts[part.Key] = data
		}
	}
	return m, parts, nil
}

// Content preloads ordinary messages; one-time messages require explicit POST.
func (h *SharesHandler) Content(c *gin.Context) { h.sendContent(c, false) }

// Receive consumes once. Browser clipboard success is a separate, client-side fact.
func (h *SharesHandler) Receive(c *gin.Context) {
	if c.GetHeader("X-Nclip-Confirm") != "1" {
		shareError(c, 400, "confirmation_required")
		return
	}
	h.sendContent(c, true)
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
	// Prepare and validate the archive before the irreversible consume operation.
	m, parts, err := unpackShare(p, content)
	if err != nil {
		shareError(c, 500, "invalid_archive_or_size")
		return
	}
	if p.BurnAfterRead {
		_, _, err = h.service.ConsumePaste(p.ID)
		if err != nil {
			shareError(c, 404, "not_found_or_consumed")
			return
		}
	}
	c.Header("X-Content-Type-Options", "nosniff")
	mw := multipart.NewWriter(c.Writer)
	c.Header("Content-Type", mw.FormDataContentType())
	manifest, err := json.Marshal(m)
	if err != nil {
		shareError(c, 500, "invalid_manifest")
		return
	}
	if err := mw.WriteField("manifest", string(manifest)); err != nil {
		return
	}
	for _, item := range m.Items {
		for _, part := range item.Parts {
			header := make(textproto.MIMEHeader)
			// Browser formData() does not consistently recognize RFC 5987 filename*.
			// Use the ASCII part key on the wire; the manifest retains the real name.
			header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": part.Key, "filename": part.Key}))
			header.Set("Content-Type", part.MIME)
			w, writeErr := mw.CreatePart(header)
			if writeErr != nil {
				return
			}
			if _, writeErr = w.Write(parts[part.Key]); writeErr != nil {
				return
			}
		}
	}
	// After consume, /raw no longer exists. Include the original ZIP so the
	// browser can offer a standard bundle download without a client ZIP library.
	if p.BurnAfterRead && m.Encoding == "zip-v1" {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "archive", "filename": p.ID + ".zip"}))
		header.Set("Content-Type", "application/zip")
		w, writeErr := mw.CreatePart(header)
		if writeErr != nil {
			return
		}
		if _, writeErr = w.Write(content); writeErr != nil {
			return
		}
	}
	if err := mw.Close(); err != nil {
		return
	}
}
