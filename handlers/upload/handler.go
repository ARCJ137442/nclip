package upload

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/services"
	"github.com/johnwmail/nclip/internal/spool"
	"github.com/johnwmail/nclip/utils"
)

// Handler handles paste upload operations
type Handler struct {
	service *services.PasteService
	config  *config.Config
}

// NewHandler creates a new upload handler
func NewHandler(service *services.PasteService, config *config.Config) *Handler {
	return &Handler{
		service: service,
		config:  config,
	}
}

// headerEnabled returns true if the given header key is present and not
// explicitly disabled. Presence with an empty value counts as enabled.
// Explicit disabling values (case-insensitive): "0", "false", "no".
func headerEnabled(c *gin.Context, header string) bool {
	vals, ok := c.Request.Header[header]
	if !ok || len(vals) == 0 {
		return false
	}

	// If any value indicates enabled (empty or non-disabling), treat header as enabled.
	// Only return false if all values are explicit disabling tokens.
	for _, raw := range vals {
		v := strings.TrimSpace(raw)
		if v == "" {
			return true
		}
		vl := strings.ToLower(v)
		if vl == "0" || vl == "false" || vl == "no" {
			// this value explicitly disables; keep checking others
			continue
		}
		// any other non-empty value -> enabled
		return true
	}
	// If we got here, every value was an explicit disabling token
	return false
}

// parseTTL parses TTL from X-TTL header or uses default
func (h *Handler) parseTTL(c *gin.Context) (time.Time, error) {
	ttlStr := c.GetHeader("X-TTL")
	if ttlStr != "" {
		d, err := time.ParseDuration(ttlStr)
		minTTL := time.Hour
		maxTTL := 7 * 24 * time.Hour
		if utils.IsDebugEnabled() {
			log.Printf("[DEBUG] Parsed X-TTL duration: %v (raw: %s)", d, ttlStr)
		}
		if err != nil || d < minTTL || d > maxTTL {
			return time.Time{}, fmt.Errorf("X-TTL must be between 1h and 7d")
		}
		return time.Now().Add(d), nil
	}
	return time.Now().Add(h.config.DefaultTTL), nil
}

// addLimit avoids integer overflow while accounting for framing bytes.
func addLimit(a, b int64) int64 {
	maxInt := int64(^uint64(0) >> 1)
	if a > maxInt-b {
		return maxInt
	}
	return a + b
}

func encodedBase64Limit(limit int64) int64 {
	if limit <= 0 {
		return limit
	}
	maxInt := int64(^uint64(0) >> 1)
	if limit > maxInt/4*3 {
		return maxInt
	}
	groups := limit / 3
	remainder := limit % 3
	encoded := groups * 4
	if remainder > 0 {
		encoded += 4
	}
	return encoded + 2
}

func tooLargeReadError(err error) bool {
	if err == nil {
		return false
	}
	var maxBytesError *http.MaxBytesError
	return errors.As(err, &maxBytesError) ||
		errors.Is(err, spool.ErrTooLarge) ||
		strings.Contains(strings.ToLower(err.Error()), "request body too large") ||
		strings.Contains(strings.ToLower(err.Error()), "message too large")
}

func isMultipartContentType(contentTypeHeader string) bool {
	return strings.HasPrefix(contentTypeHeader, "multipart/form-data")
}

// uploadRequestLimit returns the configured limit and whether the middleware
// installed a per-request override that must be enforced with MaxBytesReader.
func (h *Handler) uploadRequestLimit(c *gin.Context) (int64, bool) {
	limit := h.config.BufferSize
	configured, requestLimited := c.Get(config.RequestUploadLimitKey)
	if effective, valid := configured.(int64); valid && effective > 0 {
		limit = effective
	}
	return limit, requestLimited
}

func (h *Handler) tempDir() string {
	if h.config.DataDir == "" {
		return os.TempDir()
	}
	if err := os.MkdirAll(h.config.DataDir, 0o755); err != nil {
		log.Printf("[ERROR] upload temp directory: %v", err)
		return os.TempDir()
	}
	return h.config.DataDir
}

