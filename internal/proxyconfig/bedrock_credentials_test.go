package proxyconfig

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestBedrockUsesSelectedAWSSharedProfile(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(shared, []byte("[fixture]\naws_access_key_id = fixture-profile\naws_secret_access_key = fixture-secret\naws_session_token = fixture-session\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", shared)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-profile/") ||
			r.Header.Get("X-Amz-Security-Token") != "fixture-session" {
			t.Error("request did not use temporary profile credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output":{"message":{"role":"assistant","content":[{"text":"signed"}]}},"stopReason":"end_turn"}`)
	}))
	defer upstream.Close()
	proxy, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: bedrock/converse/fixture
      api_base: %q
      aws_region_name: us-east-1
      aws_profile_name: fixture
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: proxy.Routes})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "signed") {
		t.Fatalf("Bedrock proxy request failed: %d %s", response.Code, response.Body)
	}
}
