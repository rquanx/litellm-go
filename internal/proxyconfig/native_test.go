package proxyconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestNativeProxyEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/v1/messages", "/v1/messages/count_tokens":
			if r.Header.Get("X-Api-Key") != "upstream-key" || r.Header.Get("Anthropic-Version") != "2023-06-01" ||
				r.Header.Get("Authorization") != "" || r.Header.Get("Anthropic-Beta") != "fixture-beta" ||
				!strings.Contains(string(body), `"model":"claude-fixture"`) {
				t.Errorf("unexpected Anthropic request: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/messages/count_tokens" {
				if strings.Contains(string(body), `"stream"`) {
					t.Error("stream injected into token count request")
				}
				_, _ = io.WriteString(w, `{"input_tokens":3}`)
				return
			}
			_, _ = io.WriteString(w, `{"type":"message","model":"claude-fixture","content":[]}`)
		case "/v1/audio/speech":
			if r.Header.Get("Authorization") != "Bearer upstream-key" ||
				!strings.Contains(string(body), `"model":"tts-fixture"`) || strings.Contains(string(body), `"stream"`) {
				t.Errorf("unexpected speech request: %s", body)
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte{0xff, 0x01, 0x00})
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("TEST_NATIVE_BASE", upstream.URL+"/v1")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: messages
    litellm_params: {model: anthropic/claude-fixture, api_base: os.environ/TEST_NATIVE_BASE, api_key: upstream-key}
  - model_name: speech
    litellm_params: {model: openai/tts-fixture, api_base: os.environ/TEST_NATIVE_BASE, api_key: upstream-key}
    model_info: {supported_endpoints: [/v1/audio/speech]}
general_settings: {master_key: gateway-key}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, Token: config.MasterKey, AuthorizeInference: true})
	for _, test := range []struct {
		path, model, want, contentType string
	}{
		{"/v1/messages", "messages", `"model":"messages"`, "application/json"},
		{"/v1/messages/count_tokens", "messages", `{"input_tokens":3}`, "application/json"},
		{"/v1/audio/speech", "speech", "\xff\x01\x00", "audio/mpeg"},
	} {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"model":"`+test.model+`"}`))
		if test.model == "messages" {
			req.Header.Set("X-Api-Key", "gateway-key")
			req.Header.Set("Anthropic-Beta", "fixture-beta")
		} else {
			req.Header.Set("Authorization", "Bearer gateway-key")
		}
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), test.want) ||
			result.Header().Get("Content-Type") != test.contentType {
			t.Errorf("%s returned %d %s %q", test.path, result.Code, result.Header().Get("Content-Type"), result.Body.String())
		}
	}
}