// requestSizeLimit expands the content limit for encoded and multipart bodies.
func requestSizeLimit(limit int64, multipart, base64Enabled bool) (int64, error) {
	requestLimit := limit
	if base64Enabled {
		requestLimit = encodedBase64Limit(limit)
	}
	if multipart {
		requestLimit = addLimit(requestLimit, config.MultipartOverheadBytes)
	}
	if requestLimit <= 0 {
		return 0, fmt.Errorf("invalid buffer size configuration: %d", requestLimit)
	}
	return requestLimit, nil
}

type uploadSource struct {
	file        *spool.File
	filename    string
	contentType string
}

func (h *Handler) readUploadFile(c *gin.Context) (*uploadSource, error) {
	contentTypeHeader := c.Request.Header.Get("Content-Type")
	multipart := isMultipartContentType(contentTypeHeader)
	base64Enabled := headerEnabled(c, "X-Base64")
	limit, requestLimited := h.uploadRequestLimit(c)
	requestLimit, err := requestSizeLimit(limit, multipart, base64Enabled)
	if err != nil {
		return nil, err
	}
	if c.Request.ContentLength > requestLimit {
		return nil, fmt.Errorf("content too large: request exceeds limit of %d bytes", requestLimit)
	}
	if requestLimited || c.Request.ContentLength < 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, requestLimit)
	}

	var filename string
	var contentType string
	encodedLimit := limit
	if base64Enabled {
		encodedLimit = encodedBase64Limit(limit)
	}

	var encoded *spool.File
	if multipart {
		encoded, filename, contentType, err = h.readMultipartUpload(c, encodedLimit)
	} else {
		encoded, contentType, err = h.readDirectUpload(c, encodedLimit)
	}
	if err != nil {
		if tooLargeReadError(err) {
			return nil, fmt.Errorf("content too large: request exceeds limit of %d bytes", requestLimit)
		}
		return nil, err
	}
	content := encoded
	if base64Enabled {
		content, err = h.decodeSpool(encoded, limit)
		_ = encoded.Remove()
		if err != nil {
			return nil, err
		}
	}
	if content.Size == 0 {
		_ = content.Remove()
		return nil, fmt.Errorf("empty content")
	}
	if multipart {
		contentType = utils.DetectContentType(filename, content.Head)
	} else if contentType == "" {
		contentType = utils.DetectContentType("", content.Head)
	}
	return &uploadSource{file: content, filename: filename, contentType: contentType}, nil
}

// decodeSpool tries the encodings accepted by the legacy API without holding
// the encoded or decoded payload in memory.
func (h *Handler) decodeSpool(encoded *spool.File, limit int64) (*spool.File, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	}
	var lastErr error
	for _, encoding := range encodings {
		if _, err := encoded.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		decoded, err := spool.Write(h.tempDir(), "nclip-decode-*", base64.NewDecoder(encoding, encoded), limit, 512)
		if err == nil {
			if utils.IsDebugEnabled() {
				log.Printf("[DEBUG] Base64 decoded: %d bytes -> %d bytes", encoded.Size, decoded.Size)
			}
			return decoded, nil
		}
		lastErr = err
		if errors.Is(err, spool.ErrTooLarge) {
			return nil, fmt.Errorf("decoded content too large: exceeds limit of %d bytes", limit)
		}
	}
	return nil, fmt.Errorf("invalid base64 encoding: %v", lastErr)
}

func (h *Handler) readMultipartUpload(c *gin.Context, limit int64) (*spool.File, string, string, error) {
	reader, err := c.Request.MultipartReader()
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid multipart request")
	}
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			return nil, "", "", fmt.Errorf("no file provided")
		}
		if nextErr != nil {
			return nil, "", "", nextErr
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		filename := part.FileName()
		contentType := part.Header.Get("Content-Type")
		file, copyErr := spool.Write(h.tempDir(), "nclip-upload-*", part, limit, 512)
		_ = part.Close()
		return file, filename, mimeType(contentType), copyErr
	}
}

func (h *Handler) readDirectUpload(c *gin.Context, limit int64) (*spool.File, string, error) {
	file, err := spool.Write(h.tempDir(), "nclip-upload-*", c.Request.Body, limit, 512)
	if err != nil {
		return nil, "", err
	}
	contentType := ""
	if ct := c.ContentType(); ct != "" {
		if parsedType, _, err := mime.ParseMediaType(ct); err == nil {
			contentType = parsedType
		} else {
			contentType = ct
		}
	}
	return file, contentType, nil
}

