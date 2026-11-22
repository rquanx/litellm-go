package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPartnerOCRDownloadRejectsLoopback(t *testing.T) {
	var contacted atomic.Bool
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
	}))
	defer local.Close()
	input := map[string]any{"document": map[string]any{"type": "document_url", "document_url": local.URL}}
	if _, err := preparePartnerOCR(context.Background(), input, 1024); err == nil || contacted.Load() {
		t.Fatal("the OCR downloader connected to a loopback origin")
	}
}
