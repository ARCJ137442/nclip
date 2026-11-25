package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/channels"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/storage"
)

type channelAttachment struct {
	etag        string
	filename    string
	contentType string
	size        int64
}

func planChannelAttachment(paste *models.Paste, channelID string) (channelAttachment, error) {
	manifest := manifestFor(paste)
	plan := channelAttachment{size: paste.Size, contentType: paste.ContentType}
	switch manifest.Encoding {
	case "zip-v1":
		plan.filename = channelID + ".zip"
		plan.contentType = "application/zip"
	case "raw":
		if len(manifest.Items) != 1 || len(manifest.Items[0].Parts) != 1 {
			return channelAttachment{}, fmt.Errorf("invalid raw channel manifest")
		}
		item := manifest.Items[0]
		part := item.Parts[0]
		plan.contentType = part.MIME
		if item.Kind == "file" {
			plan.filename = cleanShareName(item.Name)
		} else {
			switch part.MIME {
			case "text/plain":
				plan.filename = channelID + ".txt"
			case "text/html":
				plan.filename = channelID + ".html"
			case "image/png":
				plan.filename = channelID + ".png"
			default:
				plan.filename = channelID + ".bin"
			}
		}
	default:
		return channelAttachment{}, fmt.Errorf("unsupported channel attachment encoding")
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%s", paste.ID, manifest.Encoding, paste.Size, plan.contentType)
	plan.etag = `"` + hex.EncodeToString(hash.Sum(nil)[:16]) + `"`
	return plan, nil
}

func (h *ChannelHandler) IssueContentTicket(c *gin.Context) {
	ticket, err := h.manager.IssueDeliveryTicket(c.Param("id"), c.GetHeader(seatTokenHeader), c.GetHeader(claimTokenHeader), time.Now())
	if err != nil {
		channelDeliveryError(c, err)
		return
	}
	downloadURL := "/api/v1/channels/" + c.Param("id") + "/content/download/" + ticket
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.JSON(http.StatusCreated, gin.H{"download_url": downloadURL})
}

func (h *ChannelHandler) ContentTicketDownload(c *gin.Context) {
	start, err := h.manager.BeginTicketDelivery(c.Param("id"), c.Param("ticket"), time.Now())
	if err != nil {
		channelDeliveryError(c, err)
		return
	}
	lease := start.Lease
	stopRequestCancel := context.AfterFunc(c.Request.Context(), lease.Cancel)
	defer stopRequestCancel()

	paste, err := h.shares.service.GetPaste(start.Slug)
	if err != nil || !paste.IsEphemeral() || paste.EphemeralChannelID != lease.ChannelID || paste.Size != start.ContentBytes {
		_, _, _ = h.manager.FinishDelivery(lease, start.ContentBytes, false, time.Now())
		if err == nil {
			err = storage.ErrNotFound
		}
		channelDeliveryError(c, err)
		return
	}
	plan, err := planChannelAttachment(paste, lease.ChannelID)
	if err != nil {
		_, _, _ = h.manager.FinishDelivery(lease, start.ContentBytes, false, time.Now())
		channelDeliveryError(c, err)
		return
	}
	byteRange, partial, err := requestedAttachmentRange(c, plan.etag, plan.size)
	if err != nil {
		writeUnsatisfiableChannelRange(c, plan.size)
		_, _, _ = h.manager.FinishDelivery(lease, plan.size, false, time.Now())
		return
	}
	if err := h.manager.SetDeliveryRange(lease, byteRange.start); err != nil {
		_, _, _ = h.manager.FinishDelivery(lease, plan.size, false, time.Now())
		channelDeliveryError(c, err)
		return
	}
	length := byteRange.end - byteRange.start + 1
	controller := writeAttachmentHeaders(c, plan, byteRange, length, partial)
	var complete bool
	var streamErr error
	err = h.shares.service.ReadPasteStream(start.Slug, false, func(current *models.Paste, source io.Reader, size int64) error {
		if current.ID != paste.ID || !current.IsEphemeral() || current.EphemeralChannelID != lease.ChannelID || size != plan.size {
			return storage.ErrNotFound
		}
		if closer, ok := source.(io.Closer); ok {
			lease.SetSource(closer)
		}
		if err := seekChannelRange(lease.Context(), source, byteRange.start); err != nil {
			return err
		}
		complete, streamErr = streamChannelReader(h, c, lease, contextReader{ctx: lease.Context(), source: source}, length, controller)
		return streamErr
	})
	if err != nil {
		complete = false
		if streamErr == nil {
			streamErr = err
		}
	}
	if !complete && lease.Context().Err() != nil {
		streamErr = lease.Context().Err()
	}
	_ = controller.SetWriteDeadline(time.Time{})
	h.finishTicketDelivery(lease, plan.size, complete, streamErr)
}

func requestedAttachmentRange(c *gin.Context, etag string, total int64) (byteRange, bool, error) {
	full := byteRange{start: 0, end: total - 1}
	if rangeHeader := c.GetHeader("Range"); rangeHeader != "" {
		if ifRange := c.GetHeader("If-Range"); ifRange != "" && ifRange != etag {
			return full, false, nil
		}
		requested, err := parseChannelRange(rangeHeader, total)
		return requested, err == nil, err
	}
	return full, false, nil
}

func writeAttachmentHeaders(c *gin.Context, plan channelAttachment, requested byteRange, length int64, partial bool) *http.ResponseController {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Accept-Ranges", "bytes")
	c.Header("ETag", plan.etag)
	c.Header("Content-Type", plan.contentType)
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": plan.filename}))
	c.Header("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.start, requested.end, plan.size))
		c.Status(http.StatusPartialContent)
	} else {
		c.Status(http.StatusOK)
	}
	return http.NewResponseController(c.Writer)
}

func seekChannelRange(ctx context.Context, source io.Reader, start int64) error {
	if start == 0 {
		return nil
	}
	if seeker, ok := source.(io.Seeker); ok {
		if _, err := seeker.Seek(start, io.SeekStart); err == nil {
			return nil
		}
	}
	_, err := io.CopyN(io.Discard, contextReader{ctx: ctx, source: source}, start)
	return err
}

func streamChannelReader(h *ChannelHandler, c *gin.Context, lease *channels.DeliveryLease, reader io.Reader, length int64, controller *http.ResponseController) (bool, error) {
	lease.SetInterrupt(func() { _ = controller.SetWriteDeadline(time.Now()) })
	buffer := make([]byte, 64*1024)
	written := int64(0)
	for written < length {
		if err := lease.Context().Err(); err != nil {
			return false, err
		}
		want := min(int64(len(buffer)), length-written)
		n, readErr := reader.Read(buffer[:int(want)])
		if n > 0 {
			chunkWritten, writeErr := writeChannelResponseChunk(h, c, lease, controller, buffer[:n])
			written += chunkWritten
			if writeErr != nil {
				return false, writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF && written == length {
				break
			}
			if readErr == io.EOF {
				readErr = io.ErrUnexpectedEOF
			}
			return false, readErr
		}
	}
	return written == length, nil
}

func (h *ChannelHandler) finishTicketDelivery(lease *channels.DeliveryLease, total int64, complete bool, streamErr error) {
	release, delivered, finishErr := h.manager.FinishDelivery(lease, total, complete, time.Now())
	if finishErr != nil && !errors.Is(finishErr, channels.ErrContentClosed) {
		log.Printf("[WARN] finish ticket delivery: %v", finishErr)
	}
	if delivered {
		if err := h.deleteReleasedContent(release); err != nil {
			log.Printf("[WARN] delete ticket-delivered content: %v", err)
		}
		return
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		log.Printf("[WARN] ticket response interrupted: %v", streamErr)
	}
}
