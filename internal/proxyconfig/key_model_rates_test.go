package proxyconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestModelRatesUseDeploymentDefaultsAndSharedTeamMetadata(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer origin.Close()
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: defaulted
    litellm_params: {model: openai/fixture-1, api_base: ` + origin.URL + `, default_api_key_rpm_limit: 2}
  - model_name: defaulted
    litellm_params: {model: openai/fixture-2, api_base: ` + origin.URL + `, default_api_key_rpm_limit: 1}
  - model_name: team-scoped
    litellm_params: {model: openai/fixture-1, api_base: ` + origin.URL + `}
`))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Teams["team"] = team{ID: "team", Models: []string{"team-scoped"},
		Metadata: map[string]any{"model_rpm_limit": map[string]any{"team-scoped": float64(1)}}}
	keys.state.Keys[keyDigest("first")] = virtualKey{TeamID: "team", Metadata: map[string]any{
		"model_rpm_limit": map[string]any{"team-scoped": float64(4)},
	}}
	keys.state.Keys[keyDigest("second")] = virtualKey{TeamID: "team"}
	keys.state.Keys[keyDigest("default")] = virtualKey{}
	keys.state.Keys[keyDigest("override")] = virtualKey{
		Metadata: map[string]any{"model_rpm_limit": map[string]any{"defaulted": float64(2)}},
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, Token: "master", AuthorizeInference: true,
		AuthorizeKey: keys.Authorize, CheckKeyBudget: keys.CheckKeyBudgetWithoutStore})
	infer := func(token, model string, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("model %s returned %d, expected %d: %s", model, w.Code, want, w.Body)
		}
	}
	infer("default", "defaulted", 200)
	infer("default", "defaulted", 429)
	infer("override", "defaulted", 200)
	infer("override", "defaulted", 200)
	infer("override", "defaulted", 429)
	infer("first", "team-scoped", 200)
	infer("second", "team-scoped", 429)
	if calls.Load() != 4 {
		t.Fatalf("provider received %d calls", calls.Load())
	}
}
