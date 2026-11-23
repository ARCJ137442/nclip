package handlers

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/services"
	"github.com/johnwmail/nclip/internal/spool"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
	"github.com/johnwmail/nclip/utils"
)

const (
	minShareTTL = time.Hour
	maxShareTTL = 168 * time.Hour
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
	c.JSON(200, gin.H{"upload_max_bytes": h.effectiveUploadLimit(c), "default_ttl_seconds": int64(h.config.DefaultTTL.Seconds()), "ttl_min_seconds": int64(minShareTTL.Seconds()), "ttl_max_seconds": int64(maxShareTTL.Seconds()), "max_items": maxShareItems, "upload_auth": h.config.UploadAuth, "clipboard_formats": []string{"text/plain", "text/html", "image/png"}})
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
	parts        map[string]*spool.File
	size         int64
	dir          string
}

func newShareUpload(dir string) *shareUpload {
	return &shareUpload{parts: map[string]*spool.File{}, dir: dir}
}

func (u *shareUpload) add(key string, file *spool.File) error {
	if key == "manifest" {
		if u.manifestSeen {
			return fmt.Errorf("invalid_manifest")
		}
		return nil
	}
	if !validPartKey(key) || u.parts[key] != nil || len(u.parts) >= maxShareItems*3 {
		return fmt.Errorf("invalid_parts")
	}
	u.parts[key] = file
	u.size += file.Size
	return nil
}

func (u *shareUpload) cleanup() {
	for _, file := range u.parts {
		_ = file.Remove()
	}
}

func readSharePart(p *multipart.Part, u *shareUpload, contentLimit int64) (string, bool) {
	key := p.FormName()
	if key == "manifest" {
		data, err := io.ReadAll(io.LimitReader(p, maxManifestBytes+1))
		if err != nil || len(data) > maxManifestBytes {
			return "request_too_large", false
		}
		if !u.manifestSeen && json.Unmarshal(data, &u.manifest) == nil {
			u.manifestSeen = true
			return "", true
		}
		return "invalid_manifest", false
	}
	limit := contentLimit - u.size
	if limit < 0 {
		return "content_too_large", false
	}
	file, err := spool.Write(u.dir, "nclip-share-part-*", p, limit, 512)
	if err != nil {
		if err == spool.ErrTooLarge {
			return "content_too_large", false
		}
		return "invalid_parts", false
	}
	if err := u.add(key, file); err != nil {
		_ = file.Remove()
		return err.Error(), false
	}
	return "", true
}

