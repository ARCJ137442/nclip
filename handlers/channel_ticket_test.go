package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/models"
)

func TestAttachmentRangeUsesImmutableRepresentationETag(t *testing.T) {
	for _, test := range []struct {
		name, requested, ifRange string
		start, end               int64
		partial, invalid         bool
	}{
		{name: "full", end: 9},
		{name: "resume", requested: "bytes=4-", ifRange: `"version"`, start: 4, end: 9, partial: true},
		{name: "changed version restarts", requested: "bytes=4-", ifRange: `"old"`, end: 9},
		{name: "multipart rejected", requested: "bytes=0-1,4-5", invalid: true},
		{name: "past end", requested: "bytes=10-", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/", nil)
			c.Request.Header.Set("Range", test.requested)
			c.Request.Header.Set("If-Range", test.ifRange)
			got, partial, err := requestedAttachmentRange(c, `"version"`, 10)
			if (err != nil) != test.invalid {
				t.Fatalf("range error: %v", err)
			}
			if !test.invalid && (got.start != test.start || got.end != test.end || partial != test.partial) {
				t.Fatalf("range=%+v partial=%t", got, partial)
			}
		})
	}
}

func TestPlanChannelAttachmentRawFileAndZipArchive(t *testing.T) {
	raw := &models.Paste{
		ID:          "raw-id",
		Size:        12,
		ContentType: "text/plain",
		Share: &models.ShareManifest{Version: 1, Encoding: "raw", Items: []models.ShareItem{{
			Kind: "file", Name: "../../report.txt", Parts: []models.SharePart{{Key: "part1", MIME: "text/plain", Size: 12}},
		}}},
	}
	rawPlan, err := planChannelAttachment(raw, "channel-id")
	if err != nil {
		t.Fatal(err)
	}
	if rawPlan.filename != "report.txt" || rawPlan.contentType != "text/plain" || rawPlan.size != 12 || rawPlan.etag == "" {
		t.Fatalf("unsafe/incorrect raw attachment plan: %+v", rawPlan)
	}

	archive := &models.Paste{
		ID:          "zip-id",
		Size:        1024,
		ContentType: "application/zip",
		Share: &models.ShareManifest{Version: 1, Encoding: "zip-v1", Items: []models.ShareItem{{
			Kind: "file", Name: "one.txt", Parts: []models.SharePart{{Key: "part1", MIME: "text/plain", Size: 3, Entry: "one.txt"}},
		}, {
			Kind: "file", Name: "two.txt", Parts: []models.SharePart{{Key: "part2", MIME: "text/plain", Size: 3, Entry: "two.txt"}},
		}}},
	}
	zipPlan, err := planChannelAttachment(archive, "channel-id")
	if err != nil {
		t.Fatal(err)
	}
	if zipPlan.filename != "channel-id.zip" || zipPlan.contentType != "application/zip" || zipPlan.size != 1024 || zipPlan.etag == "" {
		t.Fatalf("incorrect zip attachment plan: %+v", zipPlan)
	}
}
