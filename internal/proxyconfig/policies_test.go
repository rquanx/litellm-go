package proxyconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestPolicyResolutionAndEnforcement(t *testing.T) {
	var inferenceCalls int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/beta/litellm_basic_guardrail_api":
			io.WriteString(w, `{"action":"BLOCKED","blocked_reason":"Policy fixture"}`)
		case "/v1/chat/completions":
			inferenceCalls++
			io.WriteString(w, `{"id":"fixture","choices":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: openai/fixture
      api_base: %[1]q
litellm_settings:
  guardrails:
    - guardrail_name: deny
      litellm_params:
        guardrail: generic_guardrail_api
        mode: pre_call
        api_base: %[1]q
policies:
  base:
    guardrails: {add: [deny, removed]}
    condition: {model: ['demo(-v[0-9]+)?']}
  child:
    inherit: base
    guardrails: {remove: [removed]}
policy_attachments:
  - policy: child
    teams: ['prod-*']
    keys: ['service-*']
    tags: ['health-*']
  - policy: child
    models: [demo]
`, origin.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{
		Token: "master", Routes: config.Routes, Guardrails: config.Guardrails,
		PolicyGuardrails: config.Policy.GuardrailsFor(nil),
	})
	handler := config.Policy.Handler(server, "master")
	request := func(path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request("/policies/resolve", "master", `{"team_alias":"prod-east","key_alias":"service-east","tags":["health-data"],"model":"demo"}`)
	expected := policyResolution{
		Guardrails: []string{"deny"},
		Matches:    []policyMatchDetail{{Name: "child", Reason: "tag:health-data+team:prod-east+key:service-east", Guardrails: []string{"deny"}}},
	}
	var resolved policyResolution
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resolved) != nil || !reflect.DeepEqual(resolved, expected) {
		t.Fatalf("unexpected policy preview %d %s", w.Code, w.Body)
	}
	resolved = config.Policy.resolve(policyContext{Team: "prod-east", Key: "service-east", Model: "other", Tags: []string{"health-data"}}, true)
	if len(resolved.Matches) != 1 || len(resolved.Guardrails) != 0 {
		t.Fatalf("parent condition should suppress inherited guardrails: %+v", resolved)
	}
	w = request("/policies/resolve", "wrong", `{}`)
	if w.Code != 401 {
		t.Fatalf("policy preview leaked to a non-admin: %d", w.Code)
	}
	w = request("/v1/chat/completions", "master", `{"model":"demo","messages":[{"role":"user","content":"example"}]}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Policy fixture") || inferenceCalls != 0 {
		t.Fatalf("policy did not stop inference: %d %s", w.Code, w.Body)
	}
}

func TestPolicyRejectsInheritanceCycle(t *testing.T) {
	_, err := compilePolicies(map[string]policyRule{"a": {Inherit: "b"}, "b": {Inherit: "a"}}, nil)
	if err == nil {
		t.Fatal("cyclic policies were accepted")
	}
}