func readShareUpload(c *gin.Context, requestLimit, contentLimit int64, dir string) (*shareUpload, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, requestLimit)
	r, err := c.Request.MultipartReader()
	if err != nil {
		shareError(c, 400, "invalid_multipart")
		return nil, false
	}
	u := newShareUpload(dir)
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			u.cleanup()
			shareError(c, 413, "request_too_large_or_invalid")
			return nil, false
		}
		code, ok := readSharePart(p, u, contentLimit)
		_ = p.Close()
		if !ok {
			u.cleanup()
			status := http.StatusBadRequest
			if code == "content_too_large" || code == "request_too_large" {
				status = http.StatusRequestEntityTooLarge
			}
			shareError(c, status, code)
			return nil, false
		}
	}
	if !u.manifestSeen {
		u.cleanup()
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
	if err != nil || ttl < minShareTTL || ttl > maxShareTTL {
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

func (h *SharesHandler) tempDir() (string, error) {
	base := h.config.DataDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, ".nclip-share-*")
}

// buildStoredShare turns spooled parts into either the original single part or
// a server-built ZIP. Both paths stream bytes; only headers and the manifest
// are held in memory.
func buildStoredShare(m *models.ShareManifest, upload *shareUpload) (*spool.File, string, error) {
	if len(m.Items) == 1 && len(m.Items[0].Parts) == 1 {
		return buildRawShare(m, upload)
	}
	return buildZipShare(m, upload)
}

func buildRawShare(m *models.ShareManifest, upload *shareUpload) (*spool.File, string, error) {
	m.Encoding = "raw"
	part := &m.Items[0].Parts[0]
	part.Entry = ""
	file := upload.parts[part.Key]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	return file, part.MIME, nil
}

func buildZipShare(m *models.ShareManifest, upload *shareUpload) (*spool.File, string, error) {
	m.Encoding = "zip-v1"
	file, err := os.CreateTemp(upload.dir, "nclip-share-archive-*")
	if err != nil {
		return nil, "", err
	}
	path := file.Name()
	published := false
	defer func() {
		if !published {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	zw := zip.NewWriter(file)
	if err := writeShareZipParts(zw, m, upload); err != nil {
		return nil, "", err
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Store})
	if err != nil {
		return nil, "", err
	}
	if _, err := entry.Write(manifest); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	published = true
	return &spool.File{File: file, Path: path, Size: info.Size()}, "application/zip", nil
}

func writeShareZipParts(zw *zip.Writer, m *models.ShareManifest, upload *shareUpload) error {
	for _, item := range m.Items {
		for _, part := range item.Parts {
			source := upload.parts[part.Key]
			if _, err := source.Seek(0, io.SeekStart); err != nil {
				return err
			}
			entry, err := zw.CreateHeader(&zip.FileHeader{Name: part.Entry, Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := io.Copy(entry, source); err != nil {
				return err
			}
		}
	}
	return nil
}

// Create accepts a manifest and named binary fields. Aggregate part bytes are
// limited by the effective user, browser-share, and server caps.
func (h *SharesHandler) Create(c *gin.Context) {
	contentLimit := h.effectiveUploadLimit(c)
	requestLimit := contentLimit + config.MultipartOverheadBytes + maxManifestBytes
	dir, err := h.tempDir()
	if err != nil {
		shareError(c, 500, "temporary_storage_unavailable")
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	u, ok := readShareUpload(c, requestLimit, contentLimit, dir)
	if !ok {
		return
	}
	defer u.cleanup()
	if err := validateManifest(&u.manifest); err != nil {
		shareError(c, 400, err.Error())
		return
	}
	if err := validateShareFiles(&u.manifest, u.parts); err != nil {
		shareError(c, 400, err.Error())
		return
	}
	buildSharePreviews(&u.manifest, u.parts)
	ttl, err := shareTTL(c, h.config.DefaultTTL)
	if err != nil {
		shareError(c, 400, err.Error())
		return
	}
	content, contentType, err := buildStoredShare(&u.manifest, u)
	if err != nil {
		shareError(c, 500, "archive_failed")
		return
	}
	defer func() { _ = content.Remove() }()
	burn := c.GetHeader("X-Burn") == "true"
	visibility := c.GetHeader("X-Nclip-Visibility")
	if visibility != "" && visibility != "authenticated" && visibility != "public" {
		shareError(c, 400, "invalid_visibility")
		return
	}
	resp, err := h.service.CreatePasteStream(services.CreatePasteStreamRequest{
		Content:       content,
		Size:          content.Size,
		ContentType:   contentType,
		TTL:           ttl,
		BurnAfterRead: burn,
		Share:         &u.manifest,
		Public:        visibility == "public",
	})
	if err != nil {
		shareError(c, 500, "store_failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"slug": resp.Slug, "url": h.shareURL(c, resp.Slug), "burn_after_read": burn, "expires_at": resp.ExpiresAt, "public": visibility == "public", "manifest": u.manifest})
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

func writeShareFileHeader(mw *multipart.Writer, key, contentType string) (io.Writer, error) {
	header := make(textproto.MIMEHeader)
	// ASCII filenames work with browser formData(); real names live in the manifest.
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": key, "filename": key}))
	header.Set("Content-Type", contentType)
	return mw.CreatePart(header)
}

// shareBoundary derives stable framing from the immutable share response.
// Stable bytes make HTTP range requests safe across interrupted attempts.
func shareBoundary(p *models.Paste, manifest []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(p.ID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(manifest)
	return fmt.Sprintf("nclip-%x", hash.Sum(nil)[:16])
}

func writeShareMultipart(w io.Writer, boundary string, p *models.Paste, manifest []byte, m models.ShareManifest, stored *storedShare) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		return err
	}
	if err := mw.WriteField("manifest", string(manifest)); err != nil {
		return err
	}
	for _, item := range m.Items {
		for _, part := range item.Parts {
			if err := writeStoredSharePart(mw, stored, part); err != nil {
				return err
			}
		}
	}
	// After consume /raw is gone. Include the ZIP for an in-memory download.
	if p.BurnAfterRead && m.Encoding == "zip-v1" {
		if err := writeStoredSharePart(mw, stored, models.SharePart{Key: "archive", MIME: "application/zip", Size: stored.file.Size}); err != nil {
			return err
		}
	}
	return mw.Close()
}

type storedShare struct {
	file  *spool.File
	zip   *zip.Reader
	parts map[string]*zip.File
}

func openStoredShare(m *models.ShareManifest, file *spool.File) (*storedShare, error) {
	stored := &storedShare{file: file}
	if m.Encoding == "raw" {
		if len(m.Items) != 1 || len(m.Items[0].Parts) != 1 || m.Items[0].Parts[0].Size != file.Size {
			return nil, fmt.Errorf("invalid_archive")
		}
		return stored, nil
	}
	if m.Encoding != "zip-v1" {
		return nil, fmt.Errorf("unsupported_version")
	}
	zr, err := zip.NewReader(file, file.Size)
	if err != nil {
		return nil, err
	}
	stored.zip = zr
	stored.parts = map[string]*zip.File{}
	for _, entry := range zr.File {
		stored.parts[entry.Name] = entry
	}
	for _, item := range m.Items {
		for _, part := range item.Parts {
			entry := stored.parts[part.Entry]
			if entry == nil || int64(entry.UncompressedSize64) != part.Size {
				return nil, fmt.Errorf("invalid_archive")
			}
			reader, err := entry.Open()
			if err != nil {
				return nil, err
			}
			_, copyErr := io.Copy(io.Discard, reader)
			closeErr := reader.Close()
			if copyErr != nil || closeErr != nil {
				return nil, fmt.Errorf("invalid_archive")
			}
		}
	}
	return stored, nil
}

func (s *storedShare) openPart(part models.SharePart) (io.ReadCloser, error) {
	if part.Key == "archive" {
		return io.NopCloser(io.NewSectionReader(s.file, 0, s.file.Size)), nil
	}
	if s.zip == nil {
		return io.NopCloser(io.NewSectionReader(s.file, 0, s.file.Size)), nil
	}
	entry := s.parts[part.Entry]
	if entry == nil {
		return nil, fmt.Errorf("invalid_archive")
	}
	return entry.Open()
}

func writeStoredSharePart(mw *multipart.Writer, stored *storedShare, part models.SharePart) error {
	partWriter, err := writeShareFileHeader(mw, part.Key, part.MIME)
	if err != nil {
		return err
	}
	reader, err := stored.openPart(part)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(partWriter, reader)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("stream part")
	}
	if written != part.Size {
		return fmt.Errorf("part size mismatch")
	}
	return nil
}

// prepareShareResponse writes the complete multipart framing to a temporary
// file. Range serving and one-time delivery can then both use exact bytes
// without holding the payload in memory.
func prepareShareResponse(dir, boundary string, p *models.Paste, manifest []byte, m models.ShareManifest, stored *storedShare) (*spool.File, error) {
	file, err := os.CreateTemp(dir, "nclip-share-response-*")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	published := false
	defer func() {
		if !published {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := writeShareMultipart(file, boundary, p, manifest, m, stored); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	published = true
	return &spool.File{File: file, Path: path, Size: info.Size()}, nil
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
	dir, err := h.tempDir()
	if err != nil {
		shareError(c, 500, "temporary_storage_unavailable")
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	m := manifestFor(p)
	var file *spool.File
	var stored *storedShare
	var prepared *spool.File
	var boundary string
	err = h.service.ReadPasteStream(p.ID, p.BurnAfterRead, func(_ *models.Paste, source io.Reader, size int64) error {
		var writeErr error
		file, writeErr = spool.Write(dir, "nclip-receive-*", source, size, 0)
		if writeErr != nil {
			return writeErr
		}
		if file.Size != size {
			return fmt.Errorf("size_mismatch")
		}
		stored, writeErr = openStoredShare(&m, file)
		if writeErr != nil {
			return writeErr
		}
		manifest, marshalErr := json.Marshal(m)
		if marshalErr != nil {
			return marshalErr
		}
		boundary = shareBoundary(p, manifest)
		prepared, writeErr = prepareShareResponse(dir, boundary, p, manifest, m, stored)
		return writeErr
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			shareError(c, 404, "not_found_or_consumed")
			return
		}
		shareError(c, 500, "invalid_archive_or_size")
		return
	}
	defer func() { _ = file.Remove() }()
	defer func() { _ = prepared.Remove() }()
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Type", "multipart/form-data; boundary="+boundary)
	if !shareResumeAllowed(p) {
		// The current one-time path has consumed its record before writing this
		// response. P1b channel mode will guarantee single-read with an exclusive lock.
		c.Header("Content-Length", strconv.FormatInt(prepared.Size, 10))
		_, _ = io.CopyN(c.Writer, prepared, prepared.Size)
		return
	}
	// Ordinary shares keep their stored copy, so resume is safe.
	c.Header("Accept-Ranges", "bytes")
	http.ServeContent(c.Writer, c.Request, p.ID, time.Time{}, prepared)
}

// shareResumeAllowed is the single server policy point for resumability.
// P1 may allow a locked one-time receive with its scoped delivery token.
func shareResumeAllowed(p *models.Paste) bool {
	return p != nil && !p.BurnAfterRead
}
