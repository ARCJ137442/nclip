package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/internal/channels"
)

func TestChannelDeliveryErrorReturnsRetryableMachineCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "busy", err: channels.ErrDeliveryBusy, code: "delivery_busy"},
		{name: "range gap", err: channels.ErrDeliveryGap, code: "range_gap"},
		{name: "not ready", err: channels.ErrDeliveryNotReady, code: "delivery_not_ready"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)

			channelDeliveryError(context, test.err)

			var body struct {
				Error string `json:"error"`
			}
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusConflict, response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v: %s", err, response.Body.String())
			}
			if body.Error != test.code {
				t.Fatalf("error = %q, want %q", body.Error, test.code)
			}
		})
	}
}
