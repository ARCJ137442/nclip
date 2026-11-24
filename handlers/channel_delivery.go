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

func (p *preparedChannelResponse) close() {
	if p == nil {
		return
	}
	if p.source != nil {
		_ = p.source.Remove()
	}
	if p.response != nil {
		_ = p.response.Remove()
	}
	if p.directory != "" {
		_ = os.RemoveAll(p.directory)
	}
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
	reserved := true
	defer func() {
		if reserved {
			h.manager.ReleaseDeliverySpace(lease)
		}
	}()
	directory, err := h.shares.tempDir()
	if err != nil {
		return nil, err
	}
	prepared := &preparedChannelResponse{directory: directory, plan: plan}
	cleanup := true
	defer func() {
		if cleanup {
			prepared.close()
		}
	}()
	err = h.shares.service.ReadPasteStream(slug, false, func(_ *models.Paste, source io.Reader, size int64) error {
		if closer, ok := source.(io.Closer); ok {
			lease.SetSource(closer)
		}
		wrapped := contextReader{ctx: lease.Context(), source: source}
		file, writeErr := spool.Write(directory, "nclip-channel-read-source-*", wrapped, size, 0)
		if writeErr != nil {
			return writeErr
		}
		prepared.source = file
		if file.Size != size {
			return fmt.Errorf("size_mismatch")
		}
		stored, openErr := openStoredShareContext(lease.Context(), &plan.manifest, file)
		if openErr != nil {
			return openErr
		}
		response, prepareErr := prepareShareResponseContext(lease.Context(), directory, plan.boundary, paste, plan.manifestBytes, plan.manifest, stored)
		if prepareErr != nil {
			return prepareErr
		}
		prepared.response = response
		if response.Size != plan.size {
			return fmt.Errorf("multipart_size_mismatch")
		}
		return h.manager.MaterializeDeliverySpace(lease, reservation)
	})
	if err != nil {
		return nil, err
	}
	lease.SetSource(prepared.response.File)
	cleanup = false
	reserved = false
	return prepared, nil
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
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return byteRange{}, fmt.Errorf("invalid range")
		}
		if suffix > total {
			suffix = total
		}
		return byteRange{start: total - suffix, end: total - 1}, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= total {
		return byteRange{}, fmt.Errorf("range not satisfiable")
	}
	end := total - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
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
	if lease.Context().Err() != nil {
		_, _, _ = h.manager.FinishDelivery(lease, prepared.response.Size, false, time.Now())
		channelDeliveryError(c, channels.ErrContentClosed)
		prepared.close()
		h.manager.ReleaseDeliverySpace(lease)
		return
	}
	defer func() {
		prepared.close()
		h.manager.ReleaseDeliverySpace(lease)
	}()

	total := prepared.response.Size
	byteRange := byteRange{start: 0, end: total - 1}
	partial := false
	if rangeHeader := c.GetHeader("Range"); rangeHeader != "" {
		ifRange := c.GetHeader("If-Range")
		if ifRange == "" || ifRange == prepared.plan.etag {
			byteRange, err = parseChannelRange(rangeHeader, total)
			if err != nil {
				c.Header("Content-Range", fmt.Sprintf("bytes */%d", total))
				c.Header("Accept-Ranges", "bytes")
				c.AbortWithStatusJSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "range_not_satisfiable"})
				_, _, _ = h.manager.FinishDelivery(lease, total, false, time.Now())
				return
			}
			partial = true
		}
	}
	if err := h.manager.SetDeliveryRange(lease, byteRange.start); err != nil {
		_, _, _ = h.manager.FinishDelivery(lease, total, false, time.Now())
		channelDeliveryError(c, err)
		return
	}

	length := byteRange.end - byteRange.start + 1
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Accept-Ranges", "bytes")
	c.Header("ETag", prepared.plan.etag)
	c.Header("Content-Type", "multipart/form-data; boundary="+prepared.plan.boundary)
	c.Header("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.start, byteRange.end, total))
		c.Status(http.StatusPartialContent)
	} else {
		c.Status(http.StatusOK)
	}
	controller := http.NewResponseController(c.Writer)
	lease.SetInterrupt(func() { _ = controller.SetWriteDeadline(time.Now()) })
	reader := io.NewSectionReader(prepared.response.File, byteRange.start, length)
	buffer := make([]byte, 64*1024)
	written := int64(0)
	var streamErr error
	for written < length {
		if lease.Context().Err() != nil {
			streamErr = lease.Context().Err()
			break
		}
		want := int64(len(buffer))
		if remain := length - written; remain < want {
			want = remain
		}
		n, readErr := reader.Read(buffer[:int(want)])
		if n > 0 {
			nw, writeErr := c.Writer.Write(buffer[:n])
			if nw > 0 {
				written += int64(nw)
				if err := h.manager.RecordDeliveryBytes(lease, int64(nw), time.Now()); err != nil {
					streamErr = err
					break
				}
			}
			if writeErr != nil {
				streamErr = writeErr
				break
			}
			if nw != n {
				streamErr = io.ErrShortWrite
				break
			}
			if flushErr := controller.Flush(); flushErr != nil {
				streamErr = flushErr
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
	complete := streamErr == nil && written == length
	if !complete && lease.Context().Err() != nil {
		streamErr = lease.Context().Err()
	}
	_ = controller.SetWriteDeadline(time.Time{})
	release, delivered, finishErr := h.manager.FinishDelivery(lease, total, complete, time.Now())
	if finishErr != nil && !errors.Is(finishErr, channels.ErrContentClosed) {
		log.Printf("[WARN] finish channel delivery: %v", finishErr)
	}
	if delivered {
		prepared.close()
		h.manager.ReleaseDeliverySpace(lease)
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
	case errors.Is(err, channels.ErrJoinClosed), errors.Is(err, channels.ErrContentClosed), errors.Is(err, channels.ErrDeliveryComplete), errors.Is(err, channels.ErrSeatKicked):
		channelContentError(c, http.StatusGone, channels.ErrContentClosed)
	case errors.Is(err, channels.ErrContentBusy), errors.Is(err, channels.ErrDeliveryGap), errors.Is(err, channels.ErrContentConflict):
		channelContentError(c, http.StatusConflict, err)
	case errors.Is(err, channels.ErrDeliveryNotReady):
		channelContentError(c, http.StatusConflict, err)
	case errors.Is(err, channels.ErrContentNotReady), errors.Is(err, channels.ErrCapacity), errors.Is(err, storage.ErrSlugReserved):
		channelContentError(c, http.StatusServiceUnavailable, err)
	default:
		channelContentError(c, http.StatusInternalServerError, errors.New("channel_delivery_failed"))
	}
}
