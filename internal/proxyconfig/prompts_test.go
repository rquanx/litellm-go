package proxyconfig

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestGenericStoredPromptFetchesAndRendersThroughInference(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/beta/litellm_prompt_management":
			if r.Method != http.MethodGet || r.URL.Query().Get("prompt_id") != "external" ||
				r.URL.Query().Get("label") != "public" || r.Header.Get("Authorization") != "Bearer manager-key" {
				t.Errorf("unexpected prompt manager request: %s %s", r.Method, r.URL)
			}
			io.WriteString(w, `{"prompt_template":[{"role":"system","content":"{greeting}, {{who}}"}],"prompt_template_model":"demo","prompt_template_optional_params":{"temperature":0.2}}`)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			for _, part := range []string{"Hello, fixture", "history", `"temperature":0.2`} {
				if !strings.Contains(string(body), part) {
					t.Errorf("inference lacked %q: %s", part, body)
				}
			}
			if strings.Contains(string(body), "prompt_id") || r.Header.Get("Authorization") == "Bearer manager-key" {
				t.Errorf("prompt fields crossed into inference")
			}
			io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true, ResolvePrompt: (Config{}).PromptResolver(db, keys),
		Routes: []runtime.Route{{Alias: "demo", Candidates: []runtime.Candidate{{Provider: "openai", ModelID: "native", Endpoint: "chat-completions", APIBase: origin.URL}}}}})
	handler, err := (Config{}).WithPromptManagement(server, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	id := "generic-" + newEntityID()
	defer db.Exec(`DELETE FROM `+promptTable+` WHERE prompt_id=$1`, id)
	body := fmt.Sprintf(`{"prompt_id":%q,"litellm_params":{"prompt_id":"external","prompt_integration":"generic_prompt_management","api_base":%q,"api_key":"manager-key","provider_specific_query_params":{"label":"public"}}}`, id, origin.URL)
	if w := call(http.MethodPost, "/prompts", body); w.Code != 200 || strings.Contains(w.Body.String(), "manager-key") {
		t.Fatalf("create returned %d %s", w.Code, w.Body)
	}
	body = fmt.Sprintf(`{"model":"generic_prompt/demo","prompt_id":%q,"prompt_variables":{"greeting":"Hello","who":"fixture"},"messages":[{"role":"user","content":"history"}]}`, id)
	if w := call(http.MethodPost, "/v1/chat/completions", body); w.Code != 200 {
		t.Fatalf("inference returned %d %s", w.Code, w.Body)
	}
}

func TestLangfusePromptUsesEscapedNameAndBasicAuth(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, key, ok := r.BasicAuth()
		if !ok || user != "fixture-public" || key != "fixture-secret" || r.URL.EscapedPath() != "/api/public/v2/prompts/team%2Fexample" ||
			r.URL.Query().Get("label") != "production" {
			t.Errorf("unexpected Langfuse request: %s", r.URL)
		}
		io.WriteString(w, `{"type":"text","prompt":"Hello {{ who }}","config":{"model":"demo","temperature":0.2}}`)
	}))
	defer origin.Close()
	fields := map[string]any{"model": "langfuse/ignored", "prompt_id": "demo", "prompt_label": "production",
		"prompt_variables": map[string]any{"who": "fixture"}}
	params := map[string]any{"api_base": origin.URL, "prompt_id": "team/example",
		"langfuse_public_key": "fixture-public", "langfuse_secret_key": "fixture-secret"}
	code, err := renderLangfusePrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), fields, promptRecord{Params: params})
	messages, _ := fields["messages"].([]any)
	if code != 0 || err != nil || fields["model"] != "demo" || fields["temperature"] != 0.2 ||
		len(messages) != 1 || messages[0].(map[string]any)["content"] != "Hello fixture" || fields["prompt_id"] != nil {
		t.Fatalf("unexpected Langfuse rendering: %d %v %+v", code, err, fields)
	}
}

func TestRepositoryPromptAdaptersResolveDotprompt(t *testing.T) {
	const source = "---\nmodel: fixture\n---\nSystem: Hello {{name}}"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/projects/org%2Frepo/repository/files/prompts%2Fwelcome.prompt/raw":
			http.NotFound(w, r)
		case "/projects/org%2Frepo/repository/files/prompts%2Fwelcome.prompt":
			if r.URL.Query().Get("ref") != "next" || r.Header.Get("Private-Token") != "fixture-token" {
				t.Errorf("unexpected GitLab request %s", r.URL)
			}
			fmt.Fprintf(w, `{"content":%q}`, base64.StdEncoding.EncodeToString([]byte(source)))
		case "/repositories/org/repo/src/next/prompts/welcome.prompt":
			user, key, ok := r.BasicAuth()
			if !ok || user != "fixture-user" || key != "fixture-token" {
				t.Error("Bitbucket basic authentication missing")
			}
			io.WriteString(w, source)
		default:
			t.Errorf("unexpected repository path %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	for _, provider := range []string{"gitlab", "bitbucket"} {
		t.Run(provider, func(t *testing.T) {
			settings := map[string]any{"base_url": origin.URL, "access_token": "fixture-token", "prompts_path": "prompts", "project": "org/repo",
				"workspace": "org", "repository": "repo", "username": "fixture-user"}
			if provider == "bitbucket" {
				settings["auth_method"] = "basic"
			}
			record := promptRecord{PromptID: "welcome", Params: map[string]any{"prompt_integration": provider, "git_ref": "next", provider + "_config": settings}}
			fields := map[string]any{"model": "placeholder", "prompt_variables": map[string]any{"name": "Ada"}}
			code, err := renderRepositoryPrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), fields, record)
			messages, _ := fields["messages"].([]any)
			if code != 0 || err != nil || fields["model"] != "fixture" || len(messages) != 1 ||
				messages[0].(map[string]any)["content"] != "Hello Ada" {
				t.Fatalf("unexpected repository rendering %d %v %+v", code, err, fields)
			}
		})
	}
}

