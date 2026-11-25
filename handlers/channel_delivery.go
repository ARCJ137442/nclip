package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/internal/spool"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

type channelResponsePlan struct {
	manifest      models.ShareManifest
	manifestBytes []byte
	boundary      string
	etag          string
	size          int64
}

type preparedChannelResponse struct {
	directory string
	source    *spool.File
	response  *spool.File
	plan      channelResponsePlan
}

func (p *preparedChannelResponse) close() error {
	if p == nil {
		return nil
	}
	if p.source != nil {
		_ = p.source.Remove()
	}
	if p.response != nil {
		_ = p.response.Remove()
	}
	if p.directory != "" {
		if err := os.RemoveAll(p.directory); err != nil {
			return err
		}
		if _, err := os.Stat(p.directory); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("channel response temporary directory remains")
			}
			return err
		}
	}
	return nil
}

type countingWriter struct{ size int64 }

func (w *countingWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > math.MaxInt64-w.size {
		return 0, fmt.Errorf("multipart response size overflows")
	}
	w.size += int64(len(data))
	return len(data), nil
}

func planChannelResponse(paste *models.Paste) (channelResponsePlan, error) {
	manifest := manifestFor(paste)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return channelResponsePlan{}, err
	}
	boundary := shareBoundary(paste, manifestBytes)
	counter := &countingWriter{}
	writer := multipart.NewWriter(counter)
	if err := writer.SetBoundary(boundary); err != nil {
		return channelResponsePlan{}, err
	}
	if err := writer.WriteField("manifest", string(manifestBytes)); err != nil {
		return channelResponsePlan{}, err
	}
	payloadBytes := int64(0)
	for _, item := range manifest.Items {
		for _, part := range item.Parts {
			if part.Size < 0 || part.Size > math.MaxInt64-payloadBytes {
				return channelResponsePlan{}, fmt.Errorf("multipart payload size overflows")
			}
			payloadBytes += part.Size
			if _, err := writeShareFileHeader(writer, part.Key, part.MIME); err != nil {
				return channelResponsePlan{}, err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return channelResponsePlan{}, err
	}
	if paste.BurnAfterRead && manifest.Encoding == "zip-v1" {
		return channelResponsePlan{}, fmt.Errorf("channel content cannot be burn-after-read")
	}
	if payloadBytes > math.MaxInt64-counter.size {
		return channelResponsePlan{}, fmt.Errorf("multipart response size overflows")
	}
	return channelResponsePlan{
		manifest:      manifest,
		manifestBytes: manifestBytes,
		boundary:      boundary,
		etag:          `"` + boundary + `"`,
		size:          counter.size + payloadBytes,
	}, nil
}

func (h *ChannelHandler) prepareChannelResponse(lease *channels.DeliveryLease, slug string) (*preparedChannelResponse, error) {
	paste, err := h.shares.service.GetPaste(slug)
	if err != nil {
		return nil, err
	}
	if !paste.IsEphemeral() || paste.EphemeralChannelID != lease.ChannelID {
		return nil, storage.ErrNotFound
	}
	plan, err := planChannelResponse(paste)
	if err != nil {
		return nil, err
	}
	reservation, err := h.manager.HeldPhysicalBytes(paste.Size, plan.size)
	if err != nil {
		return nil, err
	}
	freeBytes, _, err := storage.FilesystemStats(h.config.DataDir)
	if err != nil {
		return nil, err
	}
	if err := h.manager.ReserveDeliverySpace(lease, reservation, freeBytes); err != nil {
		return nil, err
	}
	reservationHeld := true
	defer func() {
		if reservationHeld {
			h.manager.ReleaseDeliverySpace(lease)
		}
	}()
	prepared, err := h.materializeChannelResponse(lease, slug, paste, plan, reservation)
	if err != nil {
		if cleanupErr := prepared.close(); cleanupErr != nil {
			reservationHeld = false
			log.Printf("[WARN] failed channel materialization left temporary data; retaining reservation: %v", cleanupErr)
			return nil, fmt.Errorf("%w; temporary cleanup failed: %v", err, cleanupErr)
		}
		return nil, err
	}
	lease.SetSource(prepared.response.File)
	reservationHeld = false
	return prepared, nil
}

func (h *ChannelHandler) materializeChannelResponse(lease *channels.DeliveryLease, slug string, paste *models.Paste, plan channelResponsePlan, reservation int64) (*preparedChannelResponse, error) {
	directory, err := h.shares.tempDir()
	if err != nil {
		return nil, err
	}
	prepared := &preparedChannelResponse{directory: directory, plan: plan}
	if err := h.readAndBuildChannelResponse(lease, slug, paste, plan, reservation, prepared); err != nil {
		return prepared, err
	}
	return prepared, nil
}

func (h *ChannelHandler) readAndBuildChannelResponse(lease *channels.DeliveryLease, slug string, paste *models.Paste, plan channelResponsePlan, reservation int64, prepared *preparedChannelResponse) error {
	return h.shares.service.ReadPasteStream(slug, false, func(_ *models.Paste, source io.Reader, size int64) error {
		if closer, ok := source.(io.Closer); ok {
			lease.SetSource(closer)
		}
		file, err := spool.Write(prepared.directory, "nclip-channel-read-source-*", contextReader{ctx: lease.Context(), source: source}, size, 0)
		if err != nil {
			return err
		}
		prepared.source = file
		if file.Size != size {
			return fmt.Errorf("size_mismatch")
		}
		stored, err := openStoredShareContext(lease.Context(), &plan.manifest, file)
		if err != nil {
			return err
		}
		response, err := prepareShareResponseContext(lease.Context(), prepared.directory, plan.boundary, paste, plan.manifestBytes, plan.manifest, stored)
		if err != nil {
			return err
		}
		prepared.response = response
		if response.Size != plan.size {
			return fmt.Errorf("multipart_size_mismatch")
		}
		return h.manager.MaterializeDeliverySpace(lease, reservation)
	})
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

type contextWriter struct {
	ctx         context.Context
	destination io.Writer
}

func (w contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.destination.Write(data)
}

type byteRange struct {
	start int64
	end   int64
}

func parseChannelRange(value string, total int64) (byteRange, error) {
	if total <= 0 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return byteRange{}, fmt.Errorf("invalid range")
	}
	spec := strings.TrimPrefix(value, "bytes=")
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return byteRange{}, fmt.Errorf("invalid range")
	}
	if parts[0] == "" {
		return parseSuffixRange(parts[1], total)
	}
	return parseExplicitRange(parts[0], parts[1], total)
}

