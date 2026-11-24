package proxyconfig

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestNamedProviderCredentialsRotateAndSurviveRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if key != "Bearer fixture-one" && key != "Bearer fixture-two" {
			t.Errorf("missing named credential")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[],"received":%q}`, key)
	}))
	defer upstream.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: fixed
    litellm_params: {model: openai/fixed, api_base: %q}
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	newHandler := func() http.Handler {
		server := runtime.NewServer(runtime.Config{Routes: config.Routes})
		handler, err := WithModelManagement(server, config, "master", path)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := newHandler()
	send := func(method, path, body string, expected int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer master")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != expected {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, result.Code, expected, result.Body)
		}
		return result.Body.String()
	}
	send("POST", "/credentials", `{"credential_name":"named","credential_values":{"api_key":"fixture-one"}}`, 200)
	send("POST", "/model/new", fmt.Sprintf(`{"model_name":"demo","model_info":{"id":"dynamic"},"litellm_params":{"model":"openai/fixture","api_base":%q,"litellm_credential_name":"named"}}`, upstream.URL), 200)
	send("POST", "/config/update", `{"router_settings":{"fallbacks":[{"fixed":["unknown"]}]}}`, 400)
	send("POST", "/config/update", `{"router_settings":{"fallbacks":[{"fixed":["demo"]}]}}`, 200)
	if body := send("POST", "/v1/chat/completions", `{"model":"demo","messages":[]}`, 200); !strings.Contains(body, "fixture-one") {
		t.Fatal("dynamic model did not use the named credential")
	}
	snapshot := send("GET", "/credentials/by_model/dynamic", "", 200)
	if strings.Contains(snapshot, "fixture-one") || !strings.Contains(snapshot, "********") {
		t.Fatalf("credential data was not masked: %s", snapshot)
	}
	send("PATCH", "/credentials/named", `{"credential_values":{"api_key":"fixture-two"}}`, 200)
	send("DELETE", "/credentials/named", "", 400)
	handler = newHandler()
	if settings := send("GET", "/router/settings", "", 200); !strings.Contains(settings, `"fallbacks":[{"fixed":["demo"]}]`) {
		t.Fatalf("router configuration did not survive restart: %s", settings)
	}
	if body := send("POST", "/v1/chat/completions", `{"model":"demo","messages":[]}`, 200); !strings.Contains(body, "fixture-two") {
		t.Fatal("credential rotation did not survive restart")
	}
	send("POST", "/model/delete", `{"id":"dynamic"}`, 400)
	send("POST", "/config/update", `{"router_settings":{"fallbacks":[]}}`, 200)
	send("POST", "/model/delete", `{"id":"dynamic"}`, 200)
	send("DELETE", "/credentials/named", "", 200)
}