func TestPhoenixPromptRendersVersionedMessageParts(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/prompt_versions/version-fixture" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("unexpected Phoenix request %s", r.URL)
		}
		io.WriteString(w, `{"data":{"model_name":"demo","invocation_parameters":{"openai":{"max_tokens":12}},"template":{"messages":[{"role":"system","content":[{"type":"text","text":"Hello {{ name }}"},{"type":"text","text":"today"}]}]}}}`)
	}))
	defer origin.Close()
	params := map[string]any{"api_base": origin.URL, "api_key": "fixture-key", "prompt_id": "version-fixture"}
	fields := map[string]any{"model": "arize/ignored", "prompt_variables": map[string]any{"name": "Ada"}}
	status, err := renderPhoenixPrompt(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), fields, promptRecord{Params: params})
	messages, _ := fields["messages"].([]any)
	if status != 0 || err != nil || fields["model"] != "demo" || fields["max_tokens"] != 12.0 || len(messages) != 1 ||
		messages[0].(map[string]any)["content"] != "Hello Ada today" {
		t.Fatalf("unexpected Phoenix rendering %d %v %+v", status, err, fields)
	}
}

func TestConfiguredPromptWorksWithoutPostgresAndRespectsScope(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: demo
    litellm_params: {model: openai/demo, api_base: "http://localhost:1"}
prompts:
  - prompt_id: greeting
    litellm_params:
      prompt_integration: dotprompt
      dotprompt_content: "System: Greeting {{name}}"
`))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Keys[keyDigest("allowed")] = virtualKey{Metadata: map[string]any{"prompts": []any{"greeting.v1"}}}
	keys.state.Keys[keyDigest("denied")] = virtualKey{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fields := map[string]any{"model": "demo", "prompt_id": "greeting", "prompt_variables": map[string]any{"name": "Ada"}}
	resolve := config.PromptResolver(nil, keys)
	r.Header.Set("Authorization", "Bearer denied")
	if status, _ := resolve(r, fields); status != http.StatusNotFound {
		t.Fatalf("unscoped key could resolve configured prompt: %d", status)
	}
	r.Header.Set("Authorization", "Bearer allowed")
	if status, err := resolve(r, fields); status != 0 || err != nil {
		t.Fatalf("scoped key could not resolve configured prompt: %d %v", status, err)
	}
	messages := fields["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "Greeting Ada" {
		t.Fatalf("unexpected configured prompt: %v", messages)
	}
	handler, err := config.WithPromptManagement(http.NotFoundHandler(), nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/prompts/greeting/info", nil)
	req.Header.Set("Authorization", "Bearer allowed")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"prompt_type":"config"`) {
		t.Fatalf("configured prompt info returned %d %s", w.Code, w.Body)
	}
}

func TestConfigLoadsLocalPromptRelativeToYAML(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "fixture.prompt")
	if err := os.WriteFile(file, []byte("System: Hello {{name}}"), 0600); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "proxy.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
model_list:
  - model_name: demo
    litellm_params: {model: openai/demo, api_base: "http://localhost:1"}
prompts:
  - prompt_id: greeting
    litellm_params:
      prompt_integration: dotprompt
      prompt_file: fixture.prompt
`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	params := config.prompts["greeting"].Params
	if params["dotprompt_content"] != "System: Hello {{name}}" || params["prompt_file"] != nil {
		t.Fatalf("configured prompt file was not loaded: %+v", params)
	}
}

func TestPostgresPromptVersionsEnvironmentsAndScope(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	id := "fixture-" + newEntityID()
	keys.state.Keys[keyDigest("scoped")] = virtualKey{Metadata: map[string]any{"prompts": []any{id + ".v1"}}}
	handler, err := (Config{}).WithPromptManagement(http.NotFoundHandler(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, key, body, want string, status int) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s %s got %d %s", method, path, w.Code, w.Body)
		}
	}
	body := `{"prompt_id":"` + id + `","litellm_params":{"prompt_id":"demo","prompt_integration":"dotprompt","prompt_data":{"demo":{"content":"test prompt"}}}}`
	path := "/prompts/" + id
	send("POST", "/prompts", "master", body, id+".v1", 200)
	send("PUT", path, "master", body, id+".v2", 200)
	send("PATCH", path+".v2", "master", `{"prompt_info":{"environment":"development","description":"updated"}}`, "updated", 200)
	send("GET", path+"/versions", "master", "", "updated", 200)
	send("GET", path+".v1/info", "scoped", "", "test prompt", 200)
	send("GET", path+".v2/info", "scoped", "", "Prompt not found", 400)

	// Render through the inference entry point and keep version authorization enforced.
	var delivered string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		delivered = string(raw)
		io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[]}`)
	}))
	defer upstream.Close()
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true, AuthorizeKey: keys.Authorize,
		ResolvePrompt: (Config{}).PromptResolver(db, keys), Routes: []runtime.Route{{Alias: "demo", Candidates: []runtime.Candidate{{
			Provider: "openai", ModelID: "native", Endpoint: "chat-completions", APIBase: upstream.URL,
		}}}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","prompt_id":"`+id+`.v1","messages":[{"role":"user","content":"history"}]}`))
	r.Header.Set("Authorization", "Bearer scoped")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(delivered, "test prompt") || !strings.Contains(delivered, "history") || strings.Contains(delivered, "prompt_id") {
		t.Fatalf("stored prompt failed: %d %s", w.Code, delivered)
	}
	send("DELETE", path+"?environment=development", "master", "", "deleted successfully", 200)
}
