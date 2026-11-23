package proxyconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestConfiguredA2AAgentRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-upstream" || r.Header.Get("X-Trace-ID") != "trace-1" || r.Header.Get("X-Fixture") != "dynamic" {
			t.Errorf("unexpected upstream credentials or trace header")
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	t.Setenv("MINATO_TEST_AGENT_URL", upstream.URL)
	config, err := Load(strings.NewReader(`
agent_list:
  - agent_name: fixture
    agent_card_params: {name: Demo, url: os.environ/MINATO_TEST_AGENT_URL}
    static_headers: {Authorization: Bearer fixture-upstream}
    extra_headers: [X-Trace-ID]
    litellm_params: {require_trace_id_on_calls_to_agent: true}
  - agent_name: private-agent
    agent_card_params: {name: Private, url: os.environ/MINATO_TEST_AGENT_URL}
litellm_settings:
  public_agent_groups: [fixture]
`))
	if err != nil {
		t.Fatal(err)
	}
	var events []runtime.InferenceEvent
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "master", Agents: config.Agents,
		Callback:     func(event runtime.InferenceEvent) { events = append(events, event) },
		AuthorizeKey: func(token string) ([]string, bool) { return nil, token == "scoped" }})
	call := func(method, path, token, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Trace-ID", "trace-1")
		r.Header.Set("X-A2A-Fixture-X-Fixture", "dynamic")
		r.Header.Set("X-A2A-Fixture-Authorization", "Bearer ignored")
		r.Header.Set("X-LiteLLM-Trace-Id", "fixture-trace")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
		return w
	}
	hub := call("GET", "/public/agent_hub", "", "", 200)
	if strings.Contains(hub.Body.String(), upstream.URL) || !strings.Contains(hub.Body.String(), "Demo") || strings.Contains(hub.Body.String(), "Private") {
		t.Fatal(hub.Body)
	}
	card := call("GET", "/a2a/fixture/.well-known/agent-card.json", "scoped", "", 200)
	if strings.Contains(card.Body.String(), upstream.URL) || !strings.Contains(card.Body.String(), "/a2a/fixture") {
		t.Fatal(card.Body)
	}
	call("GET", "/v1/agents/private-agent", "scoped", "", 404)
	call("GET", "/v1/agents/private-agent", "master", "", 200)
	missing := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/fixture", nil)
	req.Header.Set("Authorization", "Bearer scoped")
	server.ServeHTTP(missing, req)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("trace requirement returned %d", missing.Code)
	}
	stream := call("POST", "/v1/a2a/fixture/message/send", "scoped", `{"jsonrpc":"2.0","id":1,"method":"message/stream","params":{}}`, 200)
	if !stream.Flushed || !strings.Contains(stream.Body.String(), `"result"`) {
		t.Fatal(stream.Body)
	}
	if len(events) != 1 || events[0].Provider != "a2a_agent" || events[0].Model != "a2a_agent/fixture" ||
		events[0].TraceID != "fixture-trace" || events[0].Status != "success" || events[0].KeyDigest == "" {
		t.Fatalf("unexpected A2A accounting event %+v", events)
	}
}

func TestManagedAgentVisibilitySurvivesRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "models.json")
	config := Config{MasterKey: "master"}
	open := func() http.Handler {
		t.Helper()
		server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "master"})
		handler, err := WithAgentManagement(server, server, config, nil, state)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := open()
	call := func(method, path, token, body string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || strings.Contains(w.Body.String(), "fixture-secret") {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	body := `{"agent_name":"demo","agent_card_params":{"name":"Demo","url":"http://localhost:1/a2a"},"static_headers":{"Authorization":"Bearer fixture-secret"}}`
	call("POST", "/v1/agents", "wrong", body, 401)
	var agent struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(call("POST", "/v1/agents", "master", body, 200)), &agent); err != nil || agent.ID == "" {
		t.Fatal("missing managed agent ID")
	}
	path := "/v1/agents/" + agent.ID
	call("POST", path+"/make_public", "master", "", 200)
	call("PATCH", path, "master", `{"agent_name":"renamed"}`, 200)
	handler = open()
	if hub := call("GET", "/public/agent_hub", "", "", 200); !strings.Contains(hub, "Demo") {
		t.Fatal(hub)
	}
	if got := call("GET", path, "master", "", 200); !strings.Contains(got, "renamed") {
		t.Fatal(got)
	}
	call("DELETE", path, "master", "", 200)
	handler = open()
	if hub := call("GET", "/public/agent_hub", "", "", 200); strings.TrimSpace(hub) != "[]" {
		t.Fatal(hub)
	}
}

