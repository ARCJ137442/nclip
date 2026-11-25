package handlers

import (
	"log"

	"github.com/johnwmail/nclip/internal/channels"
)

// Only channel response copies use this prefix; ordinary share temporary
// directories are not owned by the channel writer or its startup sweep.
const channelResponseTempPrefix = ".nclip-channel-response-"

// The cleanup queue owns both the directory and its reservation after the
// request exits. Failed deletion must retain both until a later sweep succeeds.
func (h *ChannelHandler) releasePreparedResponse(lease *channels.DeliveryLease, prepared *preparedChannelResponse) error {
	h.responseCleanupMu.Lock()
	defer h.responseCleanupMu.Unlock()
	return h.releasePreparedResponseLocked(lease, prepared)
}

func (h *ChannelHandler) releasePreparedResponseLocked(lease *channels.DeliveryLease, prepared *preparedChannelResponse) error {
	if h.pendingResponses == nil {
		h.pendingResponses = make(map[*channels.DeliveryLease]*preparedChannelResponse)
	}
	h.pendingResponses[lease] = prepared
	if err := prepared.close(); err != nil {
		return err
	}
	h.manager.ReleaseDeliverySpace(lease)
	delete(h.pendingResponses, lease)
	return nil
}

func (h *ChannelHandler) retryPreparedResponses() {
	h.responseCleanupMu.Lock()
	defer h.responseCleanupMu.Unlock()
	for lease, prepared := range h.pendingResponses {
		if err := h.releasePreparedResponseLocked(lease, prepared); err != nil {
			log.Printf("[WARN] channel response cleanup retry failed: %v", err)
		}
	}
}
