package proxyconfig

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestAzureManagedIdentitySignsInferenceAndFiles(t *testing.T) {
	var exchanges atomic.Int32
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Identity-Header") != "fixture-identity-header" ||
			r.URL.Query().Get("client_id") != "fixture-client" ||
			r.URL.Query().Get("resource") != "https://cognitiveservices.azure.com" {
			t.Errorf("unexpected local managed identity request: %s", r.URL.Path)
		}
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"fixture-entra","token_type":"Bearer","expires_on":"%d","resource":"https://cognitiveservices.azure.com"}`, time.Now().Add(time.Hour).Unix())
	}))
	defer identity.Close()
	t.Setenv("IDENTITY_ENDPOINT", identity.URL)
	t.Setenv("IDENTITY_HEADER", "fixture-identity-header")
	t.Setenv("IDENTITY_SERVER_THUMBPRINT", "")
	t.Setenv("MSI_ENDPOINT", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-entra" || r.Header.Get("api-key") != "" {
			t.Errorf("unexpected Azure authorization for %s", r.URL.Path)
		}
		if r.URL.Path == "/openai/v1/embeddings" {
			io.WriteString(w, `{"object":"list","data":[],"model":"fixture"}`)
		} else if r.URL.Path == "/openai/v1/files/file-fixture" {
			io.WriteString(w, `{"object":"file","id":"file-fixture"}`)
		} else {
			t.Errorf("unexpected Azure request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: fixture
    litellm_params:
      model: azure/text-embedding-3-small
      api_base: %q
      client_id: fixture-client
      azure_credential: ManagedIdentityCredential
`, upstream.URL+"/openai/v1")))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: config.Routes})
	for _, item := range []struct{ method, path, body string }{
		{"POST", "/v1/embeddings", `{"model":"fixture","input":"example"}`},
		{"GET", "/v1/files/file-fixture?model=fixture", ""},
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(item.method, item.path, strings.NewReader(item.body)))
		if response.Code != http.StatusOK {
			t.Fatalf("unexpected Azure response %d %s", response.Code, response.Body)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatalf("received %d token exchanges", exchanges.Load())
	}
}
