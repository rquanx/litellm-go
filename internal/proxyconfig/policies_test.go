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
		ResolvePolicy: registry.Resolver(nil),
	})
	handler := registry.Handler(server, "master", nil)
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
	if w.Header().Get("X-LiteLLM-Applied-Policies") != "child" || w.Header().Get("X-LiteLLM-Policy-Sources") != "child=model:demo" {
		t.Fatalf("unexpected attribution headers: %v", w.Header())
	}
}

func TestPolicyRejectsInheritanceCycle(t *testing.T) {
	_, err := compilePolicies(map[string]policyRule{"a": {Inherit: "b"}, "b": {Inherit: "a"}}, nil)
	if err == nil {
		t.Fatal("cyclic policies were accepted")
	}
}

func TestPolicyAttachmentImpactCountsTeamAndTagMatchesOnce(t *testing.T) {
	keys := &KeyRegistry{state: accessState{
		Teams: map[string]team{"team": {Alias: "prod-east", Metadata: map[string]any{"tags": []string{"health-team"}}}},
		Keys: map[string]virtualKey{
			"match":   {Alias: "named", TeamID: "team", Metadata: map[string]any{"tags": []any{"health-service"}}},
			"unnamed": {TeamID: "team"},
			"other":   {Alias: "unrelated"},
		},
	}}
	registry, err := OpenPolicyRegistry(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := registry.Handler(http.NotFoundHandler(), "master", keys)
	request := httptest.NewRequest(http.MethodPost, "/policies/attachments/estimate-impact",
		strings.NewReader(`{"policy_name":"baseline","teams":["prod-*"],"tags":["health-*"]}`))
	request.Header.Set("Authorization", "Bearer master")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var impact attachmentImpact
	expected := attachmentImpact{KeyCount: 2, TeamCount: 1, UnnamedKeys: 1, KeySamples: []string{"named"}, TeamSamples: []string{"prod-east"}}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &impact) != nil || !reflect.DeepEqual(impact, expected) {
		t.Fatalf("unexpected impact %d %s", response.Code, response.Body)
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
		`CREATE TEMP TABLE ` + policyTable + policySchema,
		`CREATE TEMP TABLE ` + policyAttachmentTable + policyAttachmentSchema,
		`INSERT INTO ` + policyTable + ` (policy_id,policy_name,version_number,version_status,guardrails_add) VALUES
			('v1','production-rule',1,'production',ARRAY['first']),
			('v2','production-rule',2,'draft',ARRAY['next'])`,
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
	handler := registry.Handler(http.NotFoundHandler(), "master", nil)
	for path, expected := range map[string]string{
		"/policies/list?version_status=production":    `"total_count":1`,
		"/policies/name/production-rule/versions":     `"total_count":2`,
		"/policies/compare?version_a=v1&version_b=v2": `"field_diffs":{"guardrails_add":`,
		"/policies/v2/resolved-guardrails":            `"resolved_guardrails":["first"]`,
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("%s returned %d %s", path, w.Code, w.Body)
		}
	}
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

func TestPostgresPolicyVersionLifecycle(t *testing.T) {
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
	for _, statement := range []string{
		`CREATE TEMP TABLE ` + policyTable + policySchema,
		`CREATE TEMP TABLE ` + policyAttachmentTable + policyAttachmentSchema,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := OpenPolicyRegistry(nil, db)
	if err != nil {
		t.Fatal(err)
	}
	handler := registry.Handler(http.NotFoundHandler(), "master", nil)
	call := func(method, path, body string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	idFrom := func(response string) string {
		t.Helper()
		var record struct {
			ID string `json:"policy_id"`
		}
		if json.Unmarshal([]byte(response), &record) != nil || record.ID == "" {
			t.Fatalf("missing policy ID: %s", response)
		}
		return record.ID
	}
	first := idFrom(call(http.MethodPost, "/policies", `{"policy_name":"fixture","guardrails_add":["first"]}`, 200))
	call(http.MethodPut, "/policies/"+first, `{"guardrails_add":["unexpected"]}`, 400)
	call(http.MethodPost, "/policies/attachments", `{"policy_name":"fixture","scope":"*"}`, 200)
	second := idFrom(call(http.MethodPost, "/policies/name/fixture/versions", `{}`, 200))
	call(http.MethodPut, "/policies/"+second, `{"guardrails_add":["next"]}`, 200)
	call(http.MethodPut, "/policies/"+second+"/status", `{"version_status":"production"}`, 400)
	call(http.MethodPut, "/policies/"+second+"/status", `{"version_status":"published"}`, 200)
	call(http.MethodPut, "/policies/"+second+"/status", `{"version_status":"production"}`, 200)
	if got := registry.current.Load().resolve(policyContext{}, true).Guardrails; !reflect.DeepEqual(got, []string{"next"}) {
		t.Fatalf("production was not updated: %v", got)
	}
	if got := call(http.MethodGet, "/policies/"+first, "", 200); !strings.Contains(got, `"version_status":"published"`) {
		t.Fatalf("previous production was not demoted: %s", got)
	}
	call(http.MethodDelete, "/policies/"+first, "", 200)
	call(http.MethodDelete, "/policies/"+second, "", 500)
	call(http.MethodGet, "/policies/"+second, "", 200)
}

func TestPostgresPolicyAttachmentLifecycle(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE ` + policyAttachmentTable + policyAttachmentSchema); err != nil {
		t.Fatal(err)
	}
	var baseline policyRule
	baseline.Guardrails.Add = []string{"deny"}
	engine, err := compilePolicies(map[string]policyRule{"baseline": baseline}, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenPolicyRegistry(engine, db)
	if err != nil {
		t.Fatal(err)
	}
	handler := registry.Handler(http.NotFoundHandler(), "master", nil)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	const base = "/policies/attachments"
	result := call(http.MethodPost, base, `{"policy_name":"baseline","scope":"*"}`)
	var record policyAttachmentRecord
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &record) != nil || record.ID == "" || record.Teams == nil {
		t.Fatalf("create returned %d %s", result.Code, result.Body)
	}
	if guards := registry.current.Load().resolve(policyContext{}, true).Guardrails; !reflect.DeepEqual(guards, []string{"deny"}) {
		t.Fatalf("attachment was not applied: %v", guards)
	}
	for _, path := range []string{base + "/" + record.ID, base + "/list"} {
		if result = call(http.MethodGet, path, ""); result.Code != 200 || !strings.Contains(result.Body.String(), record.ID) {
			t.Fatalf("read returned %d %s", result.Code, result.Body)
		}
	}
	if result = call(http.MethodDelete, base+"/"+record.ID, ""); result.Code != 200 {
		t.Fatalf("delete returned %d %s", result.Code, result.Body)
	}
	if guards := registry.current.Load().resolve(policyContext{}, true).Guardrails; len(guards) != 0 {
		t.Fatalf("deleted attachment is still active: %v", guards)
	}
}