func mimeType(value string) string {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return parsed
}

// storePasteAndRespond stores paste and responds to client.
func (h *Handler) storePasteAndRespond(c *gin.Context, req services.CreatePasteStreamRequest) {
	resp, err := h.service.CreatePasteStream(req)
	if err != nil {
		// Check if this is a validation error (should return 400) or server error (500)
		errMsg := err.Error()
		if strings.Contains(errMsg, "slug already exists") ||
			strings.Contains(errMsg, "invalid slug format") ||
			strings.Contains(errMsg, "X-TTL must be between") {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
			return
		}
		// For other errors, return 500
		log.Printf("[ERROR] Failed to create paste: %v", err)
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create paste"})
		return
	}

	pasteURL := h.generatePasteURL(c, resp.Slug)
	resp.URL = pasteURL

	// Always return JSON for web UI (browser)
	if h.isCli(c) || c.Request.Header.Get("Accept") == "text/plain" {
		c.String(http.StatusOK, pasteURL+"\n")
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusOK, gin.H{
		"url":             resp.URL,
		"slug":            resp.Slug,
		"burn_after_read": req.BurnAfterRead,
		"size":            req.Size,
		"expires_at":      resp.ExpiresAt,
	})
}

// generatePasteURL generates the full URL for a paste
func (h *Handler) generatePasteURL(c *gin.Context, slug string) string {
	scheme := "http"
	if h.isHTTPS(c) {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s", scheme, c.Request.Host, slug)
}

// isHTTPS detects if the request is over HTTPS
func (h *Handler) isHTTPS(c *gin.Context) bool {
	// Check X-Forwarded-Proto header (common with load balancers/proxies)
	if c.GetHeader("X-Forwarded-Proto") == "https" {
		return true
	}

	// AWS Lambda Function URLs may use different headers
	if c.GetHeader("CloudFront-Forwarded-Proto") == "https" {
		return true
	}

	// Check if the original URL scheme can be detected from request URL
	if strings.HasPrefix(c.Request.Header.Get("Referer"), "https://") {
		return true
	}

	return false
}

// isCli detects if the request is from CLI (curl, wget, Invoke-WebRequest, Invoke-RestMethod, etc.)
func (h *Handler) isCli(c *gin.Context) bool {
	userAgent := strings.ToLower(c.Request.Header.Get("User-Agent"))
	if strings.Contains(userAgent, "curl") ||
		strings.Contains(userAgent, "wget") ||
		strings.Contains(userAgent, "powershell") {
		return true
	}
	return false
}

// Upload handles paste upload via POST /
func tooLargeError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "content too large") || strings.Contains(err.Error(), "decoded content too large"))
}

func (h *Handler) upload(c *gin.Context, burnAfterRead bool) {
	source, err := h.readUploadFile(c)
	if err != nil {
		log.Printf("[ERROR] %v", err)
		c.Header("Content-Type", "application/json; charset=utf-8")
		status := http.StatusBadRequest
		if tooLargeError(err) {
			status = http.StatusRequestEntityTooLarge
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	defer func() { _ = source.file.Remove() }()

	if !burnAfterRead {
		burnAfterRead = headerEnabled(c, "X-Burn") || strings.HasSuffix(c.FullPath(), "/burn/")
	}

	req := services.CreatePasteStreamRequest{
		Content:       source.file,
		Size:          source.file.Size,
		ContentPrefix: source.file.Head,
		Filename:      source.filename,
		ContentType:   source.contentType,
		BurnAfterRead: burnAfterRead,
		Public:        c.GetHeader("X-Nclip-Visibility") == "public",
	}

	// Check for custom slug header
	customSlug := c.GetHeader("X-Slug")
	if customSlug != "" {
		// Validate slug format
		if !utils.IsValidSlug(customSlug) {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid slug format"})
			return
		}
		req.CustomSlug = customSlug
	}

	// Parse TTL
	ttl, err := h.parseTTL(c)
	if err != nil {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.TTL = time.Until(ttl)

	h.storePasteAndRespond(c, req)
}

func (h *Handler) Upload(c *gin.Context) {
	h.upload(c, false)
}

// UploadBurn handles paste upload with burn-after-read via POST /burn/
func (h *Handler) UploadBurn(c *gin.Context) {
	h.upload(c, true)
}
