package proxyconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestConfiguredPassThroughKeepsProxyCredentialsLocal(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/service/a%2Fb" {
			t.Errorf("unexpected upstream address %s %s", r.Method, r.URL.EscapedPath())
		}
		query := r.URL.Query()
		if query.Get("level") != "caller" || query.Get("format") != "json" || query.Get("fixed") != "server" {
			t.Errorf("unexpected merged query %v", query)
		}
		if r.Header.Get("Authorization") != "Bearer provider-fixture" || r.Header.Get("X-Litellm-Api-Key") != "" ||
			r.Header.Get("X-Test") != "custom" || r.Header.Get("X-Drop") != "" {
			t.Error("unexpected forwarded headers")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "fixture request" {
			t.Fatal("upstream request body changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("data: fixture\n\n"))
	}))
	defer origin.Close()
	t.Setenv("TEST_PASS_TARGET", origin.URL+"/service?level=target&fixed=server")
	t.Setenv("TEST_PASS_KEY", "Bearer provider-fixture")
	cfg, err := Load(strings.NewReader(`
general_settings:
  master_key: fixture-master
  pass_through_endpoints:
    - path: /vendor
      target: os.environ/TEST_PASS_TARGET
      include_subpath: true
      forward_headers: true
      merge_query_params: true
      methods: [POST]
      cost_per_request: 0.02
      default_query_params: {level: default, format: json}
      headers: {Authorization: os.environ/TEST_PASS_KEY}
`))
	if err != nil {
		t.Fatal(err)
	}
	var event runtime.InferenceEvent
	server := runtime.NewServer(runtime.Config{Token: cfg.MasterKey, AuthorizeInference: true, PassThrough: cfg.PassThrough,
		Callback: func(result runtime.InferenceEvent) { event = result }})
	for _, token := range []string{"wrong", "fixture-master"} {
		r := httptest.NewRequest(http.MethodPost, "/vendor/a%2Fb?level=caller", strings.NewReader("fixture request"))
		r.Header.Set("X-Litellm-Api-Key", token)
		r.Header.Set("X-Pass-Authorization", "attempted-override")
		r.Header.Set("X-Pass-X-Test", "custom")
		r.Header.Set("Connection", "X-Drop")
		r.Header.Set("X-Drop", "only-on-client-connection")
		output := httptest.NewRecorder()
		server.ServeHTTP(output, r)
		if token == "wrong" {
			if output.Code != 401 {
				t.Fatalf("invalid virtual key returned %d", output.Code)
			}
		} else if output.Code != 201 || !output.Flushed || output.Body.String() != "data: fixture\n\n" {
			t.Fatalf("unexpected forwarded response %d %q", output.Code, output.Body.String())
		}
	}
	if event.Spend == nil || *event.Spend != 0.02 || !event.Stream || event.Status != "success" {
		t.Fatalf("unexpected pass-through spend event %+v", event)
	}
}
