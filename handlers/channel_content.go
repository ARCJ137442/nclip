package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/internal/services"
	"github.com/johnwmail/nclip/internal/spool"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

const (
	contentManifestRequestLimit = maxManifestBytes + 1024
	zipEntryOverheadBound       = int64(1024)
	maxConcurrentChannelChunks  = 8
)

func channelSenderKey(c *gin.Context) string {
	key, _ := c.Cookie(senderCookieName)
	return key
}

func (h *ChannelHandler) ContentInit(c *gin.Context) {
	if h.shares == nil {
		channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
		return
	}
	requestID := c.GetHeader("Idempotency-Key")
	if !channels.ValidRequestID(requestID) {
		channelContentError(c, http.StatusBadRequest, channels.ErrInvalidRequestID)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, contentManifestRequestLimit)
	var input struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		shareError(c, http.StatusBadRequest, "invalid_manifest")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		shareError(c, http.StatusBadRequest, "invalid_manifest")
		return
	}
	if len(input.Manifest) == 0 || len(input.Manifest) > maxManifestBytes {
		shareError(c, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	var manifest models.ShareManifest
	if json.Unmarshal(input.Manifest, &manifest) != nil {
		shareError(c, http.StatusBadRequest, "invalid_manifest")
		return
	}
	if err := validateManifest(&manifest); err != nil {
		shareError(c, http.StatusBadRequest, err.Error())
		return
	}
	total, err := declaredShareBytes(manifest)
	if err != nil {
		shareError(c, http.StatusBadRequest, "invalid_manifest")
		return
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		shareError(c, http.StatusBadRequest, "invalid_manifest")
		return
	}
	manifestHash := sha256.Sum256(manifestBytes)
	result, replayed, err := h.manager.ReplayContentInit(c.Param("id"), channelSenderKey(c), requestID, manifestHash, time.Now())
	if err != nil {
		channelContentOperationError(c, err)
		return
	}
	if replayed {
		if result.State == channels.ContentAborted {
			channelContentError(c, http.StatusGone, channels.ErrContentClosed)
			return
		}
		writeContentInitResult(c, result, http.StatusOK)
		return
	}

	limit := h.shares.effectiveUploadLimit(c)
	partCount := 0
	for _, item := range manifest.Items {
		partCount += len(item.Parts)
	}
	var reservation int64
	var freeBytes int64
	if limit > 0 && total > 0 && total <= limit {
		_, blockSize, statsErr := storage.FilesystemStats(h.config.DataDir)
		if statsErr != nil {
			channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
			return
		}
		_, finalStoredLocally := h.store.(*storage.FilesystemStore)
		reservation, err = contentPhysicalReservation(limit, partCount, blockSize, finalStoredLocally)
		if err != nil {
			channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
			return
		}
	}
	directory, err := os.MkdirTemp(h.config.DataDir, channelUploadTempPrefix)
	if err != nil {
		channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
		return
	}
	if reservation > 0 {
		freeBytes, _, err = storage.FilesystemStats(h.config.DataDir)
		if err != nil {
			_ = os.RemoveAll(directory)
			channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
			return
		}
	}
	result, err = h.manager.BeginContent(c.Param("id"), channelSenderKey(c), requestID, manifest, manifestHash, total, limit, reservation, freeBytes, directory, time.Now())
	if err != nil {
		_ = os.RemoveAll(directory)
		channelContentOperationError(c, err)
		return
	}
	if result.Replayed {
		_ = os.RemoveAll(directory)
	}
	if result.State == channels.ContentAborted {
		channelContentError(c, http.StatusGone, channels.ErrContentClosed)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeContentInitResult(c, result, status)
}

func declaredShareBytes(manifest models.ShareManifest) (int64, error) {
	var total int64
	for _, item := range manifest.Items {
		for _, part := range item.Parts {
			if part.Size <= 0 || total > math.MaxInt64-part.Size {
				return 0, fmt.Errorf("invalid declared part size")
			}
			total += part.Size
		}
	}
	return total, nil
}

func contentPhysicalReservation(logicalLimit int64, partCount int, blockSize int64, finalStoredLocally bool) (int64, error) {
	if partCount < 1 || partCount > maxShareItems*3 || blockSize <= 0 {
		return 0, fmt.Errorf("invalid physical reservation inputs")
	}
	entries := int64(partCount + 1)
	overhead := int64(maxManifestBytes) + entries*zipEntryOverheadBound
	factor := int64(2) // chunk files plus the assembled archive in the staging filesystem
	files := int64(partCount + 1)
	if finalStoredLocally {
		factor = 3 // parts + archive + final stored copy can coexist during finalize
		overhead *= 2
		files += 2 // final content plus its metadata sidecar
	}
	if files > math.MaxInt64/blockSize {
		return 0, fmt.Errorf("filesystem allocation bound overflows")
	}
	allocationRounding := files * blockSize
	if overhead > math.MaxInt64-allocationRounding {
		return 0, fmt.Errorf("archive overhead bound overflows")
	}
	overhead += allocationRounding
	if logicalLimit <= 0 || logicalLimit > (math.MaxInt64-overhead)/factor {
		return 0, fmt.Errorf("physical reservation overflows")
	}
	return logicalLimit*factor + overhead, nil
}

func writeContentInitResult(c *gin.Context, result channels.ContentInitResult, status int) {
	c.Header("Cache-Control", "no-store")
	c.JSON(status, gin.H{"upload_id": result.UploadID, "state": result.State, "replayed": result.Replayed})
}

func (h *ChannelHandler) ContentChunk(c *gin.Context) {
	if h.manager == nil {
		channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
		return
	}
	if _, err := h.manager.ContentSession(c.Param("id"), channelSenderKey(c), c.Param("uploadID")); err != nil {
		channelContentOperationError(c, err)
		return
	}
	select {
	case h.contentChunkSlots <- struct{}{}:
		defer func() { <-h.contentChunkSlots }()
	default:
		c.Header("Retry-After", "1")
		channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentBusy)
		return
	}
	offset, err := strconv.ParseInt(c.GetHeader("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		channelContentError(c, http.StatusBadRequest, channels.ErrContentOffset)
		return
	}
	hashBytes, err := hex.DecodeString(c.GetHeader("X-Chunk-SHA256"))
	if err != nil || len(hashBytes) != sha256.Size {
		channelContentError(c, http.StatusBadRequest, channels.ErrContentChunk)
		return
	}
	var expectedHash [sha256.Size]byte
	copy(expectedHash[:], hashBytes)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, channels.MaxContentChunkBytes)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		channelContentError(c, http.StatusRequestEntityTooLarge, channels.ErrContentChunk)
		return
	}
	replayed, err := h.manager.AppendContentChunk(c.Param("id"), channelSenderKey(c), c.Param("uploadID"), c.Param("partKey"), offset, data, expectedHash)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			release, _ := h.manager.AbortContentUpload(c.Param("id"), channelSenderKey(c), c.Param("uploadID"))
			_ = h.deleteReleasedContent(release)
			channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
			return
		}
		channelContentOperationError(c, err)
		return
	}
	if replayed {
		c.Header("X-Chunk-Replayed", "true")
	}
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusNoContent)
}

