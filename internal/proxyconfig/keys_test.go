package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestVirtualKeyScopeAndRevocationSurviveRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "keys.json")
	const master = "fixture-master"
	buildHandler := func() http.Handler {
		keys, err := LoadKeys(master, state)
		if err != nil {
			t.Fatal(err)
		}
		candidate := runtime.Candidate{Endpoint: "chat-completions", ModelID: "example", APIBase: "http://example.invalid"}
		server := runtime.NewServer(runtime.Config{AuthorizeKey: keys.Authorize, AuthorizeInference: true, Token: master, Routes: []runtime.Route{
			{Alias: "permitted", Candidates: []runtime.Candidate{candidate}},
			{Alias: "restricted", Candidates: []runtime.Candidate{candidate}},
		}})
		return keys.Handler(server)
	}
	send := func(handler http.Handler, method, path, token, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, response.Code, status, response.Body.String())
		}
		return response.Body.String()
	}
	handler := buildHandler()
	response := send(handler, "POST", "/key/generate", master, `{"models":["permitted"],"duration":"1h"}`, 200)
	var generated struct{ Key string }
	if err := json.Unmarshal([]byte(response), &generated); err != nil || !strings.HasPrefix(generated.Key, "sk-") {
		t.Fatal("key generation did not return a credential")
	}
	if contents, err := os.ReadFile(state); err != nil || strings.Contains(string(contents), generated.Key) {
		t.Fatal("key state is missing or contains the plaintext key")
	}
	handler = buildHandler()
	models := send(handler, "GET", "/v1/models", generated.Key, "", 200)
	if !strings.Contains(models, "permitted") || strings.Contains(models, "restricted") {
		t.Fatal("virtual key model scope was not restored")
	}
	send(handler, "POST", "/v1/chat/completions", generated.Key, `{"model":"restricted","messages":[]}`, 403)
	send(handler, "POST", "/key/generate", generated.Key, `{}`, 401)
	send(handler, "POST", "/key/delete", master, `{"keys":["`+generated.Key+`"]}`, 200)
	send(buildHandler(), "GET", "/v1/models", generated.Key, "", 401)
}
