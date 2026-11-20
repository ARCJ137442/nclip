package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/accounts"
	"github.com/johnwmail/nclip/internal/services"
	"github.com/johnwmail/nclip/storage"
)

func TestSharesUploadLimitAllowsMultipartFramingButCapsContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	store, err := storage.NewFilesystemStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfg := &config.Config{DataDir: dir, BufferSize: 4 * config.MiB, DefaultTTL: 24 * time.Hour}
	cfg.UploadLimit, err = config.OpenUploadLimit(dir, config.MiB, config.MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/api/v1/shares", NewSharesHandler(services.NewPasteService(store, cfg), cfg).Create)

	makeRequest := func(size int) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("manifest", `{"version":1,"items":[{"kind":"file","name":"large.bin","parts":[{"key":"file","mime":"application/octet-stream"}]}]}`); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("file", "large.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(bytes.Repeat([]byte("x"), size)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/shares", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	if response := makeRequest(int(config.MiB)); response.Code != http.StatusCreated {
		t.Fatalf("1 MiB content plus multipart framing returned %d: %s", response.Code, response.Body.String())
	}
	if response := makeRequest(int(config.MiB + 1)); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit content returned %d: %s", response.Code, response.Body.String())
	}
}

func TestShareUploadLimitIsPerUserAndServerBounded(t *testing.T) {
	limit, err := config.OpenUploadLimit(t.TempDir(), 2*config.MiB, config.MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	handler := &SharesHandler{config: &config.Config{BufferSize: 20 * config.MiB}, uploadLimit: limit}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	quota := int64(8 * config.MiB)
	context.Set(accountContext, accounts.User{UploadLimitBytes: &quota})
	handler.UIConfig(context)
	var payload struct {
		UploadMaxBytes int64 `json:"upload_max_bytes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.UploadMaxBytes != 2*config.MiB {
		t.Fatalf("edge user limit = %d, want global 2 MiB", payload.UploadMaxBytes)
	}

	response = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(response)
	context.Request = request
	context.Set(accountContext, accounts.User{})
	handler.UIConfig(context)
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.UploadMaxBytes != 20*config.MiB {
		t.Fatalf("unlimited user limit = %d, want server 20 MiB", payload.UploadMaxBytes)
	}
}

func TestUploadLimitUIConfigUsesRuntimeValue(t *testing.T) {
	limit, err := config.OpenUploadLimit(t.TempDir(), 5*config.MiB, config.MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limit.Set(12*config.MiB, 0); err != nil {
		t.Fatal(err)
	}
	handler := &SharesHandler{config: &config.Config{BufferSize: 20 * config.MiB}, uploadLimit: limit}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	handler.UIConfig(context)
	var payload struct {
		UploadMaxBytes int64 `json:"upload_max_bytes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || payload.UploadMaxBytes != 12*config.MiB || strings.Contains(response.Body.String(), "revision") {
		t.Fatalf("unexpected UI config response %d: %s", response.Code, response.Body.String())
	}
}

func TestShareUIConfigAppliesGlobalAndServerLimitsToAnonymousRequests(t *testing.T) {
	limit, err := config.OpenUploadLimit(t.TempDir(), 5*config.MiB, config.MaxUploadLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	handler := &SharesHandler{config: &config.Config{BufferSize: 2 * config.MiB}, uploadLimit: limit}
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil)
	handler.UIConfig(context)
	var payload struct {
		UploadMaxBytes int64 `json:"upload_max_bytes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.UploadMaxBytes != 2*config.MiB {
		t.Fatalf("anonymous share limit = %d, want server limit 2 MiB", payload.UploadMaxBytes)
	}
}
