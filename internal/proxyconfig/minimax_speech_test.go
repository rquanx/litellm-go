package proxyconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestMiniMaxSpeechReturnsBinaryAudio(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if r.URL.Path != "/v1/t2a_v2" || r.Header.Get("Authorization") != "Bearer provider-key" || json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Errorf("unexpected MiniMax request: %s", r.URL)
		}
		voice, _ := input["voice_setting"].(map[string]any)
		audio, _ := input["audio_setting"].(map[string]any)
		if input["model"] != "speech-2.6-hd" || input["text"] != "fixture" || input["stream"] != false ||
			voice["voice_id"] != "male-qn-qingse" || voice["speed"] != float64(2) || audio["format"] != "wav" {
			t.Errorf("unexpected MiniMax payload: %v", input)
		}
		fmt.Fprint(w, `{"base_resp":{"status_code":0},"data":{"audio":"52494646"}}`)
	}))
	defer provider.Close()
	cfg, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: minimax/speech-2.6-hd
      api_base: %q
      api_key: provider-key
`, provider.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: cfg.Routes})
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"demo","input":"fixture","voice":"alloy","speed":3,"response_format":"wav"}`))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "RIFF" || w.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("unexpected MiniMax response: %d %s", w.Code, w.Body)
	}
}