func (h *ChannelHandler) ContentFinalize(c *gin.Context) {
	requestID := c.GetHeader("Idempotency-Key")
	if !channels.ValidRequestID(requestID) {
		channelContentError(c, http.StatusBadRequest, channels.ErrInvalidRequestID)
		return
	}
	session, _, replayed, err := h.manager.BeginContentFinalize(c.Param("id"), channelSenderKey(c), c.Param("uploadID"), requestID)
	if err != nil {
		channelContentOperationError(c, err)
		return
	}
	if replayed {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"state": channels.ContentHeld, "replayed": true})
		return
	}
	var response *services.CreatePasteResponse
	err = session.WithPartFiles(func(paths map[string]string) error {
		parts := make(map[string]*spool.File, len(paths))
		files := make([]*os.File, 0, len(paths))
		defer func() {
			for _, file := range files {
				_ = file.Close()
			}
		}()
		for key, path := range paths {
			file, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			files = append(files, file)
			info, statErr := file.Stat()
			if statErr != nil {
				return statErr
			}
			headSize := min(int64(512), info.Size())
			head := make([]byte, headSize)
			if headSize > 0 {
				_, _ = file.ReadAt(head, 0)
			}
			parts[key] = &spool.File{File: file, Path: path, Size: info.Size(), Head: head}
		}
		response, err = h.shares.CreateEphemeralShare(c.Param("id"), session.Directory, session.Manifest, parts)
		if err != nil {
			return err
		}
		if session.IsAborted() {
			_ = h.shares.service.DeletePaste(response.Slug)
			response = nil
			return channels.ErrContentClosed
		}
		return nil
	})
	if err != nil {
		h.manager.ResetContentFinalize(session)
		if errors.Is(err, syscall.ENOSPC) {
			release, _ := h.manager.AbortContentUpload(c.Param("id"), channelSenderKey(c), session.ID)
			_ = h.deleteReleasedContent(release)
			channelContentError(c, http.StatusServiceUnavailable, channels.ErrContentNotReady)
		} else if errors.Is(err, storage.ErrSlugReserved) {
			channelContentOperationError(c, channels.ErrContentConflict)
		} else if errors.Is(err, channels.ErrContentClosed) {
			channelContentOperationError(c, err)
		} else if errors.Is(err, errShareArchive) {
			channelContentError(c, http.StatusInternalServerError, errShareArchive)
		} else if errors.Is(err, errShareStore) {
			channelContentError(c, http.StatusInternalServerError, errors.New("store_failed"))
		} else {
			channelContentError(c, http.StatusBadRequest, errors.New("invalid_share_content"))
		}
		return
	}
	if response == nil {
		channelContentError(c, http.StatusGone, channels.ErrContentClosed)
		return
	}
	if err := session.RemoveStaging(); err != nil {
		_ = h.shares.service.DeletePaste(response.Slug)
		_, _ = h.manager.AbortContentUpload(c.Param("id"), channelSenderKey(c), session.ID)
		channelContentError(c, http.StatusInternalServerError, errors.New("channel_content_cleanup_failed"))
		return
	}
	heldBytes := int64(0)
	if _, finalStoredLocally := h.store.(*storage.FilesystemStore); finalStoredLocally {
		heldBytes, err = h.manager.HeldPhysicalBytes(response.Size, response.MetadataSize)
		if err != nil {
			_ = h.shares.service.DeletePaste(response.Slug)
			release, _ := h.manager.AbortContentUpload(c.Param("id"), channelSenderKey(c), session.ID)
			_ = h.deleteReleasedContent(release)
			channelContentOperationError(c, err)
			return
		}
	}
	if err := h.manager.CompleteContentFinalize(c.Param("id"), channelSenderKey(c), session, response.Slug, heldBytes); err != nil {
		_ = h.shares.service.DeletePaste(response.Slug)
		release, _ := h.manager.AbortContentUpload(c.Param("id"), channelSenderKey(c), session.ID)
		_ = h.deleteReleasedContent(release)
		channelContentOperationError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"state": channels.ContentHeld, "replayed": false})
}

