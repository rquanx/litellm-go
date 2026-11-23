package proxyconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestGenericGuardrailBlocksOrRewritesBeforeInference(t *testing.T) {
	inferenceCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/beta/litellm_basic_guardrail_api":
			if r.Header.Get("Authorization") != "Bearer guard-token" || !strings.Contains(string(raw), `"input_type":"request"`) {
				t.Errorf("unexpected guardrail request %s", raw)
			}
			if strings.Contains(string(raw), "blocked fixture") {
				io.WriteString(w, `{"action":"BLOCKED","blocked_reason":"Rejected fixture"}`)
			} else {
				io.WriteString(w, `{"action":"GUARDRAIL_INTERVENED","texts":["redacted"]}`)
			}
		case "/v1/chat/completions":
			inferenceCalls++
			if !strings.Contains(string(raw), "redacted") || strings.Contains(string(raw), "guardrails") {
				t.Errorf("unexpected inference request %s", raw)
			}
			io.WriteString(w, `{"id":"result","choices":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	cfg, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: openai/fixture
      api_base: %q
litellm_settings:
  guardrails:
    - guardrail_name: fixture
      litellm_params:
        guardrail: generic_guardrail_api
        mode: pre_call
        api_base: %q
        api_key: guard-token
        default_on: true
`, origin.URL, origin.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: cfg.Routes, Guardrails: cfg.Guardrails})
	for _, test := range []struct {
		content string
		status  int
	}{{"blocked fixture", 400}, {"allowed fixture", 200}} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[{"role":"user","content":"`+test.content+`"}]}`))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("pre-call check returned %d %s", w.Code, w.Body)
		}
	}
	if inferenceCalls != 1 {
		t.Fatalf("unexpected model request count %d", inferenceCalls)
	}
}

func TestGenericResponseGuardrailRunsBeforeWriting(t *testing.T) {
	modelRequests, guardRequests := 0, 0
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			modelRequests++
			io.WriteString(w, `{"choices":[{"message":{"content":"original","role":"assistant"}}],"id":"fixture"}`)
			return
		}
		guardRequests++
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"input_type":"response"`) || !strings.Contains(string(raw), `"texts":["original"]`) {
			t.Errorf("unexpected guardrail input %s", raw)
		}
		io.WriteString(w, `{"action":"GUARDRAIL_INTERVENED","texts":["revised"]}`)
	}))
	defer fixture.Close()
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true,
		Guardrails: []runtime.GenericGuardrail{{Name: "response-check", URL: fixture.URL + "/check", Default: true, Post: true}},
		Routes:     []runtime.Route{{Alias: "demo", Candidates: []runtime.Candidate{{Provider: "openai", ModelID: "fixture", APIBase: fixture.URL, Endpoint: "chat-completions"}}}},
	})
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"model":"demo","messages":[]}`, 200},
		{`{"model":"demo","messages":[],"stream":true}`, 400},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != test.status || strings.Contains(w.Body.String(), "original") ||
			(test.status == 200 && !strings.Contains(w.Body.String(), "revised")) {
			t.Fatalf("unexpected guardrail output %d %s", w.Code, w.Body)
		}
	}
	if modelRequests != 1 || guardRequests != 1 {
		t.Fatalf("unexpected request counts %d, %d", modelRequests, guardRequests)
	}
}

func TestAzureSafetyGuardsUseSeparateRequestAndResponseChecks(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/contentsafety/text:shieldPrompt":
			raw, _ := io.ReadAll(r.Body)
			if r.URL.Query().Get("api-version") != "2024-09-01" ||
				r.Header.Get("Ocp-Apim-Subscription-Key") != "fixture" ||
				r.Header.Get("Authorization") != "" ||
				!strings.Contains(string(raw), `"userPrompt":"recent\nquestion"`) ||
				strings.Contains(string(raw), "older") {
				t.Errorf("unexpected request shield payload: %s", raw)
			}
			io.WriteString(w, `{"userPromptAnalysis":{"attackDetected":false}}`)
		case "/contentsafety/text:analyze":
			raw, _ := io.ReadAll(r.Body)
			if r.URL.Query().Get("api-version") != "fixture-version" ||
				!strings.Contains(string(raw), `"text":"assistant reply"`) ||
				!strings.Contains(string(raw), `"categories":["Violence"]`) {
				t.Errorf("unexpected output moderation payload: %s", raw)
			}
			io.WriteString(w, `{"categoriesAnalysis":[{"category":"Violence","severity":4}]}`)
		case "/v1/chat/completions":
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"assistant reply"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params: {model: openai/fixture, api_base: %[1]q}
litellm_settings:
  guardrails:
    - guardrail_name: shield
      litellm_params: {guardrail: azure/prompt_shield, mode: pre_call, api_base: %[1]q, api_key: fixture, default_on: true}
    - guardrail_name: moderate
      litellm_params:
        guardrail: azure/text_moderations
        mode: post_call
        api_base: %[1]q
        api_key: fixture
        api_version: fixture-version
        severity_threshold_by_category: {Violence: 4}
        categories: [Violence]
        default_on: true
`, origin.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: config.Routes, Guardrails: config.Guardrails})
	body := `{"model":"demo","messages":[{"role":"user","content":"older"},{"role":"assistant","content":"previous"},{"role":"user","content":"recent"},{"role":"user","content":[{"type":"text","text":"question"}]}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Violence crossed severity 4") || strings.Contains(w.Body.String(), "assistant reply") {
		t.Fatalf("unexpected moderation response %d %s", w.Code, w.Body)
	}
}

func TestPostgresGuardrailRegistryPublishesAndRestoresRules(t *testing.T) {
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
	name := "fixture-" + newEntityID()
	server := runtime.NewServer(runtime.Config{})
	handler, err := WithGuardrailRegistry(http.NotFoundHandler(), server, keys, db, nil)
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
	body := `{"guardrail":{"guardrail_name":"` + name + `","litellm_params":{"guardrail":"generic_guardrail_api","mode":"pre_call","api_base":"http://127.0.0.1:12345","api_key":"secret-fixture"},"guardrail_info":{"description":"fixture"}}}`
	created := call(http.MethodPost, "/guardrails", body)
	var row struct {
		ID string `json:"guardrail_id"`
	}
	if created.Code != 200 || json.Unmarshal(created.Body.Bytes(), &row) != nil || row.ID == "" || strings.Contains(created.Body.String(), "secret-fixture") {
		t.Fatalf("unexpected guardrail create: %d %s", created.Code, created.Body)
	}
	handler, err = WithGuardrailRegistry(http.NotFoundHandler(), server, keys, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := call(http.MethodGet, "/guardrails/"+row.ID+"/info", "")
	if result.Code != 200 || !strings.Contains(result.Body.String(), name) || strings.Contains(result.Body.String(), "secret-fixture") {
		t.Fatalf("unexpected restored guardrail %d %s", result.Code, result.Body)
	}
	deleted := call(http.MethodDelete, "/guardrails/"+row.ID, "")
	if deleted.Code != 200 {
		t.Fatalf("delete returned %d %s", deleted.Code, deleted.Body)
	}
}
