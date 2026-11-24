package proxyconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestGeminiNativeOperations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Goog-Api-Key") != "upstream-key" || strings.Contains(string(body), `"model"`) {
			t.Errorf("unexpected Gemini request: %s", body)
		}
		switch r.URL.Path {
		case "/v1beta/models/gemini-fixture:generateContent":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"usageMetadata":{"promptTokenCount":3,"totalTokenCount":4}}`)
		case "/v1beta/models/gemini-fixture:streamGenerateContent":
			if r.URL.Query().Get("alt") != "sse" {
				t.Error("upstream streaming mode is not SSE")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"candidates\":[]}\n\n")
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("TEST_GEMINI_BASE", upstream.URL+"/v1beta")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: summary
    litellm_params: {model: gemini/gemini-fixture, api_base: os.environ/TEST_GEMINI_BASE, api_key: upstream-key}
general_settings: {master_key: gateway-key}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, Token: config.MasterKey, AuthorizeInference: true})
	for _, test := range []struct {
		path, token, want string
		status            int
	}{
		{"/v1beta/models/summary:generateContent", "wrong-key", "unauthorized", 401},
		{"/v1beta/models/summary:generateContent", "gateway-key", `"totalTokenCount":4`, 200},
		{"/models/summary:streamGenerateContent", "gateway-key", "data: {\"candidates\":[]}\n\n", 200},
	} {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`))
		req.Header.Set("X-Goog-Api-Key", test.token)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != test.status || !strings.Contains(result.Body.String(), test.want) {
			t.Errorf("%s returned %d %q", test.path, result.Code, result.Body.String())
		}
	}
}

func TestGeminiResponsesStreamCompletesWithUsageOnEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"systemInstruction"`) || !strings.Contains(string(body), `"maxOutputTokens":20`) ||
			r.URL.Path != "/v1beta/models/gemini-fixture:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
			t.Errorf("unexpected Gemini bridge request: %s %s", r.URL, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"responseId":"gem-test","candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`,
			`{"responseId":"gem-test","candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":5}}`,
		} {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
	}))
	defer upstream.Close()
	server := runtime.NewServer(runtime.Config{Routes: []runtime.Route{{Alias: "demo", Candidates: []runtime.Candidate{
		{Endpoint: "gemini-generate", ModelID: "gemini-fixture", APIBase: upstream.URL},
	}}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","input":"hello","instructions":"reply briefly","max_output_tokens":20,"stream":true}`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, req)
	if result.Code != http.StatusOK || strings.Count(result.Body.String(), "event: response.completed\n") != 1 ||
		!strings.Contains(result.Body.String(), `"total_tokens":5`) {
		t.Fatalf("unexpected conversion: %d %s", result.Code, result.Body.String())
	}
}
