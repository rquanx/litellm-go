package proxyconfig

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestMistralOCRRequestForms(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/documentintelligence/documentModels/prebuilt-read:analyze" {
			var doc map[string]any
			if json.NewDecoder(r.Body).Decode(&doc) != nil || doc["base64Source"] != "Zml4dHVyZQ==" ||
				r.URL.Query().Get("pages") != "1,3" || r.Header.Get("Ocp-Apim-Subscription-Key") != "fixture-key" {
				t.Errorf("unexpected document intelligence request: %s %+v", r.URL, doc)
			}
			w.Header().Set("Operation-Location", "http://"+r.Host+"/result")
			w.WriteHeader(202)
			return
		}
		if r.URL.Path == "/result" {
			if r.Method != http.MethodGet || r.Header.Get("Ocp-Apim-Subscription-Key") != "fixture-key" {
				t.Error("Azure polling credential missing")
			}
			io.WriteString(w, `{"status":"succeeded","analyzeResult":{"pages":[{"pageNumber":3,"width":8.5,"height":11,"unit":"inch","lines":[{"content":"azure read"}]}]}}`)
			return
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		expectedPath := map[string]string{
			"mistral-ocr-test": "/v1/ocr", "azure-ocr-test": "/providers/mistral/azure/ocr",
			"vertex-ocr-test": "/v1/projects/fixture-project/locations/us-central1/publishers/mistralai/models/vertex-ocr-test:rawPredict",
		}
		model, _ := input["model"].(string)
		if model == "deepseek-ai/deepseek-ocr-maas" {
			messages, _ := input["messages"].([]any)
			if r.URL.Path != "/v1/projects/fixture-project/locations/us-central1/endpoints/openapi/chat/completions" ||
				r.Header.Get("Authorization") != "Bearer fixture-key" || len(messages) != 1 ||
				!strings.Contains(string(mustOCRJSON(messages)), "gs://fixture/sample.png") {
				t.Errorf("unexpected DeepSeek OCR request: %s %+v", r.URL.Path, input)
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"fixture markdown"}}],"usage":{"total_tokens":5}}`)
			return
		}
		if r.URL.Path != expectedPath[model] || r.Header.Get("Authorization") != "Bearer fixture-key" || input["unsupported"] != nil {
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
	t.Setenv("TEST_OCR_ROOT", upstream.URL)
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: ocr-fixture
    litellm_params: {model: mistral/mistral-ocr-test, api_base: os.environ/TEST_OCR_BASE, api_key: fixture-key}
  - model_name: ocr-azure
    litellm_params: {model: azure_ai/azure-ocr-test, api_base: os.environ/TEST_OCR_ROOT, api_key: fixture-key}
    model_info: {mode: ocr}
  - model_name: ocr-vertex
    litellm_params: {model: vertex_ai/ocr/vertex-ocr-test, api_base: os.environ/TEST_OCR_ROOT, api_key: fixture-key, vertex_project: fixture-project}
  - model_name: ocr-deepseek
    litellm_params: {model: vertex_ai/deepseek-ai/deepseek-ocr-maas, api_base: os.environ/TEST_OCR_ROOT, api_key: fixture-key, vertex_project: fixture-project}
  - model_name: ocr-doc-intelligence
    litellm_params: {model: azure_ai/doc-intelligence/prebuilt-read, api_base: os.environ/TEST_OCR_ROOT, api_key: fixture-key}
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
		{"/ocr", "multipart/form-data; boundary=fixture", strings.Replace(multipart, "ocr-fixture", "ocr-azure", 1), 200},
		{"/ocr", "multipart/form-data; boundary=fixture", strings.Replace(multipart, "ocr-fixture", "ocr-vertex", 1), 200},
		{"/ocr", "application/json", `{"model":"ocr-deepseek","document":{"type":"image_url","image_url":"gs://fixture/sample.png"}}`, 200},
		{"/ocr", "application/json", `{"model":"ocr-doc-intelligence","document":{"type":"document_url","document_url":"data:application/pdf;base64,Zml4dHVyZQ=="},"pages":[2,0,2]}`, 200},
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
		if strings.Contains(fixture.body, "ocr-deepseek") && !strings.Contains(result.Body.String(), `"markdown":"fixture markdown"`) {
			t.Fatal(result.Body.String())
		}
		if strings.Contains(fixture.body, "ocr-doc-intelligence") && (!strings.Contains(result.Body.String(), `"markdown":"azure read"`) ||
			!strings.Contains(result.Body.String(), `"width":816`) || !strings.Contains(result.Body.String(), `"index":2`)) {
			t.Fatal(result.Body.String())
		}
	}
}

func mustOCRJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
