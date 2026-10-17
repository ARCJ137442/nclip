package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/johnwmail/nclip/models"
	"github.com/johnwmail/nclip/utils"
)

const maxShareItems = 32
const maxManifestBytes = 64 * 1024

func validPartKey(key string) bool {
	if len(key) == 0 || len(key) > 40 {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
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

func validClipboardBytes(part models.SharePart, data []byte) bool {
	if strings.HasPrefix(part.MIME, "text/") {
		return utf8.Valid(data)
	}
	if part.MIME == "image/png" {
		return http.DetectContentType(data) == "image/png"
	}
	return false
}

func validateSharePayload(m *models.ShareManifest, parts map[string][]byte) error {
	used := 0
	for i := range m.Items {
		for j := range m.Items[i].Parts {
			p := &m.Items[i].Parts[j]
			data, ok := parts[p.Key]
			if !ok || len(data) == 0 {
				return fmt.Errorf("empty_or_missing_part")
			}
			if m.Items[i].Kind == "clipboard" && !validClipboardBytes(*p, data) {
				return fmt.Errorf("invalid_clipboard_data")
			}
			p.Size = int64(len(data))
			used++
		}
	}
	if used != len(parts) {
		return fmt.Errorf("unexpected_part")
	}
	return nil
}

func writeZipEntry(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func encodeStoredShare(m *models.ShareManifest, parts map[string][]byte) ([]byte, string, error) {
	if len(m.Items) == 1 && len(m.Items[0].Parts) == 1 {
		m.Encoding = "raw"
		p := &m.Items[0].Parts[0]
		p.Entry = ""
		return parts[p.Key], p.MIME, nil
	}
	m.Encoding = "zip-v1"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, item := range m.Items {
		for _, p := range item.Parts {
			if err := writeZipEntry(zw, p.Entry, parts[p.Key]); err != nil {
				return nil, "", err
			}
		}
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	if err := writeZipEntry(zw, "manifest.json", manifest); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "application/zip", nil
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

func readArchivePart(f *zip.File, size, max int64) ([]byte, error) {
	if f == nil || size < 0 || size > max {
		return nil, fmt.Errorf("invalid_archive")
	}
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(r, size+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) != size {
		return nil, fmt.Errorf("invalid_archive")
	}
	return data, nil
}

func unpackArchive(m models.ShareManifest, content []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return nil, err
	}
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	parts := map[string][]byte{}
	for _, item := range m.Items {
		for _, p := range item.Parts {
			data, err := readArchivePart(entries[p.Entry], p.Size, int64(len(content)))
			if err != nil {
				return nil, err
			}
			parts[p.Key] = data
		}
	}
	return parts, nil
}

func unpackShare(p *models.Paste, content []byte) (models.ShareManifest, map[string][]byte, error) {
	m := manifestFor(p)
	if int64(len(content)) != p.Size {
		return m, nil, fmt.Errorf("size_mismatch")
	}
	if m.Encoding == "raw" {
		if len(m.Items) != 1 || len(m.Items[0].Parts) != 1 {
			return m, nil, fmt.Errorf("invalid_archive")
		}
		return m, map[string][]byte{m.Items[0].Parts[0].Key: content}, nil
	}
	if m.Encoding != "zip-v1" {
		return m, nil, fmt.Errorf("unsupported_version")
	}
	parts, err := unpackArchive(m, content)
	return m, parts, err
}
