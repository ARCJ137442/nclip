package handlers

import "github.com/gin-gonic/gin"

const canonicalReasonContextKey = "nclip.canonical-error-reason"

type canonicalReason struct {
	status int
	code   string
	reason string
}

func setCanonicalErrorReason(c *gin.Context, status int, code, reason string) {
	c.Set(canonicalReasonContextKey, canonicalReason{status: status, code: code, reason: reason})
}

// CanonicalErrorReason carries only an explicitly marked server-side reason
// through error canonicalization, bound to the original status and machine code.
func CanonicalErrorReason(c *gin.Context, status int, code string) string {
	value, _ := c.Get(canonicalReasonContextKey)
	reason, ok := value.(canonicalReason)
	if !ok || reason.status != status || reason.code != code {
		return ""
	}
	return reason.reason
}
