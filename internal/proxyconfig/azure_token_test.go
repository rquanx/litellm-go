package proxyconfig

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestAzureEntraTokenReachesInferenceAndResourceRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-entra" || r.Header.Get("api-key") != "" {
			t.Error("Azure request did not use its Entra bearer credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"choices":[]}`)
		} else {
			fmt.Fprint(w, `{"object":"list","data":[]}`)
		}
	}))
	defer upstream.Close()
	t.Setenv("TEST_AZURE_TOKEN", "fixture-entra")
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: azure/deployment
      api_base: %q
      api_version: "2025-01-01-preview"
      azure_ad_token: os.environ/TEST_AZURE_TOKEN
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, AuthorizeInference: true, Token: "master"})
	for _, fixture := range []struct{ method, path, body string }{
		{"POST", "/v1/chat/completions", `{"model":"demo","messages":[]}`},
		{"GET", "/v1/files?model=demo", ""},
	} {
		request := httptest.NewRequest(fixture.method, fixture.path, strings.NewReader(fixture.body))
		request.Header.Set("Authorization", "Bearer master")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("Azure %s returned %d: %s", fixture.path, response.Code, response.Body)
		}
	}
}
