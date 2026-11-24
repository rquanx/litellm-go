package proxyconfig

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestScopedRouterSettingsRetryPrecedence(t *testing.T) {
	const master = "fixture-master"
	statePath := filepath.Join(t.TempDir(), "keys.json")
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		for _, name := range []string{"router_settings_override", "num_retries", "model_group_retry_policy", "routing_strategy"} {
			if _, present := input[name]; present {
				t.Errorf("%s reached upstream", name)
			}
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"fixture failure"}}`)
	}))
	defer upstream.Close()
	buildHandler := func() http.Handler {
		t.Helper()
		keys, err := LoadKeys(master, statePath)
		if err != nil {
			t.Fatal(err)
		}
		server := runtime.NewServer(runtime.Config{
			Token: master, AuthorizeInference: true, AuthorizeKey: keys.Authorize,
			CheckKeyBudget: keys.CheckKeyBudgetWithoutStore,
			Routes: []runtime.Route{{Alias: "demo", Candidates: []runtime.Candidate{
				{Endpoint: "chat-completions", ModelID: "demo", APIBase: upstream.URL},
			}}},
		})
		return keys.Handler(server)
	}
	handler := buildHandler()
	post := func(path, token, body string, status int) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s: got %d, want %d: %s", path, res.Code, status, res.Body)
		}
		return res.Body.Bytes()
	}
	post("/team/new", master, `{"team_id":"fixture","models":["demo"],"router_settings":{"num_retries":1,"routing_strategy":"least-busy"}}`, 200)
	var key struct{ Key string }
	if err := json.Unmarshal(post("/key/generate", master, `{"team_id":"fixture","models":["demo"]}`, 200), &key); err != nil || key.Key == "" {
		t.Fatalf("could not issue fixture key: %v", err)
	}
	handler = buildHandler()
	call := func(extra string, status int, want int32) {
		t.Helper()
		attempts.Store(0)
		post("/v1/chat/completions", key.Key, `{"model":"demo","messages":[]`+extra+`}`, status)
		if got := attempts.Load(); got != want {
			t.Fatalf("got %d attempts, want %d", got, want)
		}
	}
	call("", 503, 2)
	call(`,"router_settings_override":{"num_retries":3}`, 503, 2)
	call(`,"num_retries":0`, 503, 1)
	call(`,"num_retries":1.5`, 400, 0)
	call("", 503, 2)
	post("/key/update", master, `{"key":"`+key.Key+`","router_settings":{"model_group_retry_policy":{"demo":{"InternalServerErrorRetries":2}}}}`, 200)
	call("", 503, 3)
	call(`,"model_group_retry_policy":{}`, 503, 1)
	post("/key/update", master, `{"key":"`+key.Key+`","router_settings":null}`, 200)
	call("", 503, 2)
	post("/team/update", master, `{"team_id":"fixture","router_settings":{"fallbacks":["forbidden"]}}`, 200)
	call("", 400, 0)
}
