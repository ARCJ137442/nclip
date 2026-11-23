package handlers

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/johnwmail/nclip/internal/spool"
	"github.com/johnwmail/nclip/models"
)

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	n := copy(p, chunk)
	if n == len(chunk) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = chunk[n:]
	}
	return n, nil
}

func TestValidUTF8CarriesSplitRunesBetweenChunks(t *testing.T) {
	first := append(bytes.Repeat([]byte("a"), 64*1024-1), 0xe4)
	reader := &chunkReader{chunks: [][]byte{first, {0xb8, 0xad}}}
	if err := validUTF8(reader); err != nil {
		t.Fatalf("valid split rune rejected: %v", err)
	}
}

func TestValidUTF8RejectsIncompleteOrMalformedRunes(t *testing.T) {
	if err := validUTF8(bytes.NewReader([]byte{0xe4, 0xb8})); err == nil {
		t.Fatal("incomplete rune was accepted")
	}
	if err := validUTF8(bytes.NewReader([]byte{0xff})); err == nil {
		t.Fatal("malformed rune was accepted")
	}
}

func TestValidUTF8AcceptsPlainAndMultibyteContent(t *testing.T) {
	if err := validUTF8(bytes.NewBufferString("plain\n中文\n")); err != nil {
		t.Fatal(err)
	}
}

func TestSafePreviewNormalizesAndLimitsText(t *testing.T) {
	preview := safePreview([]byte("第一行\r\n第二行\t这里还有很多文字，超出部分不能显示"))
	if got := []rune(preview); len(got) > maxPreviewRunes || strings.ContainsAny(preview, "\r\n\t") {
		t.Fatalf("preview = %q (%d runes), want %d runes without line controls", preview, len(got), maxPreviewRunes)
	}
	if !strings.Contains(preview, " ") {
		t.Fatalf("preview did not fold line breaks: %q", preview)
	}
}

func TestSafePreviewOmitsCredentialLikeText(t *testing.T) {
	for _, secret := range []string{
		"s" + "k-live-secret-value",
		"ghp" + "_012345678901234567890123456789012345",
		"github_pat" + "_012345678901234567890123456789012345",
		"A" + "KIA1234567890123456",
		"-----BEGIN " + "PRIVATE KEY-----",
		"A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6",
	} {
		if got := safePreview([]byte(secret)); got != "" {
			t.Errorf("safePreview(%q) = %q, want omitted", secret, got)
		}
	}
}

func TestSafePreviewAllowsOrdinaryLongProse(t *testing.T) {
	if got := safePreview([]byte("这是普通说明文字，没有凭证")); got == "" {
		t.Fatal("ordinary prose should have a preview")
	}
}

func TestBuildSharePreviewsOnlyForSafeClipboardPlainText(t *testing.T) {
	dir := t.TempDir()
	makeFile := func(name, text string) *spool.File {
		file, err := spool.Write(dir, name, strings.NewReader(text), int64(len(text)), 512)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	plain := makeFile("plain-*", "对方会看到这个开头")
	secret := makeFile("secret-*", "s"+"k-secret-value-that-must-not-show")
	filePart := makeFile("file-*", "file content")
	defer func() {
		_ = plain.Remove()
		_ = secret.Remove()
		_ = filePart.Remove()
	}()

	manifest := models.ShareManifest{Items: []models.ShareItem{
		{Kind: "clipboard", Parts: []models.SharePart{{Key: "plain", MIME: "text/plain"}}},
		{Kind: "clipboard", Parts: []models.SharePart{{Key: "secret", MIME: "text/plain"}}},
		{Kind: "file", Name: "file.txt", Parts: []models.SharePart{{Key: "file", MIME: "text/plain"}}},
	}}
	buildSharePreviews(&manifest, map[string]*spool.File{"plain": plain, "secret": secret, "file": filePart})
	if manifest.Items[0].Preview != "对方会看到这个开头" {
		t.Fatalf("plain preview = %q", manifest.Items[0].Preview)
	}
	if manifest.Items[1].Preview != "" || manifest.Items[2].Preview != "" {
		t.Fatalf("credential/file preview leaked: %+v", manifest.Items)
	}
}