func parseSuffixRange(value string, total int64) (byteRange, error) {
	suffix, err := strconv.ParseInt(value, 10, 64)
	if err != nil || suffix <= 0 {
		return byteRange{}, fmt.Errorf("invalid range")
	}
	if suffix > total {
		suffix = total
	}
	return byteRange{start: total - suffix, end: total - 1}, nil
}

func parseExplicitRange(startValue, endValue string, total int64) (byteRange, error) {
	start, err := strconv.ParseInt(startValue, 10, 64)
	if err != nil || start < 0 || start >= total {
		return byteRange{}, fmt.Errorf("range not satisfiable")
	}
	end := total - 1
	if endValue != "" {
		end, err = strconv.ParseInt(endValue, 10, 64)
		if err != nil || end < start {
			return byteRange{}, fmt.Errorf("invalid range")
		}
		if end >= total {
			end = total - 1
		}
	}
	return byteRange{start: start, end: end}, nil
}

func (h *ChannelHandler) ContentDownload(c *gin.Context) {
	start, err := h.manager.BeginDelivery(c.Param("id"), c.GetHeader(seatTokenHeader), c.GetHeader(claimTokenHeader), time.Now())
	if err != nil {
		channelDeliveryError(c, err)
		return
	}
	lease := start.Lease
	stopRequestCancel := context.AfterFunc(c.Request.Context(), lease.Cancel)
	defer stopRequestCancel()
	prepared, err := h.prepareChannelResponse(lease, start.Slug)
	if err != nil {
		_, _, _ = h.manager.FinishDelivery(lease, 0, false, time.Now())
		channelDeliveryError(c, err)
		return
	}
	defer func() {
		if err := prepared.close(); err != nil {
			log.Printf("[WARN] channel response temporary cleanup failed; retaining reservation: %v", err)
			return
		}
		h.manager.ReleaseDeliverySpace(lease)
	}()
	if lease.Context().Err() != nil {
		_, _, _ = h.manager.FinishDelivery(lease, prepared.response.Size, false, time.Now())
		channelDeliveryError(c, channels.ErrContentClosed)
		return
	}
	total := prepared.response.Size
	byteRange, partial, err := requestedChannelRange(c, prepared.plan, total)
	if err != nil {
		writeUnsatisfiableChannelRange(c, total)
		_, _, _ = h.manager.FinishDelivery(lease, total, false, time.Now())
		return
	}
	if err := h.manager.SetDeliveryRange(lease, byteRange.start); err != nil {
		_, _, _ = h.manager.FinishDelivery(lease, total, false, time.Now())
		channelDeliveryError(c, err)
		return
	}

	length := byteRange.end - byteRange.start + 1
	controller := writeChannelDownloadHeaders(c, prepared.plan, byteRange, total, length, partial)
	complete, streamErr := streamChannelResponse(h, c, lease, prepared, byteRange.start, length, controller)
	if !complete && lease.Context().Err() != nil {
		streamErr = lease.Context().Err()
	}
	_ = controller.SetWriteDeadline(time.Time{})
	h.finishChannelDelivery(lease, prepared, total, complete, streamErr)
}

func requestedChannelRange(c *gin.Context, plan channelResponsePlan, total int64) (byteRange, bool, error) {
	full := byteRange{start: 0, end: total - 1}
	rangeHeader := c.GetHeader("Range")
	if rangeHeader == "" {
		return full, false, nil
	}
	if ifRange := c.GetHeader("If-Range"); ifRange != "" && ifRange != plan.etag {
		return full, false, nil
	}
	requested, err := parseChannelRange(rangeHeader, total)
	return requested, err == nil, err
}

