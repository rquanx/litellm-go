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

func TestDeploymentDefaultsAndToolOrdering(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string
			Temperature float64
			MaxTokens int `json:"max_tokens"`
			TopP float64 `json:"top_p"`
			Tools []struct{ Function struct{ Name string } }
			Vendor map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Model != "fixture" || payload.Temperature != 0 || payload.MaxTokens != 500 || payload.TopP != 0.3 ||
			len(payload.Tools) != 2 || payload.Tools[0].Function.Name != "configured" || payload.Tools[1].Function.Name != "request" ||
			payload.Vendor["keep"] != true || payload.Vendor["remove"] != nil {
			t.Errorf("unexpected transformed request: %+v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer origin.Close()
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: demo
    litellm_params:
      model: openai_like/fixture
      api_base: ` + origin.URL + `
      temperature: 0.7
      max_tokens: 500
      tools: [{type: function, function: {name: configured}}]
      extra_body: {model: ignored, vendor: {keep: true, remove: true}}
      additional_drop_params: [extra_body.vendor.remove]
      top_p: 0.5
router_settings:
  default_litellm_params:
    top_p: 0.3
    tools: [{type: function, function: {name: router-default}}]
`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config.models[0])
	if err != nil {
		t.Fatal(err)
	}
	var restored deployment
	if err := decodeModelJSON(strings.NewReader(string(encoded)), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Params.MaxTokens == nil || *restored.Params.MaxTokens != 500 {
		t.Fatal("deployment defaults were lost in persisted model JSON")
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, RequestDefaults: config.RequestDefaults})
	input := `{"model":"demo","messages":[],"temperature":0,"tools":[{"type":"function","function":{"name":"request"}}]}`
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(input))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("request failed: %d %s", response.Code, response.Body)
	}
}
