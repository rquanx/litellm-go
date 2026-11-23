package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestPostgresManagedMCPApprovalRefreshesOnReplica(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE minato_proxy_state (name TEXT PRIMARY KEY,revision BIGINT NOT NULL,value JSONB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	const master = "fixture-master"
	keys, err := LoadKeys(master, "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Teams["team"] = team{ID: "team"}
	keys.state.Keys[keyDigest("member")] = virtualKey{TeamID: "team"}
	open := func() *modelRegistry {
		server := runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true})
		registry, err := withModelManagement(server, Config{}, master, postgresModels(db, master), keys)
		if err != nil {
			t.Fatal(err)
		}
		return registry
	}
	primary, replica := open(), open()
	call := func(handler http.Handler, method, path, token, body string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s %s returned %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	raw := call(primary, http.MethodPost, mcpServerPath+"/register", "member",
		`{"server_name":"fixture","transport":"http","url":"http://127.0.0.1:1/mcp","available_on_public_internet":true}`, 201)
	var candidate mcpServerInput
	if json.Unmarshal([]byte(raw), &candidate) != nil || candidate.ServerID == "" {
		t.Fatalf("missing submission ID: %s", raw)
	}
	if err := replica.refreshManagedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	call(replica, http.MethodPut, mcpServerPath+"/"+candidate.ServerID+"/approve", master, "", 200)
	if before := call(primary, http.MethodGet, "/public/mcp_hub", "", "", 200); strings.TrimSpace(before) != "[]" {
		t.Fatalf("pending server published before refresh: %s", before)
	}
	if err := primary.refreshManagedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := call(primary, http.MethodGet, "/public/mcp_hub", "", "", 200); !strings.Contains(after, candidate.ServerID) {
		t.Fatalf("server not published after refresh: %s", after)
	}
	call(primary, http.MethodDelete, mcpServerPath+"/"+candidate.ServerID, master, "", 200)
}

func TestPostgresImportsLiteLLMMCPServers(t *testing.T) {
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
		CREATE TEMP TABLE "LiteLLM_MCPServerTable" (
			server_id TEXT, server_name TEXT, transport TEXT, url TEXT, approval_status TEXT,
			auth_type TEXT, credentials JSONB, static_headers JSONB, token_url TEXT
		);
		CREATE TEMP TABLE "LiteLLM_MCPToolsetTable" (
			toolset_id TEXT, toolset_name TEXT, description TEXT, tools JSONB, created_at TIMESTAMP, updated_at TIMESTAMP
		);
		INSERT INTO "LiteLLM_MCPToolsetTable" VALUES
			('fixture-set','collection',NULL,'[{"server_id":"fixture-id","tool_name":"echo"}]',now(),now());
		INSERT INTO "LiteLLM_MCPServerTable" VALUES
			('fixture-id','fixture','http','http://localhost:1/mcp','active','bearer_token',
			'{"auth_value":"fixture-import-token"}','{"X-Fixture":"value"}',NULL),
			('oauth-id','oauth-fixture','http','http://localhost:1/mcp','active','oauth2',
			'{"client_id":"fixture-client","client_secret":"fixture-oauth-secret"}',NULL,'http://localhost:1/token'),
			('pending','unreviewed','stdio',NULL,'pending',NULL,NULL,NULL,NULL),
			('review-id','queued','http','http://localhost:1/mcp','pending_review',NULL,NULL,NULL,NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{MasterKey: "fixture-master"}
	if count, err := ImportLiteLLMMCPServers(db, config); err != nil || count != 3 {
		t.Fatalf("import count %d: %v", count, err)
	}
	var state modelState
	if err := postgresModels(db, config.MasterKey).load(&state); err != nil {
		t.Fatal(err)
	}
	entry := state.MCPServers["fixture"]
	if entry.ServerID != "fixture-id" || entry.Headers["Authorization"] != "Bearer fixture-import-token" ||
		entry.Headers["X-Fixture"] != "value" || len(state.MCPServers) != 3 {
		t.Fatal("stored MCP server did not retain its ID and credentials")
	}
	if queued := state.MCPServers["queued"]; queued.ServerID != "review-id" || queued.ApprovalStatus != "pending_review" {
		t.Fatalf("pending MCP server did not retain its review status: %+v", queued)
	}
	oauth := state.MCPServers["oauth-fixture"]
	if oauth.AuthType != "oauth2" || oauth.ClientID != "fixture-client" || oauth.ClientSecret != "fixture-oauth-secret" || oauth.TokenURL != "http://localhost:1/token" {
		t.Fatal("stored MCP server did not retain its OAuth settings")
	}
	if len(state.MCPToolsets) != 1 || state.MCPToolsets[0].ID != "fixture-set" ||
		len(state.MCPToolsets[0].Tools) != 1 || state.MCPToolsets[0].Tools[0].ServerID != entry.ServerID {
		t.Fatal("stored toolset did not retain its ID and server reference")
	}
}

func TestManagedMCPServersPersistAndPublish(t *testing.T) {
	const master = "fixture-master"
	state := filepath.Join(t.TempDir(), "models.json")
	open := func() (http.Handler, *runtime.Server) {
		server := runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true})
		handler, err := WithModelManagement(server, Config{}, master, state)
		if err != nil {
			t.Fatal(err)
		}
		return handler, server
	}
	handler, _ := open()
	call := func(method, path, payload string, expected int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+master)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != expected || strings.Contains(result.Body.String(), "fixture-provider-secret") {
			t.Fatalf("%s %s got %d, want %d: %s", method, path, result.Code, expected, result.Body.String())
		}
		return result.Body.Bytes()
	}
	var created mcpServerInput
	raw := call("POST", mcpServerPath, `{"server_name":"fixture","transport":"http","url":"http://localhost:1/mcp","static_headers":{"Authorization":"Bearer fixture-provider-secret"}}`, 200)
	if json.Unmarshal(raw, &created) != nil || created.ServerID == "" {
		t.Fatalf("missing managed MCP server ID in %s", raw)
	}
	handler, _ = open()
	call("GET", mcpServerPath+"/"+created.ServerID, "", 200)
	if public := string(call("GET", "/public/mcp_hub", "", 200)); strings.TrimSpace(public) != "[]" {
		t.Fatalf("private MCP server was public: %s", public)
	}
	call("PUT", mcpServerPath, `{"server_id":"`+created.ServerID+`","transport":"http","url":"http://localhost:1/mcp?token=fixture-provider-secret","available_on_public_internet":true}`, 200)
	handler, _ = open()
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/public/mcp_hub", nil))
	var public []publicMCPServer
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &public) != nil || len(public) != 1 ||
		public[0].ID != created.ServerID || public[0].URL == nil || *public[0].URL != "http://localhost:1/mcp" ||
		strings.Contains(result.Body.String(), "fixture-provider-secret") {
		t.Fatalf("unexpected public MCP metadata: %s", result.Body)
	}
	call("PUT", mcpServerPath, `{"server_id":"`+created.ServerID+`","server_name":"renamed","transport":"http","url":"http://localhost:1/mcp"}`, 200)
	call("DELETE", mcpServerPath+"/"+created.ServerID, "", 200)
	handler, _ = open()
	call("GET", mcpServerPath+"/"+created.ServerID, "", 404)
}

