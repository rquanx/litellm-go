package proxyconfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestConfiguredPassThroughWebSocket(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upstream/room" || r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Error("incorrect upstream WebSocket address or authorization")
		}
		socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"fixture"}})
		if err != nil {
			return
		}
		defer socket.CloseNow()
		kind, body, err := socket.Read(r.Context())
		if err == nil {
			_ = socket.Write(r.Context(), kind, body)
		}
	}))
	defer origin.Close()
	t.Setenv("TEST_PASS_SOCKET", "ws"+strings.TrimPrefix(origin.URL, "http")+"/upstream")
	cfg, err := Load(strings.NewReader(`
general_settings:
  master_key: fixture-master
  pass_through_endpoints:
    - path: /vendor-socket
      target: os.environ/TEST_PASS_SOCKET
      include_subpath: true
      forward_headers: true
      headers: {Authorization: Bearer fixture-provider}
`))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: cfg.MasterKey, PassThrough: cfg.PassThrough}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/vendor-socket/room", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer fixture-master"}}, Subprotocols: []string{"fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	if socket.Subprotocol() != "fixture" {
		t.Fatal("subprotocol was lost")
	}
	if err := socket.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	kind, body, err := socket.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(body) != "\x01\x02\x03" {
		t.Fatalf("WebSocket payload changed: %v %v %v", kind, body, err)
	}
}

func TestManagedPassThroughSurvivesRestart(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-fixture" {
			t.Error("persisted pass-through credential missing")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	const master = "fixture-master"
	state := filepath.Join(t.TempDir(), "state.json")
	open := func() http.Handler {
		t.Helper()
		handler, err := WithModelManagement(runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true}), Config{}, master, state)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := open()
	call := func(method, path, key, body string, status int) []byte {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		output := httptest.NewRecorder()
		handler.ServeHTTP(output, request)
		if output.Code != status || strings.Contains(output.Body.String(), "provider-fixture") {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, output.Code, status, output.Body.String())
		}
		return output.Body.Bytes()
	}
	body := `{"path":"/vendor","target":"` + origin.URL + `","headers":{"Authorization":"Bearer provider-fixture"}}`
	call("POST", passThroughManagementPath, "wrong", body, 401)
	var created struct {
		Endpoints []struct {
			ID string `json:"id"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(call("POST", passThroughManagementPath, master, body, 200), &created); err != nil ||
		len(created.Endpoints) != 1 || created.Endpoints[0].ID == "" {
		t.Fatalf("missing managed pass-through endpoint: %+v", created)
	}
	call("GET", "/vendor", master, "", 204)
	handler = open()
	call("GET", "/vendor", master, "", 204)
	id := created.Endpoints[0].ID
	call("POST", passThroughManagementPath+"/"+id, master, strings.Replace(body, "/vendor", "/renamed", 1), 200)
	call("GET", "/vendor", master, "", 404)
	call("GET", "/renamed", master, "", 204)
	call("DELETE", passThroughManagementPath+"?endpoint_id="+id, master, "", 200)
	handler = open()
	call("GET", "/renamed", master, "", 404)
}

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
