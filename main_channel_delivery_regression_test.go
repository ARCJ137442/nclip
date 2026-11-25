package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type failedFlushWriter struct {
	*httptest.ResponseRecorder
	failures int
}

func (w *failedFlushWriter) Flush() { w.failures++ }
func (w *failedFlushWriter) FlushError() error {
	w.failures++
	return errors.New("injected final network flush failure")
}

func issueRegressionTicket(t *testing.T, f channelDeliveryFixture) string {
	t.Helper()
	r := channelRequestWithCookies(f.router, http.MethodPost, f.contentPath+"/ticket", nil, nil, map[string]string{"X-Nclip-Seat-Token": f.seatToken, "X-Nclip-Claim-Token": f.claimToken, "Origin": "https://clip.test"})
	var ticket struct {
		URL string `json:"download_url"`
	}
	if r.Code != http.StatusCreated || json.Unmarshal(r.Body.Bytes(), &ticket) != nil {
		t.Fatalf("ticket: %d", r.Code)
	}
	return ticket.URL
}

func TestRegressionNativeOpenFailureReturnsError(t *testing.T) {
	router, store, dir := shareTestRouterWithStore(t, 8<<20, false)
	f := createAdmittedChannelFixture(t, router)
	f.router = router
	f.store = store
	f.contentPath = "/api/v1/channels/" + f.channelID + "/content"
	uploadChannelFixtureContent(t, f, []byte("data"))
	url := issueRegressionTicket(t, f)
	all, err := store.ListEphemeralPastes()
	if err != nil || len(all) != 1 {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, all[0].ID)); err != nil {
		t.Fatal(err)
	}
	r := channelRequestWithCookies(router, http.MethodGet, url, nil, nil, nil)
	if r.Code == http.StatusOK || r.Code == http.StatusPartialContent {
		t.Fatalf("source open failed but status=%d length=%s type=%s body=%q", r.Code, r.Header().Get("Content-Length"), r.Header().Get("Content-Type"), r.Body.String())
	}
	if r.Header().Get("Content-Disposition") != "" || r.Header().Get("Content-Length") == "4" {
		t.Fatal("open failure kept attachment success headers")
	}
}

func TestRegressionNativeFlushFailureRetainsContent(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(map[bool]string{true: "native", false: "app"}[native], func(t *testing.T) { assertFlushFailureRetainsContent(t, native) })
	}
}

func assertFlushFailureRetainsContent(t *testing.T, native bool) {
	t.Helper()
	f := newChannelDeliveryFixture(t, []byte("data"))
	url := f.contentPath
	if native {
		url = issueRegressionTicket(t, f)
	}
	w := &failedFlushWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	if !native {
		r.Header.Set("X-Nclip-Seat-Token", f.seatToken)
		r.Header.Set("X-Nclip-Claim-Token", f.claimToken)
	}
	f.router.ServeHTTP(w, r)
	if w.failures == 0 {
		t.Fatal("fixture did not reach flush")
	}
	left, err := f.store.ListEphemeralPastes()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) == 0 {
		t.Fatalf("ignored final FlushError and deleted content after %d failing flush(es)", w.failures)
	}
	retry := httptest.NewRecorder()
	f.router.ServeHTTP(retry, r.Clone(r.Context()))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after flush failure: %d %s", retry.Code, retry.Body.String())
	}
}
