package handlers

import (
	"bytes"
	"io"
	"testing"
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
	reader := &chunkReader{chunks: [][]byte{first, []byte{0xb8, 0xad}}}
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
