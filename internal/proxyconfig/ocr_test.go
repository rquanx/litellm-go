package proxyconfig

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestMistralOCRRequestForms(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/ocr" || r.Header.Get("Authorization") != "Bearer fixture-key" ||
			input["model"] != "mistral-ocr-test" || input["unsupported"] != nil {
			t.Errorf("unexpected OCR request: %s %+v", r.URL.Path, input)
		}
		document, _ := input["document"].(map[string]any)
		if document["type"] != "document_url" ||
			(document["document_url"] != "https://example.invalid/file.pdf" && document["document_url"] != "data:application/pdf;base64,Zml4dHVyZQ==") {
			t.Errorf("unexpected OCR document: %+v", document)
		}
		pages, _ := input["pages"].([]any)
		if len(pages) != 1 || pages[0] != float64(0) {
			t.Errorf("unexpected OCR pages: %+v", input["pages"])
		}
		io.WriteString(w, `{"pages":[{"index":0,"markdown":"fixture"}],"model":"provider-result","usage_info":{"pages_processed":1}}`)
	}))
	defer upstream.Close()
	t.Setenv("TEST_OCR_BASE", upstream.URL+"/v1")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: ocr-fixture
    litellm_params: {model: mistral/mistral-ocr-test, api_base: os.environ/TEST_OCR_BASE, api_key: fixture-key}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, AuthorizeInference: true})
	multipart := "--fixture\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nocr-fixture\r\n" +
		"--fixture\r\nContent-Disposition: form-data; name=\"pages\"\r\n\r\n[0]\r\n" +
		"--fixture\r\nContent-Disposition: form-data; name=\"file\"; filename=\"fixture.pdf\"\r\nContent-Type: application/octet-stream\r\n\r\nfixture\r\n--fixture--\r\n"
	for _, fixture := range []struct {
		path, media, body string
		status           int
	}{
		{"/v1/ocr", "application/json", `{"model":"ocr-fixture","document":{"type":"document_url","document_url":"https://example.invalid/file.pdf"},"pages":[0],"unsupported":1}`, 200},
		{"/ocr", "multipart/form-data; boundary=fixture", multipart, 200},
		{"/v1/ocr", "application/json", `{"model":"ocr-fixture","document":{"type":"file","file":"fixture.pdf"}}`, 400},
	} {
		req := httptest.NewRequest("POST", fixture.path, strings.NewReader(fixture.body))
		req.Header.Set("Content-Type", fixture.media)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != fixture.status {
			t.Fatalf("OCR returned %d, want %d: %s", result.Code, fixture.status, result.Body)
		}
		if fixture.status == 200 && !strings.Contains(result.Body.String(), `"object":"ocr"`) {
			t.Fatal(result.Body.String())
		}
	}
}
