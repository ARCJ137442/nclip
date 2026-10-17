package handlers

import (
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/models"
)

type pasteLister interface {
	ListPastes() ([]models.Paste, error)
}

// Shares returns paginated metadata for managers, never clipboard payloads.
func (h *AccountHandler) Shares(c *gin.Context) {
	lister, ok := h.pastes.(pasteLister)
	if !ok {
		c.JSON(501, gin.H{"error": "listing_unavailable"})
		return
	}
	pastes, err := lister.ListPastes()
	if err != nil {
		c.JSON(500, gin.H{"error": "listing_failed"})
		return
	}
	query := strings.ToLower(strings.TrimSpace(c.Query("q")))
	filtered := make([]models.Paste, 0, len(pastes))
	var totalBytes int64
	for _, paste := range pastes {
		if matchesShare(paste, query) {
			filtered = append(filtered, paste)
			totalBytes += paste.Size
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].ID < filtered[j].ID
		}
		return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
	})
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	const perPage = 20
	total := len(filtered)
	pages := max(1, (total+perPage-1)/perPage)
	page = min(page, pages)
	start := min((page-1)*perPage, total)
	end := min(start+perPage, total)
	c.JSON(200, gin.H{"items": filtered[start:end], "page": page, "pages": pages, "total": total, "total_bytes": totalBytes})
}

func matchesShare(paste models.Paste, query string) bool {
	if query == "" || strings.Contains(strings.ToLower(paste.ID+" "+paste.ContentType), query) {
		return true
	}
	if paste.Share != nil {
		for _, item := range paste.Share.Items {
			if strings.Contains(strings.ToLower(item.Name), query) {
				return true
			}
		}
	}
	return false
}
