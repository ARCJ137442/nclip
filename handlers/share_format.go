package handlers

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/johnwmail/nclip/internal/spool"
	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/utils"
)

const maxShareItems = 32
const maxManifestBytes = 64 * 1024
const maxPreviewRunes = 24

var (
	secretMarkerPattern = regexp.MustCompile(`(?i)(sk-|ghp_|github_pat_|AKIA|-----BEGIN)`)
	longBase64Pattern   = regexp.MustCompile(`[A-Za-z0-9+/=_-]{24,}`)
)

func validPartKey(key string) bool {
	if len(key) == 0 || len(key) > 40 {
		return false
	}
	for _, ch := range key {
		lower := ch >= 'a' && ch <= 'z'
		digit := ch >= '0' && ch <= '9'
		if !lower && !digit && ch != '-' {
			return false
		}
	}
	return true
}

func cleanShareName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "" || name == "/" {
		return "content"
	}
	if len(name) > 200 {
		name = string([]rune(name)[:min(len([]rune(name)), 60)])
	}
	return name
}

func isClipboardFormat(mt string) bool {
	return mt == "text/plain" || mt == "text/html" || mt == "image/png"
}

func normalizeSharePart(p *models.SharePart, kind string, seen, formats map[string]bool) error {
	if !validPartKey(p.Key) || p.Key == "manifest" || p.Key == "archive" || seen[p.Key] {
		return fmt.Errorf("invalid_manifest")
	}
	seen[p.Key] = true
	mt, _, err := mime.ParseMediaType(p.MIME)
	if err != nil || formats[mt] {
		return fmt.Errorf("invalid_manifest")
	}
	formats[mt] = true
	p.MIME = mt
	if kind == "clipboard" && !isClipboardFormat(mt) {
		return fmt.Errorf("unsupported_format")
	}
	return nil
}

func normalizeShareItem(item *models.ShareItem, index int, seen map[string]bool) error {
	item.Name = cleanShareName(item.Name)
	if item.Kind != "clipboard" && item.Kind != "file" {
		return fmt.Errorf("invalid_manifest")
	}
	if len(item.Parts) == 0 || len(item.Parts) > 3 || item.Kind == "file" && len(item.Parts) != 1 {
		return fmt.Errorf("invalid_manifest")
	}
	formats := map[string]bool{}
	for j := range item.Parts {
		p := &item.Parts[j]
		if err := normalizeSharePart(p, item.Kind, seen, formats); err != nil {
			return err
		}
		entryName := item.Name
		if item.Kind == "clipboard" {
			entryName = "clipboard" + utils.ExtensionByMime(p.MIME)
		}
		p.Entry = fmt.Sprintf("items/%02d/%02d-%s", index, j, entryName)
	}
	return nil
}

func validateManifest(m *models.ShareManifest) error {
	if m.Version != 1 || len(m.Items) == 0 || len(m.Items) > maxShareItems {
		return fmt.Errorf("invalid_manifest")
	}
	seen := map[string]bool{}
	for i := range m.Items {
		if err := normalizeShareItem(&m.Items[i], i, seen); err != nil {
			return err
		}
	}
	return nil
}

func validateShareFiles(m *models.ShareManifest, parts map[string]*spool.File) error {
	used := 0
	for i := range m.Items {
		for j := range m.Items[i].Parts {
			part := &m.Items[i].Parts[j]
			file, ok := parts[part.Key]
			if !ok || file == nil || file.Size == 0 {
				return fmt.Errorf("empty_or_missing_part")
			}
			if m.Items[i].Kind == "clipboard" && !validClipboardFile(*part, file) {
				return fmt.Errorf("invalid_clipboard_data")
			}
			part.Size = file.Size
			used++
		}
	}
	if used != len(parts) {
		return fmt.Errorf("unexpected_part")
	}
	return nil
}

// buildSharePreviews adds a short server-derived hint from plain clipboard text.
// It deliberately omits likely credentials because metadata is visible to anyone
// holding a share link. Only the bounded spool prefix is inspected.
func buildSharePreviews(m *models.ShareManifest, parts map[string]*spool.File) {
	for i := range m.Items {
		item := &m.Items[i]
		item.Preview = ""
		if item.Kind != "clipboard" {
			continue
		}
		for _, part := range item.Parts {
			if part.MIME != "text/plain" {
				continue
			}
			if file := parts[part.Key]; file != nil {
				item.Preview = safePreview(file.Head)
			}
			break
		}
	}
}

func safePreview(prefix []byte) string {
	text := strings.ToValidUTF8(string(prefix), "")
	if secretMarkerPattern.MatchString(text) || hasLongBase64Token(text) {
		return ""
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, text)
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > maxPreviewRunes {
		runes = runes[:maxPreviewRunes]
	}
	return string(runes)
}

func hasLongBase64Token(text string) bool {
	for _, token := range longBase64Pattern.FindAllString(text, -1) {
		letters, digits := false, false
		for _, r := range token {
			letters = letters || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
			digits = digits || r >= '0' && r <= '9'
		}
		if letters && digits {
			return true
		}
	}
	return false
}

func validClipboardFile(part models.SharePart, file *spool.File) bool {
	if part.MIME == "image/png" {
		return http.DetectContentType(file.Head) == "image/png"
	}
	if !strings.HasPrefix(part.MIME, "text/") {
		return false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	defer func() { _, _ = file.Seek(0, io.SeekStart) }()
	return validUTF8(file) == nil
}

var errInvalidUTF8 = fmt.Errorf("invalid_utf8")

// validUTF8 validates a stream in chunks while carrying at most the three
// bytes of an incomplete rune between reads.
func validUTF8(r io.Reader) error {
	buf := make([]byte, 64*1024)
	carry := make([]byte, 0, utf8.UTFMax-1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, 0, len(carry)+n)
			data = append(data, carry...)
			data = append(data, buf[:n]...)
			cut := len(data)
			start := cut - 1
			for start >= 0 && cut-start < utf8.UTFMax && !utf8.RuneStart(data[start]) {
				start--
			}
			if start >= 0 && !utf8.FullRune(data[start:cut]) {
				cut = start
			}
			if !utf8.Valid(data[:cut]) {
				return errInvalidUTF8
			}
			carry = append(carry[:0], data[cut:]...)
		}
		if err == io.EOF {
			if len(carry) != 0 {
				return errInvalidUTF8
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func manifestFor(p *models.Paste) models.ShareManifest {
	if p.Share != nil {
		return *p.Share
	}
	kind := "file"
	contentType, _, err := mime.ParseMediaType(p.ContentType)
	if err != nil {
		contentType = "application/octet-stream"
	}
	if strings.HasPrefix(contentType, "text/") || contentType == "image/png" {
		kind = "clipboard"
	}
	return models.ShareManifest{Version: 1, Encoding: "raw", Items: []models.ShareItem{{Kind: kind, Name: p.ID + utils.ExtensionByMime(contentType), Parts: []models.SharePart{{Key: "original", MIME: contentType, Size: p.Size}}}}}
}