func writeUnsatisfiableChannelRange(c *gin.Context, total int64) {
	c.Header("Content-Range", fmt.Sprintf("bytes */%d", total))
	c.Header("Accept-Ranges", "bytes")
	c.AbortWithStatusJSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "range_not_satisfiable"})
}

func writeChannelDownloadHeaders(c *gin.Context, plan channelResponsePlan, requested byteRange, total, length int64, partial bool) *http.ResponseController {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Accept-Ranges", "bytes")
	c.Header("ETag", plan.etag)
	c.Header("Content-Type", "multipart/form-data; boundary="+plan.boundary)
	c.Header("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.start, requested.end, total))
		c.Status(http.StatusPartialContent)
	} else {
		c.Status(http.StatusOK)
	}
	controller := http.NewResponseController(c.Writer)
	return controller
}

func streamChannelResponse(h *ChannelHandler, c *gin.Context, lease *channels.DeliveryLease, prepared *preparedChannelResponse, start, length int64, controller *http.ResponseController) (bool, error) {
	lease.SetInterrupt(func() { _ = controller.SetWriteDeadline(time.Now()) })
	reader := io.NewSectionReader(prepared.response.File, start, length)
	buffer := make([]byte, 64*1024)
	written := int64(0)
	var streamErr error
	for written < length {
		if err := lease.Context().Err(); err != nil {
			streamErr = err
			break
		}
		want := min(int64(len(buffer)), length-written)
		n, readErr := reader.Read(buffer[:int(want)])
		if n > 0 {
			chunkWritten, writeErr := writeChannelResponseChunk(h, c, lease, controller, buffer[:n])
			written += chunkWritten
			if writeErr != nil {
				streamErr = writeErr
				break
			}
		}
		if readErr != nil {
			if readErr == io.EOF && written == length {
				break
			}
			streamErr = readErr
			break
		}
	}
	return streamErr == nil && written == length, streamErr
}

func writeChannelResponseChunk(h *ChannelHandler, c *gin.Context, lease *channels.DeliveryLease, controller *http.ResponseController, buffer []byte) (int64, error) {
	written, err := c.Writer.Write(buffer)
	if written > 0 {
		if recordErr := h.manager.RecordDeliveryBytes(lease, int64(written), time.Now()); recordErr != nil {
			return int64(written), recordErr
		}
	}
	if err != nil {
		return int64(written), err
	}
	if written != len(buffer) {
		return int64(written), io.ErrShortWrite
	}
	if err := controller.Flush(); err != nil {
		return int64(written), err
	}
	return int64(written), nil
}

func (h *ChannelHandler) finishChannelDelivery(lease *channels.DeliveryLease, prepared *preparedChannelResponse, total int64, complete bool, streamErr error) {
	release, delivered, finishErr := h.manager.FinishDelivery(lease, total, complete, time.Now())
	if finishErr != nil && !errors.Is(finishErr, channels.ErrContentClosed) {
		log.Printf("[WARN] finish channel delivery: %v", finishErr)
	}
	if delivered {
		if err := h.deleteReleasedContent(release); err != nil {
			log.Printf("[WARN] delete delivered channel content: %v", err)
		}
		return
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		log.Printf("[WARN] channel response interrupted: %v", streamErr)
	}
}

func channelDeliveryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, channels.ErrChannelNotFound), errors.Is(err, storage.ErrNotFound):
		channelContentError(c, http.StatusNotFound, channels.ErrChannelNotFound)
	case errors.Is(err, channels.ErrUnauthorized):
		channelContentError(c, http.StatusForbidden, channels.ErrUnauthorized)
	case errors.Is(err, channels.ErrDeliveryComplete), errors.Is(err, channels.ErrSeatKicked):
		channelContentError(c, http.StatusGone, err)
	case errors.Is(err, channels.ErrJoinClosed), errors.Is(err, channels.ErrContentClosed):
		channelContentError(c, http.StatusGone, channels.ErrContentClosed)
	case errors.Is(err, channels.ErrContentBusy), errors.Is(err, channels.ErrDeliveryBusy), errors.Is(err, channels.ErrDeliveryGap), errors.Is(err, channels.ErrDeliveryModeConflict), errors.Is(err, channels.ErrContentConflict):
		channelContentError(c, http.StatusConflict, err)
	case errors.Is(err, channels.ErrDeliveryNotReady):
		channelContentError(c, http.StatusConflict, err)
	case errors.Is(err, channels.ErrContentNotReady), errors.Is(err, channels.ErrCapacity), errors.Is(err, storage.ErrSlugReserved):
		channelContentError(c, http.StatusServiceUnavailable, err)
	default:
		channelContentError(c, http.StatusInternalServerError, errors.New("channel_delivery_failed"))
	}
}
