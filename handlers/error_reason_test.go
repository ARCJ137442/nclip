package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCanonicalReasonRequiresMarkedStatusAndCode(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if CanonicalErrorReason(c, 410, "join_closed") != "" {
		t.Fatal("unmarked error gained reason")
	}
	setCanonicalErrorReason(c, 410, "join_closed", "sender_rejected")
	for _, test := range []struct {
		status     int
		code, want string
	}{
		{410, "join_closed", "sender_rejected"}, {403, "join_closed", ""}, {410, "other", ""},
	} {
		if got := CanonicalErrorReason(c, test.status, test.code); got != test.want {
			t.Fatalf("status=%d code=%s reason=%s", test.status, test.code, got)
		}
	}
}
