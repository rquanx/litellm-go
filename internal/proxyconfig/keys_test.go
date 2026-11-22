package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	send(handler, "GET", "/models", "unknown", "", 401)
	send(handler, "GET", "/models", generated.Key, "", 200)
	send(handler, "GET", "/models/restricted", generated.Key, "", 404)
	send(handler, "GET", "/model_group/info?model_group=restricted", generated.Key, "", 404)
	send(handler, "GET", "/model_group/info", "unknown", "", 401)
	if groups := send(handler, "GET", "/model_group/info", generated.Key, "", 200); !strings.Contains(groups, "permitted") || strings.Contains(groups, "restricted") {
		t.Fatal("model group discovery did not apply the virtual key scope")
	}
	model := send(handler, "GET", "/v1/models/permitted", generated.Key, "", 200)
	if !strings.Contains(model, `"created":1677610602`) {
		t.Fatalf("individual model discovery response missing creation time: %s", model)
	}
	send(handler, "POST", "/chat/completions", generated.Key, `{"model":"restricted","messages":[]}`, 403)
	send(handler, "POST", "/engines/restricted/chat/completions", generated.Key, `{"model":"permitted","messages":[]}`, 403)
	azureRequest := httptest.NewRequest("POST", "/openai/deployments/restricted/chat/completions?api-version=2024-10-21",
		strings.NewReader(`{"model":"permitted","messages":[]}`))
	azureRequest.Header.Set("api-key", generated.Key)
	azureResponse := httptest.NewRecorder()
	handler.ServeHTTP(azureResponse, azureRequest)
	if azureResponse.Code != http.StatusForbidden {
		t.Fatalf("Azure API key must authenticate before checking the path model: %d", azureResponse.Code)
	}
	send(handler, "POST", "/key/generate", generated.Key, `{}`, 401)
	digest := keyDigest(generated.Key)
	reference := `{"key":"` + digest + `"}`
	send(handler, "POST", "/key/block", master, reference, 200)
	handler = buildHandler()
	send(handler, "GET", "/v1/models", generated.Key, "", 401)
	send(handler, "POST", "/key/unblock", master, reference, 200)
	send(handler, "POST", "/key/update", master, `{"key":"`+digest+`","key_alias":"Fixture","duration":"2w","models":["restricted"]}`, 200)
	models = send(handler, "GET", "/v1/models", generated.Key, "", 200)
	if strings.Contains(models, "permitted") || !strings.Contains(models, "restricted") {
		t.Fatal("key update did not change the inference model scope")
	}
	send(handler, "POST", "/key/update", master, `{"key":"`+digest+`","duration":null}`, 200)
	keys := send(handler, "GET", "/key/list?key_alias=fixture&return_full_object=true", master, "", 200)
	if !strings.Contains(keys, digest) || !strings.Contains(keys, "Fixture") || strings.Contains(keys, generated.Key) || strings.Contains(keys, "expires") {
		t.Fatalf("unexpected key list: %s", keys)
	}
	const rotated = "sk-rotated-fixture-key"
	send(handler, "POST", "/key/regenerate", master, `{"key":"`+digest+`","new_key":"`+rotated+`","grace_period":"1h"}`, 200)
	handler = buildHandler()
	send(handler, "GET", "/v1/models", generated.Key, "", 200)
	send(handler, "GET", "/v1/models", rotated, "", 200)
	send(handler, "POST", "/key/block", master, `{"key":"`+rotated+`"}`, 200)
	send(handler, "GET", "/v1/models", generated.Key, "", 401)
	send(handler, "POST", "/key/unblock", master, `{"key":"`+rotated+`"}`, 200)
	send(handler, "POST", "/key/delete", master, `{"key_aliases":["Fixture"]}`, 200)
	handler = buildHandler()
	send(handler, "GET", "/v1/models", generated.Key, "", 401)
	send(handler, "GET", "/v1/models", rotated, "", 401)
}

func TestKeyExpiryCalendarUnits(t *testing.T) {
	now := time.Date(2024, time.January, 31, 12, 0, 0, 0, time.UTC)
	for input, expected := range map[string]time.Time{
		"1mo": time.Date(2024, time.February, 29, 12, 0, 0, 0, time.UTC),
		"1w": now.Add(7 * 24 * time.Hour),
		"1d": now.Add(24 * time.Hour),
	} {
		expiry, err := keyExpiry(now, input)
		if err != nil || expiry == nil || !expiry.Equal(expected) {
			t.Errorf("%s: expiry=%v, err=%v, want %v", input, expiry, err, expected)
		}
	}
}