func TestMCPServerSubmissionRequiresApprovalBeforePublication(t *testing.T) {
	const master = "fixture-master"
	keys, err := LoadKeys(master, "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Teams["team"] = team{ID: "team"}
	keys.state.Keys[keyDigest("member")] = virtualKey{TeamID: "team"}
	state := filepath.Join(t.TempDir(), "models.json")
	open := func() http.Handler {
		server := runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true})
		handler, err := withModelManagement(server, Config{}, master, chooseStateStore(state, nil, "models"), keys)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := open()
	call := func(method, path, key, body string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status || strings.Contains(w.Body.String(), "fixture-provider-secret") {
			t.Fatalf("%s %s returned %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	call(http.MethodPost, mcpServerPath+"/register", "member", `{"server_name":"stdio","transport":"stdio","command":"fixture"}`, 400)
	response := call(http.MethodPost, mcpServerPath+"/register", "member",
		`{"server_name":"pending","transport":"http","url":"http://127.0.0.1:1/mcp","available_on_public_internet":true,"static_headers":{"Authorization":"fixture-provider-secret"}}`, 201)
	var created mcpServerInput
	if json.Unmarshal([]byte(response), &created) != nil || created.ServerID == "" || created.ApprovalStatus != "pending_review" {
		t.Fatalf("unexpected submission %s", response)
	}
	handler = open()
	const hub = "/public/mcp_hub"
	if body := call(http.MethodGet, hub, "", "", 200); strings.TrimSpace(body) != "[]" {
		t.Fatalf("pending server was published: %s", body)
	}
	path := mcpServerPath + "/" + created.ServerID
	call(http.MethodPut, path+"/approve", "member", "", 401)
	call(http.MethodPut, path+"/approve", master, "", 200)
	if body := call(http.MethodGet, hub, "", "", 200); !strings.Contains(body, created.ServerID) {
		t.Fatalf("approved server was not published: %s", body)
	}
	call(http.MethodPut, path+"/reject", master, `{"review_notes":"fixture"}`, 200)
	if body := call(http.MethodGet, hub, "", "", 200); strings.TrimSpace(body) != "[]" {
		t.Fatalf("rejected server was still public: %s", body)
	}
}

func TestMCPRESTUsesScopedStreamableHTTPSessions(t *testing.T) {
	var closed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mcp-fixture" {
			t.Error("MCP server authorization was missing")
		}
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "test-session" {
				t.Error("MCP session ID missing on termination")
			}
			closed.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var message struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&message) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "test-session" {
			t.Error("MCP session ID was not propagated")
		}
		var result json.RawMessage
		switch message.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "test-session")
			result = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{"tools":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"echo","inputSchema":{"type":"object"}},{"name":"private","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			if message.Params["name"] != "echo" {
				t.Error("unexpected MCP tool call")
			}
			result = json.RawMessage(`{"content":[{"type":"text","text":"fixture"}],"isError":false}`)
		case "prompts/list":
			result = json.RawMessage(`{"prompts":[{"name":"sample","description":"fixture"}]}`)
		case "prompts/get":
			if message.Params["name"] != "sample" {
				t.Error("unexpected MCP prompt name")
			}
			result = json.RawMessage(`{"messages":[{"role":"user","content":{"type":"text","text":"fixture prompt"}}]}`)
		case "resources/list":
			result = json.RawMessage(`{"resources":[{"name":"manual","uri":"file:///fixture"}]}`)
		case "resources/read":
			if message.Params["uri"] != "file:///fixture" {
				t.Error("unexpected MCP resource URI")
			}
			result = json.RawMessage(`{"contents":[{"uri":"file:///fixture","text":"fixture resource"}]}`)
		default:
			t.Error("unexpected MCP request")
		}
		event, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message\ndata: " + string(event) + "\n\n"))
	}))
	defer upstream.Close()
	t.Setenv("TEST_MCP_URL", upstream.URL)
	cfg, err := Load(strings.NewReader(`
mcp_servers:
  fixture:
    url: os.environ/TEST_MCP_URL
    transport: http
    access_groups: [fixture-group]
    allowed_tools: [echo]
    allowed_params: {echo: [message]}
    static_headers: {Authorization: Bearer mcp-fixture}
`))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Teams["fixture-team"] = team{ID: "fixture-team", ObjectPermission: &objectPermission{
		MCPAccessGroups: []string{"fixture-group"}, MCPTools: map[string][]string{"fixture": {"echo"}},
	}}
	registry.state.Keys[keyDigest("fixture-key")] = virtualKey{TeamID: "fixture-team"}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "fixture-master", MCPServers: cfg.MCPServers,
		AuthorizeMCP: registry.AuthorizeMCP, AuthorizeMCPToolset: registry.AuthorizeMCPToolset})
	call := func(method, p, key, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, p, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		output := httptest.NewRecorder()
		server.ServeHTTP(output, request)
		if output.Code != status {
			t.Fatalf("%s returned %d: %s", p, output.Code, output.Body.String())
		}
		return output.Body.String()
	}
	if result := call("GET", "/mcp-rest/tools/list", "fixture-key", "", 200); !strings.Contains(result, "echo") || strings.Contains(result, "private") {
		t.Fatalf("unexpected tools %s", result)
	}
	if result := call("GET", "/v1/mcp/access_groups", "fixture-key", "", 200); !strings.Contains(result, "fixture-group") {
		t.Fatalf("unexpected MCP access groups %s", result)
	}
	call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"private"}`, 403)
	call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"echo","arguments":{"blocked":"fixture"}}`, 403)
	if result := call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"echo"}`, 200); !strings.Contains(result, "fixture") {
		t.Fatalf("unexpected tool response %s", result)
	}
	if result := call("POST", "/mcp", "fixture-key", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, 200); !strings.Contains(result, "protocolVersion") {
		t.Fatalf("unexpected MCP initialization response %s", result)
	}
	if result := call("POST", "/mcp", "fixture-key", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200); !strings.Contains(result, "fixture-echo") {
		t.Fatalf("unexpected MCP tool list %s", result)
	}
	if result := call("POST", "/mcp", "fixture-key", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fixture-echo"}}`, 200); !strings.Contains(result, "fixture") {
		t.Fatalf("unexpected MCP tool response %s", result)
	}
	for _, tc := range []struct {
		body, expected string
	}{
		{`{"jsonrpc":"2.0","id":4,"method":"prompts/list"}`, "fixture-sample"},
		{`{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"fixture-sample"}}`, "fixture prompt"},
		{`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`, "fixture-manual"},
		{`{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///fixture"}}`, "fixture resource"},
	} {
		if result := call("POST", "/fixture/mcp", "fixture-key", tc.body, 200); !strings.Contains(result, tc.expected) {
			t.Fatalf("unexpected MCP response %s", result)
		}
	}
	if closed.Load() != 8 {
		t.Fatalf("closed %d MCP sessions", closed.Load())
	}
	handler, err := WithModelManagement(server, cfg, "fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	setRequest := httptest.NewRequest(http.MethodPost, mcpToolsetPath, strings.NewReader(`{"toolset_name":"collection","tools":[{"server_id":"fixture","tool_name":"echo"}]}`))
	setRequest.Header.Set("Authorization", "Bearer fixture-master")
	setResponse := httptest.NewRecorder()
	handler.ServeHTTP(setResponse, setRequest)
	var toolset runtime.MCPToolset
	if setResponse.Code != 201 || json.Unmarshal(setResponse.Body.Bytes(), &toolset) != nil || toolset.ID == "" {
		t.Fatalf("could not create toolset: %d %s", setResponse.Code, setResponse.Body.String())
	}
	const setPath = "/toolset/collection/mcp"
	const listSet = `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`
	call(http.MethodPost, setPath, "fixture-key", listSet, 404)
	registry.state.Keys[keyDigest("fixture-key")] = virtualKey{ObjectPermission: &objectPermission{MCPToolsets: []string{toolset.ID}}}
	if got := call(http.MethodPost, setPath, "fixture-key", listSet, 200); !strings.Contains(got, "fixture-echo") {
		t.Fatalf("toolset grant did not expose the selected tool: %s", got)
	}
	if got := call(http.MethodPost, setPath, "fixture-key", `{"jsonrpc":"2.0","id":9,"method":"prompts/list"}`, 200); !strings.Contains(got, "-32601") {
		t.Fatalf("toolset permitted prompt enumeration: %s", got)
	}
	for _, p := range []string{"/mcp-rest/test/connection", "/mcp-rest/test/tools/list"} {
		request := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"url":"`+upstream.URL+`","transport":"http","static_headers":{"Authorization":"Bearer mcp-fixture"}}`))
		request.Header.Set("Authorization", "Bearer fixture-master")
		output := httptest.NewRecorder()
		handler.ServeHTTP(output, request)
		if output.Code != 200 || strings.Contains(output.Body.String(), "mcp-fixture") {
			t.Fatalf("unexpected probe response %d %s", output.Code, output.Body.String())
		}
	}
}