func (h *ChannelHandler) ContentAbort(c *gin.Context) {
	release, err := h.manager.AbortContentUpload(c.Param("id"), channelSenderKey(c), c.Param("uploadID"))
	if err != nil {
		channelContentOperationError(c, err)
		return
	}
	if err := h.deleteReleasedContent(release); err != nil {
		channelContentError(c, http.StatusInternalServerError, errors.New("channel_content_cleanup_failed"))
		return
	}
	c.Status(http.StatusNoContent)
}

func channelContentOperationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, channels.ErrChannelNotFound):
		channelContentError(c, http.StatusNotFound, err)
	case errors.Is(err, channels.ErrInvalidRequestID), errors.Is(err, channels.ErrInvalidSeatID):
		channelContentError(c, http.StatusBadRequest, err)
	case errors.Is(err, channels.ErrUnauthorized):
		channelContentError(c, http.StatusForbidden, err)
	case errors.Is(err, channels.ErrContentTooLarge):
		channelContentError(c, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, channels.ErrContentClosed), errors.Is(err, channels.ErrPendingExpired):
		channelContentError(c, http.StatusGone, err)
	case errors.Is(err, channels.ErrContentConflict), errors.Is(err, channels.ErrContentOffset), errors.Is(err, channels.ErrContentChunk), errors.Is(err, channels.ErrContentIncomplete), errors.Is(err, channels.ErrContentBusy), errors.Is(err, channels.ErrIdempotencyConflict):
		channelContentError(c, http.StatusConflict, err)
	case errors.Is(err, channels.ErrContentNotReady), errors.Is(err, channels.ErrCapacity):
		channelContentError(c, http.StatusServiceUnavailable, err)
	default:
		channelContentError(c, http.StatusInternalServerError, errors.New("channel_content_failed"))
	}
}

func channelContentError(c *gin.Context, status int, err error) {
	shareError(c, status, err.Error())
}
