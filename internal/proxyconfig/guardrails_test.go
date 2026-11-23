package proxyconfig

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
