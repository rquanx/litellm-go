package proxyconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestGenericAPISendsScopedInferenceMetadata(t *testing.T) {
	events := make(chan runtime.InferenceEvent, 2)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []struct {
			runtime.InferenceEvent
			StatusFields map[string]string `json:"status_fields"`
			CallID       string            `json:"litellm_call_id"`
			APISource    string            `json:"api_base"`
			FirstOutput  float64           `json:"completionStartTime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch) != 1 || r.Header.Get("X-Callback") != "fixture" {
			t.Errorf("invalid generic callback batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		event := batch[0]
		if event.CallID != event.ID || event.StatusFields["llm_api_status"] != "success" ||
			event.Provider != "openai" || event.APISource == "" || event.FirstOutput < event.StartTime {
			t.Errorf("unexpected standard callback fields: %+v", event)
		}
		events <- event.InferenceEvent
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

func TestGenericAPIRetriesTransientDelivery(t *testing.T) {
	var calls atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	callback := GenericCallback{endpoint: sink.URL, retries: 2}
	callback.send(context.Background(), sink.Client(), []runtime.InferenceEvent{{ID: "fixture"}})
	if calls.Load() != 2 {
		t.Fatalf("expected transient failure and one successful retry, got %d calls", calls.Load())
	}
}

func TestGenericAPIDeliveryFormats(t *testing.T) {
	t.Setenv("GENERIC_LOGGER_HEADERS", "X-Fixture=enabled,X-Token=test==")
	deliveries := make(chan string, 2)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture") != "enabled" || r.Header.Get("X-Token") != "test==" {
			t.Error("missing environment headers")
		}
		body, _ := io.ReadAll(r.Body)
		deliveries <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	for _, format := range []string{"ndjson", "single"} {
		callback, _, err := prepareCallback(callbackSettings{Endpoint: sink.URL, Format: format, Success: []string{"generic_api"}})
		if err != nil {
			t.Fatal(err)
		}
		callback[0].send(context.Background(), sink.Client(), []runtime.InferenceEvent{
			{ID: "first", Status: "success"}, {ID: "second", Status: "failure", HTTPStatus: 429},
		})
		var bodies []string
		for len(deliveries) > 0 {
			bodies = append(bodies, <-deliveries)
		}
		if format == "ndjson" && len(bodies) == 1 {
			bodies = strings.Split(bodies[0], "\n")
		}
		if len(bodies) != 2 || !strings.Contains(bodies[0], `"id":"first"`) ||
			!strings.Contains(bodies[1], `"error_code":"429"`) {
			t.Fatalf("invalid %s delivery: %v", format, bodies)
		}
	}
}

func TestGenericCallbackPresetsUseIndependentDestinations(t *testing.T) {
	generic := httptest.NewServer(http.NotFoundHandler())
	defer generic.Close()
	named := httptest.NewServer(http.NotFoundHandler())
	defer named.Close()
	t.Setenv("RUBRIK_WEBHOOK_URL", named.URL)
	t.Setenv("RUBRIK_API_KEY", "fixture")
	t.Setenv("GENERIC_LOGGER_HEADERS", "")
	callbacks, _, err := prepareCallback(callbackSettings{
		Endpoint: generic.URL, Callbacks: []string{"generic_api", "rubrik"}, Failure: []string{"rubrik"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callbacks) != 2 || callbacks[0].endpoint != generic.URL || callbacks[1].endpoint != named.URL {
		t.Fatalf("unexpected configured callback names %v", callbacks.Names())
	}
	if !callbacks[0].failure || !callbacks[1].success || callbacks[1].failure ||
		callbacks[1].headers["Authorization"] != "Bearer fixture" {
		t.Fatal("callback preset did not preserve its event and header settings")
	}
}

func TestLegacyGenericCallbackFormatsUsageAndNulls(t *testing.T) {
	payload := legacyCallbackPayload(runtime.InferenceEvent{
		ID: "call-id", ResponseID: "completion-id", CallType: "acompletion", Prompt: 3, Completion: 2, Total: 5,
		Messages: json.RawMessage(`[{"role":"user","content":"it's a test"}]`),
	})
	if payload["id"] != "completion-id" || payload["cache_hit"] != "False" || payload["response"] != "None" ||
		payload["usage"] != "{'prompt_tokens': 3, 'completion_tokens': 2, 'total_tokens': 5}" ||
		payload["messages"] != `[{'content': "it's a test", 'role': 'user'}]` {
		t.Fatalf("unexpected v1 callback %v", payload)
	}
}

func TestCallbackHealthUsesSyntheticEventAndRequiresMaster(t *testing.T) {
	received := make(chan string, 1)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer fixture.Close()
	callbacks := GenericCallbacks{&GenericCallback{name:"generic_api", endpoint:fixture.URL, format:"json_array", success:true}}
	server := runtime.NewServer(runtime.Config{Token:"master", AuthorizeInference:true, CallbackHealth:callbacks.Probe,
		AuthorizeKey:func(token string)([]string,bool){return nil, token=="virtual"}})
	for _, test := range []struct{token string; status int}{{"virtual",403},{"master",200}} {
		r := httptest.NewRequest(http.MethodGet,"/health/services?service=generic_api",nil)
		r.Header.Set("Authorization","Bearer "+test.token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w,r)
		if w.Code != test.status { t.Fatalf("unexpected probe response: %d %s",w.Code,w.Body) }
	}
	select {
	case body := <-received:
		if !strings.Contains(body,"litellm-mock-response-model") { t.Fatalf("unexpected callback payload %s",body) }
	default:
		t.Fatal("fixture callback did not receive the probe")
	}
}