func TestPostgresImportsLiteLLMAgents(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TEMP TABLE minato_proxy_state (name TEXT PRIMARY KEY, revision BIGINT NOT NULL, value JSONB NOT NULL);
		CREATE TEMP TABLE "LiteLLM_AgentsTable" (agent_id TEXT, agent_name TEXT, agent_card_params JSONB, static_headers JSONB, litellm_params JSONB);
		INSERT INTO "LiteLLM_AgentsTable" VALUES ('source-id','imported','{"url":"https://agent.example.test","name":"Import"}',
			'{"Authorization":"Bearer source-secret"}','{"custom_llm_provider":"a2a_agent"}');`)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{MasterKey: "fixture-master", publicAgents: []string{"imported"}}
	if count, err := ImportLiteLLMAgents(db, config); err != nil || count != 1 {
		t.Fatalf("agent import failed: %d %v", count, err)
	}
	store := postgresModels(db, config.MasterKey)
	store.name = "agents"
	var state managedAgents
	if err := store.load(&state); err != nil || len(state.Entries) != 1 || state.Entries[0].ID != "source-id" ||
		state.Entries[0].Headers["Authorization"] != "Bearer source-secret" || len(state.Public) != 1 || state.Public[0] != "imported" {
		t.Fatalf("could not restore imported agent: %v", err)
	}
}

func TestAgentGrantsAllowPrivateGroup(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Keys[keyDigest("scoped")] = virtualKey{ObjectPermission: &objectPermission{AgentGroups: []string{"group"}}}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "master", AuthorizeKey: keys.Authorize,
		AuthorizeAgent: keys.AuthorizeAgent, Agents: []runtime.Agent{
			{ID: "private", Name: "private", Card: map[string]any{"name": "private"}, AccessGroups: []string{"group"}},
			{ID: "public", Name: "public", Card: map[string]any{"name": "public"}, Public: true},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	request.Header.Set("Authorization", "Bearer scoped")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"agent_id":"private"`) ||
		strings.Contains(result.Body.String(), `"agent_id":"public"`) {
		t.Fatalf("wrong agent list %d %s", result.Code, result.Body)
	}
}

func TestA2ACompletionBridgeFormatsModelMessages(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]any
		_ = json.NewDecoder(r.Body).Decode(&fields)
		raw, _ := json.Marshal(fields)
		if r.URL.Path != "/v1/chat/completions" || !strings.Contains(string(raw), `"content":"Hello\nWorld"`) || fields["model"] != "fixture" ||
			r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("incorrect completion bridge request %s %s", r.URL.Path, raw)
		}
		if fields["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"reply \"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"fixture\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"reply fixture"}}]}`)
		}
	}))
	defer upstream.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
agent_list:
  - agent_name: bridge
    agent_card_params: {name: bridge, url: %q}
    litellm_params: {custom_llm_provider: openai, model: fixture, api_key: fixture-key}
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "master", Agents: config.Agents})
	for _, method := range []string{"message/send", "message/stream"} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"id","method":%q,"params":{"message":{"role":"user","parts":[{"kind":"text","text":"Hello"},{"kind":"text","text":"World"}]}}}`, method)
		r := httptest.NewRequest(http.MethodPost, "/a2a/bridge", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		result := httptest.NewRecorder()
		server.ServeHTTP(result, r)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "reply fixture") {
			t.Fatalf("bridge %s returned %d %s", method, result.Code, result.Body)
		}
		if method == "message/stream" && (!strings.Contains(result.Body.String(), `"kind":"task"`) ||
			!strings.Contains(result.Body.String(), `"state":"completed"`)) {
			t.Fatalf("bridge did not deliver streaming task lifecycle %s", result.Body)
		}
	}
}
