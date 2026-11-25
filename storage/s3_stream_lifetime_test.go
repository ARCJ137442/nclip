package storage

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type streamLifetimeTransport struct{ ctx context.Context }

func (r *streamLifetimeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.ctx = req.Context()
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Length": []string{"4"}}, Body: io.NopCloser(strings.NewReader("data")), Request: req}, nil
}
func TestRegressionS3StreamOutlivesTenSeconds(t *testing.T) {
	tr := &streamLifetimeTransport{}
	client := s3.New(s3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), BaseEndpoint: aws.String("http://s3.example.test"), UsePathStyle: true, HTTPClient: &http.Client{Transport: tr}})
	store := &S3Store{bucket: "test", client: client}
	body, _, err := store.OpenContent("abc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = body.Close() })
	first := make([]byte, 1)
	if _, err := body.Read(first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.ctx.Done():
		t.Fatalf("source GET context expires during slow downstream stream: %v", tr.ctx.Err())
	case <-time.After(11 * time.Second):
	}
	started := time.Now()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("closing source did not cancel S3 context")
	}
	if time.Since(started) > time.Second {
		t.Fatal("source cancellation was slow")
	}
}
