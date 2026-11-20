package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
)

type uploadSettingsResponse struct {
	UploadLimitMiB int64  `json:"upload_limit_mib"`
	CeilingMiB     int64  `json:"ceiling_mib"`
	Revision       uint64 `json:"revision"`
}

func uploadSettings(limit *config.UploadLimit) uploadSettingsResponse {
	value, ceiling, revision := limit.Snapshot()
	return uploadSettingsResponse{UploadLimitMiB: value / config.MiB, CeilingMiB: ceiling / config.MiB, Revision: revision}
}

// GetUploadSettings returns the persistent upload quota to an authorized manager.
func GetUploadSettings(limit *config.UploadLimit) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if limit == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "settings_unavailable"})
			return
		}
		c.JSON(http.StatusOK, uploadSettings(limit))
	}
}

// SaveUploadSettings validates and persists a manager's upload quota update.
func SaveUploadSettings(limit *config.UploadLimit) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "settings_unavailable"})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var input struct {
			UploadLimitMiB int64  `json:"upload_limit_mib"`
			Revision       uint64 `json:"revision"`
		}
		if c.ShouldBindJSON(&input) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
		if input.UploadLimitMiB < 1 || input.UploadLimitMiB > config.MaxUploadLimitBytes/config.MiB {
			c.JSON(http.StatusBadRequest, gin.H{"error": "upload_limit_out_of_range"})
			return
		}
		_, err := limit.Set(input.UploadLimitMiB*config.MiB, input.Revision)
		if err != nil {
			currentRevision := uploadSettings(limit).Revision
			status := http.StatusInternalServerError
			switch err.Error() {
			case "upload_limit_out_of_range":
				status = http.StatusBadRequest
			case "upload_limit_changed":
				status = http.StatusConflict
			}
			c.JSON(status, gin.H{"error": err.Error(), "revision": currentRevision})
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, uploadSettings(limit))
	}
}
