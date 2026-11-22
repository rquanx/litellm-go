package proxyconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestGenericAPISendsScopedInferenceMetadata(t *testing.T) {
	events := make(chan runtime.InferenceEvent, 2)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []runtime.InferenceEvent
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch) != 1 || r.Header.Get("X-Callback") != "fixture" {
			t.Errorf("invalid generic callback batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		events <- batch[0]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"response text"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()
	t.Setenv("GENERIC_LOGGER_HEADERS", "")
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params: {model: openai/fixture, api_base: %q}
litellm_settings:
  success_callback: [generic_api]
  generic_logger_endpoint: %q
  generic_logger_headers: {X-Callback: fixture}
`, upstream.URL, sink.URL)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, Callback: config.Callback.Start(ctx), LogMessages: config.LogMessages})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[{"content":"prompt text"}]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	select {
	case event := <-events:
		if event.Status != "success" || event.Total != 5 || event.ModelGroup != "demo" ||
			string(event.Messages) != "null" || string(event.Response) != "null" {
			t.Fatalf("unexpected callback event: %+v", event)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("local callback was not delivered")
	}
}
