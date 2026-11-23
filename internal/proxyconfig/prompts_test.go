package proxyconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestPostgresPromptVersionsEnvironmentsAndScope(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	id := "fixture-" + newEntityID()
	keys.state.Keys[keyDigest("scoped")] = virtualKey{Metadata: map[string]any{"prompts": []any{id + ".v1"}}}
	handler, err := WithPromptManagement(http.NotFoundHandler(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, key, body, want string, status int) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s %s got %d %s", method, path, w.Code, w.Body)
		}
	}
	body := `{"prompt_id":"` + id + `","litellm_params":{"prompt_id":"demo","prompt_integration":"dotprompt","prompt_data":{"demo":{"content":"test prompt"}}}}`
	path := "/prompts/" + id
	send("POST", "/prompts", "master", body, id+".v1", 200)
	send("PUT", path, "master", body, id+".v2", 200)
	send("PATCH", path+".v2", "master", `{"prompt_info":{"environment":"development","description":"updated"}}`, "updated", 200)
	send("GET", path+"/versions", "master", "", "updated", 200)
	send("GET", path+".v1/info", "scoped", "", "test prompt", 200)
	send("GET", path+".v2/info", "scoped", "", "Prompt not found", 400)

	// Render through the inference entry point and keep version authorization enforced.
	var delivered string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		delivered = string(raw)
		io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[]}`)
	}))
	defer upstream.Close()
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true, AuthorizeKey: keys.Authorize,
		ResolvePrompt: StoredPromptResolver(db, keys), Routes: []runtime.Route{{Alias: "demo", Candidates: []runtime.Candidate{{
			Provider: "openai", ModelID: "native", Endpoint: "chat-completions", APIBase: upstream.URL,
		}}}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","prompt_id":"`+id+`.v1","messages":[{"role":"user","content":"history"}]}`))
	r.Header.Set("Authorization", "Bearer scoped")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(delivered, "test prompt") || !strings.Contains(delivered, "history") || strings.Contains(delivered, "prompt_id") {
		t.Fatalf("stored prompt failed: %d %s", w.Code, delivered)
	}
	send("DELETE", path+"?environment=development", "master", "", "deleted successfully", 200)
}
