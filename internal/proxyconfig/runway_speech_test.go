package proxyconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestRunwaySpeechPollsAndDownloadsWithoutProviderCredential(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/text_to_speech":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["promptText"] != "fixture" || body["model"] != "eleven_multilingual_v2" ||
				body["voice"].(map[string]any)["presetId"] != "Maya" || r.Header.Get("Authorization") != "Bearer fixture-key" {
				t.Errorf("unexpected speech task %v", body)
			}
			fmt.Fprint(w, `{"id":"task","status":"PENDING"}`)
		case "GET /v1/tasks/task":
			if r.Header.Get("X-Runway-Version") == "" || r.Header.Get("Authorization") != "Bearer fixture-key" {
				t.Error("poll is missing provider authentication")
			}
			fmt.Fprintf(w, `{"id":"task","status":"SUCCEEDED","output":["http://%s/audio"]}`, r.Host)
		case "GET /audio":
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Runway-Version") != "" {
				t.Error("download forwarded provider credentials")
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "audio fixture")
		default:
			t.Errorf("unexpected Runway request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	cfg, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: runwayml/eleven_multilingual_v2
      api_base: %q
      api_key: fixture-key
`, provider.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: cfg.Routes})
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"demo","input":"fixture","voice":"alloy"}`))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "audio fixture" || w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("unexpected Runway response: %d %s", w.Code, w.Body)
	}
}
