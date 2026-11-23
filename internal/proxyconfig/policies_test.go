package proxyconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	registry, err := OpenPolicyRegistry(config.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{
		Token: "master", Routes: config.Routes, Guardrails: config.Guardrails,
		PolicyGuardrails: registry.GuardrailsFor(nil),
	})
	handler := registry.Handler(server, "master")
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

func TestPostgresPolicyRefreshKeepsLastValidSnapshot(t *testing.T) {
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
	statements := []string{
		`CREATE TEMP TABLE ` + policyTable + ` (
			policy_name text, version_number integer, version_status text, inherit text,
			guardrails_add text[], guardrails_remove text[], condition jsonb, pipeline jsonb
		)`,
		`CREATE TEMP TABLE ` + policyAttachmentTable + ` (
			attachment_id text, policy_name text, scope text, teams text[], keys text[], models text[], tags text[],
			created_at timestamp DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO ` + policyTable + ` VALUES
			('production-rule',1,'production',NULL,ARRAY['first'],'{}',NULL,NULL),
			('production-rule',2,'draft',NULL,ARRAY['next'],'{}',NULL,NULL)`,
		`INSERT INTO ` + policyAttachmentTable + ` (attachment_id,policy_name,scope)
			VALUES ('attachment','production-rule','*')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := OpenPolicyRegistry(nil, db)
	if err != nil {
		t.Fatal(err)
	}
	handler := registry.Handler(http.NotFoundHandler(), "master")
	resolve := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/policies/resolve"+query, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	check := func(query, guard string) {
		t.Helper()
		w := resolve(query)
		var response policyResolution
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || !reflect.DeepEqual(response.Guardrails, []string{guard}) {
			t.Fatalf("expected %q, got %d %s", guard, w.Code, w.Body)
		}
	}
	check("", "first")
	if _, err := db.Exec(`UPDATE ` + policyTable + ` SET version_status='production' WHERE version_number=2`); err != nil {
		t.Fatal(err)
	}
	check("", "first")
	check("?force_sync=true", "next")
	if _, err := db.Exec(`UPDATE ` + policyTable + ` SET inherit='missing' WHERE version_number=2`); err != nil {
		t.Fatal(err)
	}
	if w := resolve("?force_sync=true"); w.Code != 500 {
		t.Fatalf("expected invalid inheritance to fail refresh, got %d %s", w.Code, w.Body)
	}
	check("", "next")
}
