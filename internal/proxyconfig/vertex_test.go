package proxyconfig

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestVertexConfiguredServiceAccountInfersProjectAndReusesToken(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(private)})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			requests.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("assertion") == "" ||
				r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Error("unexpected service account token request")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"fixture-sa-token","expires_in":3600,"token_type":"Bearer"}`)
		case "/v1/projects/fixture-project/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent":
			if r.Header.Get("Authorization") != "Bearer fixture-sa-token" {
				t.Error("the request lost the service account token")
			}
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"service account"}]}}]}`)
		default:
			t.Errorf("unexpected Vertex request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	credentials, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "fixture-project", "private_key": string(pemKey),
		"client_email": "fixture@example.invalid", "token_uri": upstream.URL + "/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINATO_VERTEX_CREDENTIAL_FIXTURE", string(credentials))
	t.Setenv("VERTEXAI_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("VERTEXAI_LOCATION", "")
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: vertex_ai/gemini-2.5-flash
      api_base: %q
      vertex_credentials: os.environ/MINATO_VERTEX_CREDENTIAL_FIXTURE
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	for range 2 {
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "service account") {
			t.Fatalf("unexpected Vertex response: %d %s", response.Code, response.Body)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("token requests = %d, want 1", requests.Load())
	}
}

func TestVertexGeminiUsesProjectEndpointAndBearer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/demo-project/locations/global/publishers/google/models/gemini-2.5-flash:generateContent" ||
			r.Header.Get("Authorization") != "Bearer fixture-oauth" || r.Header.Get("x-goog-api-key") != "" {
			t.Errorf("unexpected Vertex request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"vertex fixture"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer upstream.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: vertex_ai/gemini-2.5-flash
      api_base: %q
      api_key: fixture-oauth
      vertex_project: demo-project
      vertex_location: global
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "vertex fixture") {
		t.Fatalf("Vertex response: %d %s", response.Code, response.Body)
	}
}
