package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestDynamicModelsArePublishedAndRestored(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: static-model
    litellm_params: {model: openai/static, api_base: http://example.invalid}
general_settings: {master_key: local-master}
litellm_settings:
  public_model_groups: [static-model]
  public_model_groups_links: {Static: "https://example.invalid/static"}
`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	buildHandler := func() http.Handler {
		server := runtime.NewServer(runtime.Config{Token: config.MasterKey, AuthorizeInference: true})
		handler, err := WithModelManagement(server, config, config.MasterKey, path)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	request := func(handler http.Handler, method, path, body, token string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, response.Code, status, response.Body.String())
		}
		return response.Body.String()
	}
	handler := buildHandler()
	request(handler, "GET", "/model/info", "", "wrong", 401)
	request(handler, "POST", "/model_group/make_public", `{"model_groups":[]}`, "wrong", 401)
	request(handler, "POST", "/model_group/make_public", `{"model_groups":[]}`, config.MasterKey, 200)
	request(handler, "POST", "/model_hub/update_useful_links", `{"useful_links":{"Managed":{"url":"https://example.invalid/managed","index":0}}}`, config.MasterKey, 200)
	model := `{"model_name":"dynamic-model","litellm_params":{"model":"openai/dynamic","api_base":"http://example.invalid","api_key":"fixture-credential"},"model_info":{"id":"managed-1"}}`
	request(handler, "POST", "/model/new", model, config.MasterKey, 200)
	request(handler, "POST", "/model/new", model, config.MasterKey, 400)
	list := request(handler, "GET", "/v1/models", "", config.MasterKey, 200)
	if !strings.Contains(list, "dynamic-model") || !strings.Contains(list, "static-model") {
		t.Fatal("model was not published alongside the static configuration")
	}
	handler = buildHandler()
	if groups := request(handler, "GET", "/public/model_hub", "", "", 200); strings.TrimSpace(groups) != "[]" {
		t.Fatalf("public group clearing was not persisted: %s", groups)
	}
	hub := request(handler, "GET", "/public/model_hub/info", "", "", 200)
	if !strings.Contains(hub, "example.invalid/managed") || strings.Contains(hub, "example.invalid/static") {
		t.Fatalf("public links were not restored: %s", hub)
	}
	info := request(handler, "GET", "/v1/model/info?litellm_model_id=managed-1", "", config.MasterKey, 200)
	if !strings.Contains(info, "dynamic-model") || strings.Contains(info, "fixture-credential") || strings.Contains(info, "example.invalid") {
		t.Fatal("model info did not restore or redacted fields were exposed")
	}
	request(handler, "PATCH", "/model/managed-1/update", `{"model_name":"renamed-model"}`, config.MasterKey, 200)
	list = request(handler, "GET", "/v1/models", "", config.MasterKey, 200)
	if strings.Contains(list, "dynamic-model") || !strings.Contains(list, "renamed-model") {
		t.Fatal("model update was not published atomically")
	}
	request(handler, "POST", "/model/delete", `{"id":"managed-1"}`, config.MasterKey, 200)
	list = request(buildHandler(), "GET", "/v1/models", "", config.MasterKey, 200)
	if strings.Contains(list, "renamed-model") || !strings.Contains(list, "static-model") {
		t.Fatal("model deletion did not survive restart")
	}
}
